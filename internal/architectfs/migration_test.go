package architectfs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// migrationReplica is a disposable two-architect installation: architect "a"
// with a local repo "core" and a repo "infra" on machine bk, and architect
// "b". Progress and done records are protected by accepted fingerprints.
type migrationReplica struct {
	a, b, core string
	cfg        config.Config
	service    *TicketService
	protection *Protection
}

func newMigrationReplica(t *testing.T) *migrationReplica {
	t.Helper()
	r := &migrationReplica{a: t.TempDir(), b: t.TempDir(), core: t.TempDir()}
	r.cfg = config.Config{
		Machines: map[string]config.MachineConfig{"bk": {SSH: "bk-alias"}},
		Architects: map[string]config.ArchitectConfig{
			"a": {Path: r.a, Repos: map[string]string{"core": r.core, "infra": "/srv/infra"}, RepoMachines: map[string]string{"infra": "bk"}},
			"b": {Path: r.b, Repos: map[string]string{}},
		},
	}
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r.protection = NewProtection(store.NewWorkspaceRecordStore(db))
	r.service = NewTicketService(ConfigLocator(config.StaticSource(r.cfg)))
	r.service.SetProtection(r.protection)
	return r
}

func (r *migrationReplica) write(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(r.a, filepath.FromSlash(rel))
	mustWrite(t, p, content)
	return p
}

const (
	backlogID  = "2026-10-01-0900-backlog"
	progressID = "2026-10-01-0901-progress"
	doneID     = "2026-10-01-0902-done"
)

func (r *migrationReplica) seed(t *testing.T) {
	t.Helper()
	r.write(t, "OVERVIEW.md", "---\nupdated_at: 2026-10-01T00:00:00Z\n---\n\nSee ["+"tracing]("+r.a+"/investigations/tracing.md) and "+r.core+"/README.md, [b](knowledge/b.md#part), [web](https://example.com), [ref](architect:specs/x.md).\n")
	r.write(t, "tickets/backlog/"+backlogID+"/ticket.md", "---\n"+
		"title: Backlog\nrepo: core\ncreated_at: 2026-10-01T09:00:00Z\nupdated_at: 2026-10-01T09:00:00Z\n"+
		"# a comment that must survive\ncustom: keep\nreferences:\n"+
		"  - "+r.a+"/investigations/tracing.md\n"+
		"  - '"+r.core+"/docs'\n"+
		"  - "+doneID+"\n"+
		"  - "+r.b+"/notes.md\n"+
		"  - /elsewhere/x.md\n"+
		"  - abc\n"+
		"---\n\nBody with "+r.a+"/kept.md in prose.\n")
	r.write(t, "tickets/progress/"+progressID+"/ticket.md", "---\n"+
		"title: Progress\nrepo: infra\ncreated_at: 2026-10-01T09:01:00Z\nupdated_at: 2026-10-01T09:01:00Z\nreferences:\n"+
		"    - /srv/infra/spec.md\n"+
		"    - "+r.a+"/knowledge/a.md\n"+
		"    - "+r.core+"/x.md\n"+
		"---\n\nProgress body.\n")
	r.write(t, "tickets/done/"+doneID+"/ticket.md", "---\n"+
		"title: Done\nrepo: core\ncreated_at: 2026-10-01T09:02:00Z\nupdated_at: 2026-10-01T09:02:00Z\nreferences: [\""+r.a+"/OVERVIEW.md\", "+backlogID+"]\n"+
		"---\n\nDone body.\n")
	r.write(t, "tickets/done/"+doneID+"/conclusion.md", "---\nstarted_at: 2026-10-01T09:02:00Z\nconcluded_at: 2026-10-01T10:00:00Z\noutcome: completed\n---\n\nHistorical "+r.a+"/OVERVIEW.md stays.\n")
	// Accept the current records, as a running daemon would have.
	if _, diags, err := r.protection.Inspect(context.Background(), r.a); err != nil || len(diags) != 0 {
		t.Fatalf("accept records: %v %v", diags, err)
	}
}

func ticketPlan(t *testing.T, report MigrationReport, key, id string) TicketMigration {
	t.Helper()
	for _, a := range report.Architects {
		if a.Key != key {
			continue
		}
		for _, ticket := range a.Tickets {
			if ticket.ID == id {
				return ticket
			}
		}
	}
	t.Fatalf("ticket %s/%s not in report", key, id)
	return TicketMigration{}
}

func dispositions(t TicketMigration) []string {
	var out []string
	for _, m := range t.References {
		out = append(out, m.Disposition+"="+m.To)
	}
	return out
}

func setDecision(t *testing.T, report *MigrationReport, id, from string, to string, drop bool) {
	t.Helper()
	for i := range report.Architects {
		for j := range report.Architects[i].Tickets {
			ticket := &report.Architects[i].Tickets[j]
			if ticket.ID != id {
				continue
			}
			for k := range ticket.References {
				if ticket.References[k].From == from {
					ticket.References[k].To, ticket.References[k].Drop = to, drop
					return
				}
			}
		}
	}
	t.Fatalf("no reference %s in %s", from, id)
}

func TestReferenceMigrationPlanAndApply(t *testing.T) {
	r := newMigrationReplica(t)
	r.seed(t)
	ctx := context.Background()
	original := map[string]string{}
	for _, rel := range []string{"tickets/backlog/" + backlogID + "/ticket.md", "tickets/progress/" + progressID + "/ticket.md", "tickets/done/" + doneID + "/ticket.md", "tickets/done/" + doneID + "/conclusion.md", "OVERVIEW.md"} {
		original[rel] = readFile(t, filepath.Join(r.a, rel))
	}

	plan, err := r.service.PlanReferenceMigration(ctx, r.cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Architects) != 2 || plan.Architects[0].Key != "a" || plan.Architects[1].Key != "b" || plan.Architects[0].Scanned != 3 {
		t.Fatalf("architects = %+v", plan.Architects)
	}
	// Planning changes nothing.
	for rel, content := range original {
		if readFile(t, filepath.Join(r.a, rel)) != content {
			t.Fatalf("plan changed %s", rel)
		}
	}

	backlog := ticketPlan(t, plan, "a", backlogID)
	if got, want := dispositions(backlog), []string{
		"convert=architect:investigations/tracing.md",
		"convert=core:docs",
		"keep=",
		"cross_architect=",
		"unmapped=",
		"invalid=",
	}; !slices.Equal(got, want) {
		t.Fatalf("backlog = %v, want %v", got, want)
	}
	if backlog.References[0].Line != 9 || backlog.References[3].Candidates[0] != "architect b" {
		t.Fatalf("backlog details = %+v", backlog.References)
	}
	progress := ticketPlan(t, plan, "a", progressID)
	if got, want := dispositions(progress), []string{
		"convert=infra:spec.md",
		"convert=architect:knowledge/a.md",
		"ambiguous=",
	}; !slices.Equal(got, want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
	if !strings.Contains(progress.References[1].Reason, "previously looked up on machine bk") || !strings.Contains(progress.References[2].Reason, "interpreted on machine bk") {
		t.Fatalf("progress reasons = %+v", progress.References)
	}
	done := ticketPlan(t, plan, "a", doneID)
	if got, want := dispositions(done), []string{"convert=architect:OVERVIEW.md", "keep="}; !slices.Equal(got, want) {
		t.Fatalf("done = %v, want %v", got, want)
	}
	docs := plan.Architects[0].Documents
	if len(docs) != 3 || docs[0].File != "OVERVIEW.md" || docs[0].Suggestion != "architect:investigations/tracing.md" || docs[1].Suggestion != "core:README.md" || docs[2].Path != "knowledge/b.md#part" || docs[2].Suggestion != "architect:knowledge/b.md" || !strings.Contains(docs[2].Reason, "does not exist") {
		t.Fatalf("documents = %+v", docs)
	}

	// Review: drop the unmapped path, correct the invalid id; the
	// cross-architect and ambiguous entries stay undecided.
	setDecision(t, &plan, backlogID, "/elsewhere/x.md", "", true)
	setDecision(t, &plan, backlogID, "abc", progressID, false)

	applied, err := r.service.ApplyReferenceMigration(ctx, r.cfg, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{backlogID, progressID, doneID} {
		if got := ticketPlan(t, applied, "a", id); got.Result != MigrationApplied {
			t.Fatalf("%s result = %s %s", id, got.Result, got.Message)
		}
	}
	backlogText := readFile(t, filepath.Join(r.a, "tickets/backlog", backlogID, "ticket.md"))
	wantBacklog := strings.Replace(original["tickets/backlog/"+backlogID+"/ticket.md"], "  - "+r.a+"/investigations/tracing.md\n", "  - architect:investigations/tracing.md\n", 1)
	wantBacklog = strings.Replace(wantBacklog, "  - '"+r.core+"/docs'\n", "  - core:docs\n", 1)
	wantBacklog = strings.Replace(wantBacklog, "  - /elsewhere/x.md\n", "", 1)
	wantBacklog = strings.Replace(wantBacklog, "  - abc\n", "  - "+progressID+"\n", 1)
	if backlogText != wantBacklog {
		t.Fatalf("backlog rewritten as\n%s\nwant\n%s", backlogText, wantBacklog)
	}
	doneText := readFile(t, filepath.Join(r.a, "tickets/done", doneID, "ticket.md"))
	if doneText != strings.Replace(original["tickets/done/"+doneID+"/ticket.md"], `["`+r.a+`/OVERVIEW.md", `+backlogID+`]`, `[architect:OVERVIEW.md, `+backlogID+`]`, 1) {
		t.Fatalf("done rewritten as\n%s", doneText)
	}
	for _, rel := range []string{"tickets/done/" + doneID + "/conclusion.md", "OVERVIEW.md"} {
		if readFile(t, filepath.Join(r.a, rel)) != original[rel] {
			t.Fatalf("apply changed %s", rel)
		}
	}
	// The runtime-owned records were accepted through protection: no
	// conflict, and the board resolves the converted references.
	if _, diags, err := r.protection.Inspect(ctx, r.a); err != nil || len(diags) != 0 {
		t.Fatalf("protection after apply: %v %v", diags, err)
	}
	ticket, err := r.service.GetTicket(ctx, r.a, doneID)
	if err != nil || ticket.ResolvedReferences[0].Kind != domain.ReferenceKindArchitect || ticket.Updated == nil || !ticket.Updated.Equal(*mustTime(t, "2026-10-01T09:02:00Z")) {
		t.Fatalf("done after apply = %+v %v", ticket.TicketSummary, err)
	}

	// Rerunning the same plan is idempotent.
	again, err := r.service.ApplyReferenceMigration(ctx, r.cfg, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{backlogID, progressID, doneID} {
		if got := ticketPlan(t, again, "a", id); got.Result != MigrationAlreadyApplied {
			t.Fatalf("rerun %s = %s %s", id, got.Result, got.Message)
		}
	}
	// A new plan lists only what still needs a decision.
	replan, err := r.service.PlanReferenceMigration(ctx, r.cfg, []string{"a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, ticket := range replan.Architects[0].Tickets {
		listed = append(listed, ticket.ID+":"+strings.Join(dispositions(ticket), ","))
	}
	if want := []string{backlogID + ":keep=,keep=,keep=,cross_architect=,keep=", progressID + ":keep=,keep=,ambiguous="}; !slices.Equal(listed, want) {
		t.Fatalf("replan = %v, want %v", listed, want)
	}
}

func mustTime(t *testing.T, s string) *time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return &v
}

// A ticket changed after the plan is a conflict and left untouched; a
// workspace with an unresolved protected-record conflict blocks every record
// rather than being overwritten; an invalid decision is rejected.
func TestReferenceMigrationConflictsAndRejections(t *testing.T) {
	r := newMigrationReplica(t)
	r.seed(t)
	ctx := context.Background()
	plan, err := r.service.PlanReferenceMigration(ctx, r.cfg, []string{"a"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The architect edits the backlog ticket after the plan.
	backlogPath := filepath.Join(r.a, "tickets/backlog", backlogID, "ticket.md")
	edited := strings.Replace(readFile(t, backlogPath), "Body with", "Edited body with", 1)
	if err := os.WriteFile(backlogPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	// And an invalid decision on the progress ticket.
	setDecision(t, &plan, progressID, r.core+"/x.md", "nowhere:x.md", false)

	applied, err := r.service.ApplyReferenceMigration(ctx, r.cfg, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := ticketPlan(t, applied, "a", backlogID); got.Result != MigrationConflict || !strings.Contains(got.Message, "changed since the plan") {
		t.Fatalf("backlog = %s %s", got.Result, got.Message)
	}
	if readFile(t, backlogPath) != edited {
		t.Fatal("conflicting ticket was rewritten")
	}
	if got := ticketPlan(t, applied, "a", progressID); got.Result != MigrationRejected || !strings.Contains(got.Message, `"nowhere" is not architect or a repo key`) {
		t.Fatalf("progress = %s %s", got.Result, got.Message)
	}
	if got := ticketPlan(t, applied, "a", doneID); got.Result != MigrationApplied {
		t.Fatalf("done = %s %s", got.Result, got.Message)
	}

	// An external edit to a conclusion is a protected-record conflict: the
	// next apply changes nothing and says why.
	conclusion := filepath.Join(r.a, "tickets/done", doneID, "conclusion.md")
	if err := os.WriteFile(conclusion, []byte(readFile(t, conclusion)+"tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err = r.service.PlanReferenceMigration(ctx, r.cfg, []string{"a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, backlogPath)
	applied, err = r.service.ApplyReferenceMigration(ctx, r.cfg, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := ticketPlan(t, applied, "a", backlogID); got.Result != MigrationBlocked || !strings.Contains(got.Message, "conclusion.md") {
		t.Fatalf("blocked = %s %s", got.Result, got.Message)
	}
	if readFile(t, backlogPath) != before {
		t.Fatal("blocked apply rewrote a ticket")
	}
}

func TestRewriteReferencesRefusesUnsafeLayouts(t *testing.T) {
	base := "---\ntitle: T\nreferences:\n  - /a/b # note\n---\nbody\n"
	if _, err := rewriteReferences(base, []ReferenceMapping{{From: "/a/b", To: "architect:b"}}); err == nil || !strings.Contains(err.Error(), "comment") {
		t.Fatalf("commented item err = %v", err)
	}
	multi := "---\ntitle: T\nreferences: [/a/b,\n  /c]\n---\nbody\n"
	if _, err := rewriteReferences(multi, []ReferenceMapping{{From: "/a/b", To: "architect:b"}, {From: "/c", Drop: true}}); err == nil || !strings.Contains(err.Error(), "multi-line flow") {
		t.Fatalf("multi-line flow err = %v", err)
	}
	all := "---\ntitle: T\nreferences:\n  - /a/b\nother: x\n---\nbody\n"
	out, err := rewriteReferences(all, []ReferenceMapping{{From: "/a/b", Drop: true}})
	if err != nil || out != "---\ntitle: T\nreferences: []\nother: x\n---\nbody\n" {
		t.Fatalf("drop all = %q %v", out, err)
	}
}

// Explicit prefix rules map paths no configured root contains (a repo that
// moved) and take precedence over inference; a rule whose target owner does
// not exist leaves the entry unmapped with the reason.
func TestReferenceMigrationRules(t *testing.T) {
	r := newMigrationReplica(t)
	r.seed(t)
	rules := []MigrationRule{
		{FromPrefix: "/elsewhere", ToPrefix: "core:moved"},
		{Architect: "a", FromPrefix: r.b, ToPrefix: "nope:"},
	}
	plan, err := r.service.PlanReferenceMigration(context.Background(), r.cfg, []string{"a"}, rules)
	if err != nil {
		t.Fatal(err)
	}
	backlog := ticketPlan(t, plan, "a", backlogID)
	if m := backlog.References[4]; m.Disposition != MigrationConvert || m.To != "core:moved/x.md" || !strings.Contains(m.Reason, "rule /elsewhere") {
		t.Fatalf("ruled = %+v", m)
	}
	if m := backlog.References[3]; m.Disposition != MigrationUnmapped || !strings.Contains(m.Reason, `"nope" is not architect or a repo key`) {
		t.Fatalf("bad rule target = %+v", m)
	}
	if _, err := r.service.PlanReferenceMigration(context.Background(), r.cfg, nil, []MigrationRule{{FromPrefix: "relative", ToPrefix: "core:"}}); err == nil {
		t.Fatal("relative rule accepted")
	}
}
