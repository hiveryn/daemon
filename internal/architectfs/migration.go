package architectfs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/contextref"
	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

// Reference migration converts ticket references written for the removed
// absolute-path interpretation into the strict grammar. It is a two-step,
// reviewable operation run inside the daemon (so runtime-owned records change
// through Protection.Mutate, the same path every daemon mutation takes):
//
//  1. PlanReferenceMigration inventories every registered architect: each
//     ticket reference in backlog, progress and done that is not already
//     valid, with an inferred replacement where ownership is unambiguous, and
//     every absolute path in architect-owned Markdown (Overview, knowledge,
//     investigations, specs, workflows…) for manual follow-up. It changes
//     nothing.
//  2. ApplyReferenceMigration applies a reviewed plan: for each ticket it
//     rewrites only the reference scalars in place (other frontmatter, body
//     and updated_at untouched) when the file still has the plan's hash.
//     Every ticket reports its own result; rerunning a plan is idempotent.
//
// Nothing is inferred across architects and general Markdown prose is never
// turned into references.

// Migration dispositions of one reference.
const (
	MigrationKeep           = "keep"            // already valid; unchanged
	MigrationConvert        = "convert"         // to holds the inferred replacement
	MigrationAmbiguous      = "ambiguous"       // several owners possible; decide
	MigrationUnmapped       = "unmapped"        // no configured owner; decide
	MigrationCrossArchitect = "cross_architect" // another architect's workspace; decide
	MigrationInvalid        = "invalid"         // not a path and not valid; decide
)

// Ticket results of an apply.
const (
	MigrationApplied        = "applied"
	MigrationAlreadyApplied = "already_applied"
	MigrationNoChange       = "no_change"
	MigrationConflict       = "conflict"
	MigrationBlocked        = "blocked"
	MigrationRejected       = "rejected"
	MigrationFailed         = "failed"
)

type (
	MigrationRule      = domain.ReferenceMigrationRule
	MigrationReport    = domain.ReferenceMigrationReport
	ArchitectMigration = domain.ArchitectReferenceMigration
	TicketMigration    = domain.TicketReferenceMigration
	ReferenceMapping   = domain.ReferenceMapping
	DocumentFinding    = domain.DocumentReferenceFinding
)

const migrationGuidance = "Review every non-keep entry: set `to` (a valid reference) or `drop: true` for ambiguous, unmapped, cross_architect and invalid references; convert entries may be corrected the same way. Apply the reviewed file with `hiverynd references-migrate apply <file>`; tickets whose file changed since the plan are reported as conflicts and left untouched — plan again. Document findings are not changed by apply: replace each absolute path with the suggested reference (architect:<path> or <repoKey>:<path>), which workers read with readContext; do not use relative links."

// owner is a root a legacy absolute path may belong to.
type owner struct {
	reference string // architect or the repo key; "" for another architect
	root      string
	machine   string // "" local
	architect string // another architect's key
}

func owners(cfg config.Config, key string) (own, others []owner) {
	a := cfg.Architects[key]
	own = append(own, owner{reference: domain.ArchitectReferencePrefix, root: filepath.Clean(a.Path)})
	for repo, p := range a.Repos {
		machine := a.RepoMachines[repo]
		if machine == "" {
			p = filepath.Clean(p)
		} else {
			p = path.Clean(p)
		}
		own = append(own, owner{reference: repo, root: p, machine: machine})
	}
	for otherKey, other := range cfg.Architects {
		if otherKey != key {
			others = append(others, owner{root: filepath.Clean(other.Path), architect: otherKey})
		}
	}
	return own, others
}

func under(root, p string) (string, bool) {
	if p == root {
		return domain.ReferenceRootTarget, true
	}
	if strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") {
		return strings.TrimPrefix(p, strings.TrimSuffix(root, "/")+"/"), true
	}
	return "", false
}

// mapAbsolute infers the owner of a legacy absolute path. provenance is the
// machine the removed interpreter inspected it on (the ticket's primary repo
// machine), or nil when unknown. The deepest containing root wins; equal
// roots are ambiguous. A repo on a machine other than the provenance is never
// inferred — the path may have meant either machine. The architect
// workspace is the laptop's and accepted regardless, since that mismatch is
// the defect being corrected.
func mapAbsolute(own, others []owner, p string, provenance *string) ReferenceMapping {
	m := ReferenceMapping{From: p}
	p = path.Clean(filepath.ToSlash(p))
	type hit struct {
		o   owner
		rel string
	}
	var hits []hit
	for _, o := range own {
		if rel, ok := under(o.root, p); ok {
			hits = append(hits, hit{o, rel})
		}
	}
	if len(hits) == 0 {
		for _, o := range others {
			if _, ok := under(o.root, p); ok {
				m.Disposition = MigrationCrossArchitect
				m.Candidates = []string{"architect " + o.architect}
				m.Reason = "the path is in architect " + o.architect + "'s workspace; references cannot address another architect, so copy what is needed into this workspace or drop it"
				return m
			}
		}
		m.Disposition = MigrationUnmapped
		m.Reason = "no configured root (architect workspace or repo of this architect) contains the path"
		return m
	}
	sort.SliceStable(hits, func(i, j int) bool { return len(hits[i].o.root) > len(hits[j].o.root) })
	for _, h := range hits {
		m.Candidates = append(m.Candidates, h.o.reference+":"+h.rel)
	}
	best := hits[0]
	if len(hits) > 1 && len(hits[1].o.root) == len(best.o.root) {
		m.Disposition = MigrationAmbiguous
		m.Reason = "several configured roots are this same directory"
		return m
	}
	if best.o.reference != domain.ArchitectReferencePrefix && provenance != nil && *provenance != best.o.machine {
		m.Disposition = MigrationAmbiguous
		m.Reason = fmt.Sprintf("repo %q is on %s but the path was interpreted on %s", best.o.reference, machineLabel(best.o.machine), machineLabel(*provenance))
		return m
	}
	to := best.o.reference + ":" + best.rel
	if _, err := domain.ParseReference(to); err != nil {
		m.Disposition = MigrationUnmapped
		m.Reason = "the relative path cannot be written as a reference: " + err.Error()
		return m
	}
	m.Disposition, m.To = MigrationConvert, to
	if len(hits) > 1 {
		m.Reason = "deepest of the containing roots"
	}
	if best.o.reference == domain.ArchitectReferencePrefix && provenance != nil && *provenance != "" {
		m.Reason = strings.TrimPrefix(m.Reason+"; ", "; ") + "the path is in this architect's workspace on the Hiveryn host, though it was previously looked up on " + machineLabel(*provenance)
	}
	return m
}

func machineLabel(machine string) string {
	if machine == "" {
		return "this machine (local)"
	}
	return "machine " + machine
}

func fileHash(p string) (string, []byte, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), data, nil
}

// PlanReferenceMigration inventories the architects in keys (all registered
// architects when empty). It reads only.
func (s *TicketService) PlanReferenceMigration(ctx context.Context, cfg config.Config, keys []string, rules []MigrationRule) (MigrationReport, error) {
	keys, err := migrationKeys(cfg, keys)
	if err != nil {
		return MigrationReport{}, err
	}
	for i, rule := range rules {
		if !filepath.IsAbs(rule.FromPrefix) || path.Clean(rule.FromPrefix) != rule.FromPrefix || rule.FromPrefix == "/" {
			return MigrationReport{}, &domain.ValidationError{Field: fmt.Sprintf("rules[%d].from_prefix", i), Message: "must be a clean absolute directory other than /"}
		}
		owner, target, ok := strings.Cut(rule.ToPrefix, ":")
		if !ok || !domain.ValidRepoKey(owner) {
			return MigrationReport{}, &domain.ValidationError{Field: fmt.Sprintf("rules[%d].to_prefix", i), Message: "must be <owner>: or <owner>:<path>"}
		}
		if target != "" {
			if _, err := domain.ParseReference(rule.ToPrefix); err != nil {
				return MigrationReport{}, &domain.ValidationError{Field: fmt.Sprintf("rules[%d].to_prefix", i), Message: err.Error()}
			}
		}
		if rule.Architect != "" {
			if _, ok := cfg.Architects[rule.Architect]; !ok {
				return MigrationReport{}, &domain.NotFoundError{Resource: "architect", ID: rule.Architect}
			}
		}
	}
	report := MigrationReport{Mode: "plan", GeneratedAt: time.Now().UTC(), Architects: []ArchitectMigration{}, Guidance: migrationGuidance, Rules: rules}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return MigrationReport{}, err
		}
		report.Architects = append(report.Architects, planArchitect(cfg, key, rules))
	}
	report.Totals = migrationTotals(report)
	return report, nil
}

func migrationKeys(cfg config.Config, keys []string) ([]string, error) {
	if len(keys) == 0 {
		for key := range cfg.Architects {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		if _, ok := cfg.Architects[key]; !ok {
			return nil, &domain.NotFoundError{Resource: "architect", ID: key}
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func planArchitect(cfg config.Config, key string, rules []MigrationRule) ArchitectMigration {
	a := cfg.Architects[key]
	out := ArchitectMigration{Key: key, Path: a.Path, Tickets: []TicketMigration{}, Documents: []DocumentFinding{}}
	scope, err := contextref.ScopeFor(cfg, key)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	own, others := owners(cfg, key)
	entries, err := scanTickets(a.Path)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for _, status := range []domain.TicketStatus{domain.TicketStatusBacklog, domain.TicketStatusProgress, domain.TicketStatusDone} {
		for i := range entries[status] {
			entry := &entries[status][i]
			out.Scanned++
			if t, ok := planTicket(a.Path, scope, own, others, rulesFor(key, rules), entry); ok {
				out.Tickets = append(out.Tickets, t)
			}
		}
	}
	out.Documents = scanDocuments(a.Path, scope, own, others, rulesFor(key, rules))
	return out
}

// rulesFor keeps the rules that apply to architect key.
func rulesFor(key string, rules []MigrationRule) []MigrationRule {
	var out []MigrationRule
	for _, rule := range rules {
		if rule.Architect == "" || rule.Architect == key {
			out = append(out, rule)
		}
	}
	return out
}

// applyRule maps p by the longest matching rule whose target owner exists in
// scope. ok reports whether any rule matched; m is the resulting mapping.
func applyRule(scope contextref.Scope, rules []MigrationRule, p string) (ReferenceMapping, bool) {
	p = path.Clean(filepath.ToSlash(p))
	var best *MigrationRule
	var rel string
	for i := range rules {
		r := &rules[i]
		if tail, ok := under(r.FromPrefix, p); ok && (best == nil || len(r.FromPrefix) > len(best.FromPrefix)) {
			owner, _, _ := strings.Cut(r.ToPrefix, ":")
			if owner != domain.ArchitectReferencePrefix {
				if _, known := scope.Repos[owner]; !known && r.Architect == "" {
					continue // a global rule for a repo this architect does not have
				}
			}
			best, rel = r, tail
		}
	}
	if best == nil {
		return ReferenceMapping{}, false
	}
	m := ReferenceMapping{From: p, Reason: "rule " + best.FromPrefix + " → " + best.ToPrefix}
	var to string
	switch owner, target, _ := strings.Cut(best.ToPrefix, ":"); {
	case target == "" && rel == domain.ReferenceRootTarget:
		to = owner + ":."
	case target == "":
		to = owner + ":" + rel
	case rel == domain.ReferenceRootTarget:
		to = best.ToPrefix
	default:
		to = best.ToPrefix + "/" + rel
	}
	parsed, err := domain.ParseReference(to)
	if err == nil {
		_, err = scope.Resolve(parsed)
	}
	if err != nil {
		m.Disposition = MigrationUnmapped
		m.Reason += ": " + err.Error()
		return m, true
	}
	m.Disposition, m.To = MigrationConvert, to
	return m, true
}

func planTicket(root string, scope contextref.Scope, own, others []owner, rules []MigrationRule, entry *ticketEntry) (TicketMigration, bool) {
	rel := filepath.ToSlash(filepath.Join(ticketsDirName, string(entry.status), entry.id, ticketFileName))
	t := TicketMigration{ID: entry.id, Status: string(entry.status), File: rel, References: []ReferenceMapping{}}
	hash, _, err := fileHash(filepath.Join(root, rel))
	if err != nil {
		t.Error = err.Error()
		return t, true
	}
	t.SHA256 = hash
	for _, w := range entry.warnings {
		if w.Code == "INVALID_TICKET" {
			t.Error = "the ticket cannot be read, so its references are not migrated: " + w.Message
			return t, true
		}
	}
	var provenance *string
	if m, ok := scope.Repos[entry.metadata.Repo]; ok {
		provenance = &m.Machine
	}
	changed := false
	seen := map[string]bool{}
	for i, value := range entry.metadata.References {
		var m ReferenceMapping
		parsed, perr := domain.ParseReference(value)
		switch {
		case perr == nil && parsed.Kind != domain.ReferenceKindTicket:
			if _, rerr := scope.Resolve(parsed); rerr != nil {
				m = ReferenceMapping{From: value, Disposition: MigrationInvalid, Reason: rerr.Error()}
			} else {
				m = ReferenceMapping{From: value, Disposition: MigrationKeep}
			}
		case perr == nil:
			m = ReferenceMapping{From: value, Disposition: MigrationKeep}
		case filepath.IsAbs(value):
			if ruled, ok := applyRule(scope, rules, value); ok {
				m = ruled
				m.From = value
				break
			}
			m = mapAbsolute(own, others, value, provenance)
			if provenance == nil && m.Disposition == MigrationConvert && m.To != "" && !strings.HasPrefix(m.To, domain.ArchitectReferencePrefix+":") {
				m.Reason = strings.TrimPrefix(m.Reason+"; ", "; ") + "the ticket's primary repo is not configured, so the machine the path was meant for is unknown"
				m.Disposition, m.Candidates, m.To = MigrationAmbiguous, []string{m.To}, ""
			}
		default:
			m = ReferenceMapping{From: value, Disposition: MigrationInvalid, Reason: perr.Error()}
		}
		if i < len(entry.metadata.ReferenceLines) {
			m.Line = entry.metadata.ReferenceLines[i] + 1
		}
		final := m.From
		if m.To != "" {
			final = m.To
		}
		if seen[final] {
			m.Drop, m.To = true, ""
			m.Reason = strings.TrimPrefix(m.Reason+"; ", "; ") + "duplicate of an earlier reference"
			if m.Disposition == MigrationKeep {
				m.Disposition = MigrationInvalid
			}
		}
		seen[final] = true
		if m.Disposition != MigrationKeep {
			changed = true
		}
		t.References = append(t.References, m)
	}
	return t, changed
}

// Absolute paths in prose and links: the usual top-level directories of macOS
// and Linux homes and data volumes, or ~/.
// Relative Markdown link targets: anything but a URL, mailto, fragment,
// absolute path or an existing <key>:<path> reference.
var markdownLink = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)

var absolutePathInText = regexp.MustCompile("(?:^|[\\s(<\\[\"'`=])((?:/(?:Users|home|Volumes|mnt|srv|opt|data|private|tmp|var)/|~/)[^\\s)>\\]\"'`]+)")

func scanDocuments(root string, scope contextref.Scope, own, others []owner, rules []MigrationRule) []DocumentFinding {
	findings := []DocumentFinding{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			findings = append(findings, DocumentFinding{File: p, Disposition: MigrationUnmapped, Reason: "cannot scan: " + err.Error()})
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "runs":
				return filepath.SkipDir
			}
			if rel == ticketsDirName || rel == "architect-sessions" {
				return filepath.SkipDir // runtime records; historical conclusions are preserved
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".md") || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			findings = append(findings, DocumentFinding{File: rel, Disposition: MigrationUnmapped, Reason: "cannot read: " + err.Error()})
			return nil
		}
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for n := 1; scanner.Scan(); n++ {
			for _, match := range absolutePathInText.FindAllStringSubmatch(scanner.Text(), -1) {
				found := strings.TrimRight(match[1], ".,;:")
				finding := DocumentFinding{File: rel, Line: n, Path: found}
				if strings.HasPrefix(found, "~/") {
					finding.Disposition = MigrationUnmapped
					finding.Reason = "home-relative paths are machine-specific; write a reference to the owning root"
				} else {
					local := ""
					m, ok := applyRule(scope, rules, found)
					if !ok {
						m = mapAbsolute(own, others, found, &local)
					}
					finding.Disposition, finding.Reason = m.Disposition, m.Reason
					finding.Suggestion = m.To
				}
				findings = append(findings, finding)
			}
			for _, match := range markdownLink.FindAllStringSubmatch(scanner.Text(), -1) {
				if finding, ok := relativeLinkFinding(root, rel, n, match[1]); ok {
					findings = append(findings, finding)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			findings = append(findings, DocumentFinding{File: rel, Disposition: MigrationUnmapped, Reason: "scan stopped: " + err.Error()})
		}
		return nil
	})
	return findings
}

// relativeLinkFinding reports a relative Markdown link: references are the
// only way documents point to context, so each becomes architect:<path>
// resolved against the linking file's directory, or unmapped when it leaves
// the workspace.
func relativeLinkFinding(root, file string, line int, target string) (DocumentFinding, bool) {
	if target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "~") {
		return DocumentFinding{}, false
	}
	if owner, _, ok := strings.Cut(target, ":"); ok && !strings.Contains(owner, "/") {
		return DocumentFinding{}, false // a URL scheme, mailto or an existing reference
	}
	clean, _, _ := strings.Cut(target, "#")
	clean, _, _ = strings.Cut(clean, "?")
	if clean == "" {
		return DocumentFinding{}, false
	}
	finding := DocumentFinding{File: file, Line: line, Path: target}
	resolved := path.Clean(path.Join(path.Dir(file), clean))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		finding.Disposition = MigrationUnmapped
		finding.Reason = "relative link leaves the architect workspace; write the owning <repoKey>:<path> reference"
		return finding, true
	}
	to := domain.ArchitectReferencePrefix + ":" + resolved
	if _, err := domain.ParseReference(to); err != nil {
		finding.Disposition, finding.Reason = MigrationUnmapped, "relative link cannot be written as a reference: "+err.Error()
		return finding, true
	}
	finding.Disposition, finding.Suggestion = MigrationConvert, to
	finding.Reason = "relative link; documents point to context with references"
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(resolved))); err != nil {
		finding.Reason += " (the target does not exist)"
	}
	return finding, true
}

func migrationTotals(report MigrationReport) map[string]int {
	totals := map[string]int{}
	for _, a := range report.Architects {
		totals["architects"]++
		totals["tickets_scanned"] += a.Scanned
		if a.Error != "" {
			totals["architect_errors"]++
		}
		for _, t := range a.Tickets {
			totals["tickets_listed"]++
			if t.Error != "" {
				totals["ticket_errors"]++
			}
			if t.Result != "" {
				totals["result_"+t.Result]++
			}
			for _, r := range t.References {
				totals["references_"+r.Disposition]++
			}
		}
		totals["document_findings"] += len(a.Documents)
	}
	return totals
}

// ApplyReferenceMigration applies a reviewed plan. Each ticket is applied on
// its own, through Protection.Mutate so runtime-owned records stay accepted,
// and only while its file still has the plan's hash. Ticket.Result reports
// what happened; the returned report is the plan annotated with results.
func (s *TicketService) ApplyReferenceMigration(ctx context.Context, cfg config.Config, plan MigrationReport) (MigrationReport, error) {
	report := plan
	report.Mode, report.GeneratedAt, report.Guidance = "apply", time.Now().UTC(), ""
	report.Architects = make([]ArchitectMigration, len(plan.Architects))
	for i, a := range plan.Architects {
		a.Tickets = append([]TicketMigration(nil), a.Tickets...)
		report.Architects[i] = a
		registered, ok := cfg.Architects[a.Key]
		if !ok || filepath.Clean(registered.Path) != filepath.Clean(a.Path) {
			report.Architects[i].Error = fmt.Sprintf("architect %q is not registered at %s; plan again", a.Key, a.Path)
			continue
		}
		scope, err := contextref.ScopeFor(cfg, a.Key)
		if err != nil {
			report.Architects[i].Error = err.Error()
			continue
		}
		for j := range report.Architects[i].Tickets {
			if err := ctx.Err(); err != nil {
				return MigrationReport{}, err
			}
			t := &report.Architects[i].Tickets[j]
			t.Result, t.Message = s.applyTicket(ctx, a.Path, scope, *t)
		}
	}
	report.Totals = migrationTotals(report)
	return report, nil
}

func (s *TicketService) applyTicket(ctx context.Context, root string, scope contextref.Scope, t TicketMigration) (string, string) {
	if t.Error != "" {
		return MigrationNoChange, "not applied: " + t.Error
	}
	want, changed, err := migratedReferences(scope, t)
	if err != nil {
		return MigrationRejected, err.Error()
	}
	if !changed {
		return MigrationNoChange, "no decided change in the plan"
	}
	expected := filepath.ToSlash(filepath.Join(ticketsDirName, t.Status, t.ID, ticketFileName))
	if t.File != expected {
		return MigrationRejected, "the plan's file " + t.File + " does not match its ticket " + expected
	}
	file := filepath.Join(root, filepath.FromSlash(t.File))
	result, message := "", ""
	err = s.mutate(ctx, root, t.ID, func() error {
		hash, data, err := fileHash(file)
		if errors.Is(err, os.ErrNotExist) {
			result, message = MigrationConflict, "the ticket is no longer at "+t.File+" (moved or removed since the plan); plan again"
			return nil
		}
		if err != nil {
			return err
		}
		current, err := parseMarkdownDocument(string(data))
		if err != nil {
			return err
		}
		meta, err := decodeTicketMetadata(current.Metadata)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(nonNilStrings(meta.References), want) {
			result, message = MigrationAlreadyApplied, "the references already match the plan"
			return nil
		}
		if hash != t.SHA256 {
			result, message = MigrationConflict, "the file changed since the plan (sha256 "+hash+", plan "+t.SHA256+"); plan again"
			return nil
		}
		rewritten, err := rewriteReferences(string(data), t.References)
		if err != nil {
			result, message = MigrationRejected, err.Error()
			return nil
		}
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		tmp := file + ".hiveryn-migrate"
		if err := os.WriteFile(tmp, []byte(rewritten), info.Mode().Perm()); err != nil {
			return fmt.Errorf("write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, file); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace %s: %w", file, err)
		}
		result = MigrationApplied
		return nil
	})
	var conflict *domain.ConflictError
	switch {
	case errors.As(err, &conflict):
		return MigrationBlocked, "the workspace has an unresolved protected-record conflict, so no record was changed: " + conflict.Message
	case err != nil:
		return MigrationFailed, err.Error()
	}
	return result, message
}

// migratedReferences is the references list the plan asks for, after
// checking every decided value.
func migratedReferences(scope contextref.Scope, t TicketMigration) ([]string, bool, error) {
	out := []string{}
	changed := false
	seen := map[string]bool{}
	for _, m := range t.References {
		if m.Drop {
			changed = true
			continue
		}
		value := m.From
		if m.To != "" && m.To != m.From {
			parsed, err := domain.ParseReference(m.To)
			if err == nil && parsed.Kind != domain.ReferenceKindTicket {
				_, err = scope.Resolve(parsed)
			}
			if err != nil {
				return nil, false, fmt.Errorf("decision for %q: %w", m.From, err)
			}
			value, changed = m.To, true
		}
		if seen[value] {
			return nil, false, fmt.Errorf("the decided references repeat %q; drop one", value)
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, changed, nil
}

// rewriteReferences replaces the references sequence items of a ticket.md in
// place, line by line, so the rest of the frontmatter keeps its exact text.
// Dropped items lose their line; an emptied block sequence becomes `[]`. A
// layout it cannot rewrite safely (comments on items, multi-line scalars,
// multi-line flow sequences) is refused rather than reformatted. The result
// is re-parsed and must differ from the original only in references.
func rewriteReferences(content string, mappings []ReferenceMapping) (string, error) {
	if !strings.HasPrefix(content, "---\n") {
		return "", errors.New("ticket.md has no frontmatter")
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return "", errors.New("ticket.md frontmatter is not closed")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content[4:4+end]), &doc); err != nil {
		return "", fmt.Errorf("parse frontmatter: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return "", errors.New("ticket.md frontmatter is empty")
	}
	meta := doc.Content[0]
	var key, seq *yaml.Node
	for i := 0; i+1 < len(meta.Content); i += 2 {
		if meta.Content[i].Value == "references" {
			key, seq = meta.Content[i], meta.Content[i+1]
		}
	}
	if seq == nil || seq.Kind != yaml.SequenceNode || len(seq.Content) != len(mappings) {
		return "", errors.New("the ticket's references no longer match the plan; plan again")
	}
	values := []string{}
	for i, item := range seq.Content {
		if item.Kind != yaml.ScalarNode || item.Value != mappings[i].From {
			return "", errors.New("the ticket's references no longer match the plan; plan again")
		}
		if item.LineComment != "" || item.HeadComment != "" || item.FootComment != "" || item.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return "", fmt.Errorf("reference %q has a comment or multi-line form; convert this ticket by hand-reviewed edit", item.Value)
		}
		if !mappings[i].Drop {
			v := item.Value
			if mappings[i].To != "" {
				v = mappings[i].To
			}
			values = append(values, v)
		}
	}
	lines := strings.SplitAfter(content, "\n")
	// Frontmatter YAML line n is file line n+1 (after the opening "---").
	idx := func(yamlLine int) int { return yamlLine }
	encode := func(v string) (string, error) {
		out, err := yaml.Marshal(v)
		if err != nil {
			return "", err
		}
		s := strings.TrimSuffix(string(out), "\n")
		if strings.Contains(s, "\n") {
			return "", fmt.Errorf("reference %q cannot be written on one line", v)
		}
		return s, nil
	}
	if seq.Style&yaml.FlowStyle != 0 {
		line := idx(key.Line)
		for _, item := range seq.Content {
			if item.Line != key.Line {
				return "", errors.New("multi-line flow sequence of references; convert this ticket by hand-reviewed edit")
			}
		}
		encoded := make([]string, 0, len(values))
		for _, v := range values {
			e, err := encode(v)
			if err != nil {
				return "", err
			}
			encoded = append(encoded, e)
		}
		l := lines[line]
		open, closeAt := strings.Index(l, "["), strings.LastIndex(l, "]")
		if open < 0 || closeAt < open {
			return "", errors.New("unexpected flow sequence layout; convert this ticket by hand-reviewed edit")
		}
		lines[line] = l[:open] + "[" + strings.Join(encoded, ", ") + "]" + l[closeAt+1:]
	} else {
		remove := map[int]bool{}
		for i, item := range seq.Content {
			line := idx(item.Line)
			for _, other := range seq.Content[:i] {
				if other.Line == item.Line {
					return "", errors.New("several references on one line; convert this ticket by hand-reviewed edit")
				}
			}
			if mappings[i].Drop {
				remove[line] = true
				continue
			}
			if mappings[i].To == "" || mappings[i].To == mappings[i].From {
				continue
			}
			e, err := encode(mappings[i].To)
			if err != nil {
				return "", err
			}
			l := lines[line]
			eol := ""
			if strings.HasSuffix(l, "\n") {
				eol = "\n"
			}
			lines[line] = l[:item.Column-1] + e + eol
		}
		if len(values) == 0 {
			l := lines[idx(key.Line)]
			lines[idx(key.Line)] = l[:key.Column-1] + "references: []\n"
		}
		kept := lines[:0]
		for i, l := range lines {
			if !remove[i] {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	out := strings.Join(lines, "")

	// Safety net: only references changed.
	before, err := parseMarkdownDocument(content)
	if err != nil {
		return "", err
	}
	after, err := parseMarkdownDocument(out)
	if err != nil {
		return "", fmt.Errorf("rewritten frontmatter does not parse: %w", err)
	}
	var b, a map[string]any
	if err := before.Metadata.Decode(&b); err != nil {
		return "", err
	}
	if err := after.Metadata.Decode(&a); err != nil {
		return "", err
	}
	gotRefs := a["references"]
	delete(a, "references")
	delete(b, "references")
	if !reflect.DeepEqual(a, b) || before.Body != after.Body {
		return "", errors.New("rewriting the references would change other frontmatter or the body; convert this ticket by hand-reviewed edit")
	}
	var got []string
	if list, ok := gotRefs.([]any); ok {
		for _, v := range list {
			s, _ := v.(string)
			got = append(got, s)
		}
	}
	if !reflect.DeepEqual(nonNilStrings(got), values) {
		return "", fmt.Errorf("rewritten references %v do not match the plan %v", got, values)
	}
	return out, nil
}

// Migrator binds the migration to the current configuration.
func (s *TicketService) Migrator(source config.Source) domain.ReferenceMigrator {
	return referenceMigrator{s: s, source: source}
}

type referenceMigrator struct {
	s      *TicketService
	source config.Source
}

func (m referenceMigrator) PlanReferenceMigration(ctx context.Context, architects []string, rules []MigrationRule) (MigrationReport, error) {
	cfg, err := m.source.Current()
	if err != nil {
		return MigrationReport{}, fmt.Errorf("read configuration: %w", err)
	}
	return m.s.PlanReferenceMigration(ctx, cfg, architects, rules)
}

func (m referenceMigrator) ApplyReferenceMigration(ctx context.Context, plan MigrationReport) (MigrationReport, error) {
	cfg, err := m.source.Current()
	if err != nil {
		return MigrationReport{}, fmt.Errorf("read configuration: %w", err)
	}
	if plan.Mode != "plan" {
		return MigrationReport{}, &domain.ValidationError{Field: "mode", Message: "apply takes a plan (mode \"plan\") produced by `hiverynd references-migrate plan`"}
	}
	return m.s.ApplyReferenceMigration(ctx, cfg, plan)
}
