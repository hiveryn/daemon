package architectfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/contextref"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

// referenceConfig registers the architect at root with a local repo
// "daemon" at localRepo and a repo "infra" at /remote/infra on machine bk
// (SSH alias bk-alias).
func referenceConfig(root, localRepo string) config.Config {
	return referenceConfigWithRemote(root, localRepo, "/remote/infra")
}

func referenceConfigWithRemote(root, localRepo, remoteRepo string) config.Config {
	return config.Config{
		Machines: map[string]config.MachineConfig{"bk": {SSH: "bk-alias"}},
		Architects: map[string]config.ArchitectConfig{"hiveryn": {
			Path:         root,
			Repos:        map[string]string{"daemon": localRepo, "infra": remoteRepo},
			RepoMachines: map[string]string{"infra": "bk"},
		}},
	}
}

// fakeRemote answers remote inspections from a per-alias table of absolute
// paths (absent entries are missing) and records every call.
type fakeRemote struct {
	mu    sync.Mutex
	paths map[string]map[string]domain.PathKind
	down  map[string]error
	calls []string // alias: sorted absolute paths
	// gate, when set, holds every inspection until it is closed.
	gate chan struct{}
}

func (f *fakeRemote) inspect(_ context.Context, alias string, items []string) ([]contextref.State, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var sorted []string
	for _, item := range items {
		_, p, _ := strings.Cut(item, "\x00")
		sorted = append(sorted, p)
	}
	slices.Sort(sorted)
	f.calls = append(f.calls, alias+": "+strings.Join(sorted, ","))
	if err := f.down[alias]; err != nil {
		return nil, err
	}
	states := make([]contextref.State, len(items))
	for i, item := range items {
		_, p, _ := strings.Cut(item, "\x00")
		if kind, ok := f.paths[alias][p]; ok {
			states[i].Kind = kind
		} else {
			states[i].Missing = true
		}
	}
	return states, nil
}

func (f *fakeRemote) set(alias, path string, kind domain.PathKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paths == nil {
		f.paths = map[string]map[string]domain.PathKind{}
	}
	if f.paths[alias] == nil {
		f.paths[alias] = map[string]domain.PathKind{}
	}
	f.paths[alias][path] = kind
}

func (f *fakeRemote) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newReferenceService(cfg config.Config, remote *fakeRemote, clock *fakeClock) *TicketService {
	s := NewTicketService(ConfigLocator(config.StaticSource(cfg)))
	s.inspectRemote = remote.inspect
	s.now = clock.Now
	return s
}

func refTicket(t *testing.T, root, id, repo string, refs ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("---\ntitle: " + id + "\n")
	if repo != "" {
		b.WriteString("repo: " + repo + "\n")
	}
	b.WriteString("references:\n")
	for _, ref := range refs {
		b.WriteString("  - " + ref + "\n")
	}
	b.WriteString("---\n\nbody\n")
	writeTicketFile(t, root, domain.TicketStatusBacklog, id, b.String())
}

func resolvedByValue(t *testing.T, summary domain.TicketSummary) map[string]domain.TicketReference {
	t.Helper()
	out := map[string]domain.TicketReference{}
	for _, ref := range summary.ResolvedReferences {
		out[ref.Value] = ref
	}
	return out
}

func boardTicket(t *testing.T, board domain.TicketBoard, id string) domain.TicketSummary {
	t.Helper()
	for _, summary := range append(append(board.Backlog, board.Progress...), board.Done...) {
		if summary.ID == id {
			return summary
		}
	}
	t.Fatalf("ticket %s not on board", id)
	return domain.TicketSummary{}
}

func warningCodes(summary domain.TicketSummary) []string {
	var codes []string
	for _, w := range summary.Warnings {
		codes = append(codes, w.Code)
	}
	return codes
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	targetID = "2026-10-27-0100-target"
	remoteID = "2026-10-27-0101-remote-primary"
)

// The defect this contract fixes: a ticket whose primary repo is remote still
// finds its architect-workspace document on this machine, a local repo locally
// and a remote repo on its own machine. Each reference is resolved where its
// target lives, never on the ticket's primary machine.
func TestReferencesResolveWhereTheirTargetLives(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	localRepo := t.TempDir()
	mustWrite(t, filepath.Join(root, "investigations", "2026-10-27-tracing.md"), "notes")
	if err := os.MkdirAll(filepath.Join(localRepo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	remote.set("bk-alias", "/remote/infra/spec.md", domain.PathKindFile)
	clock := &fakeClock{now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root, localRepo), remote, clock)

	refTicket(t, root, targetID, "daemon")
	refTicket(t, root, remoteID, "infra",
		targetID,
		"architect:investigations/2026-10-27-tracing.md",
		"daemon:docs",
		"infra:spec.md",
		"infra:absent.md",
		"architect:missing.md",
		"2026-10-27-0102-nowhere",
	)

	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	summary := boardTicket(t, board, remoteID)
	refs := resolvedByValue(t, summary)
	check := func(value string, want domain.TicketReference) {
		t.Helper()
		want.Value = value
		if got := refs[value]; got != want {
			t.Fatalf("%s =\n %#v\nwant\n %#v", value, got, want)
		}
	}
	check(targetID, domain.TicketReference{Kind: domain.ReferenceKindTicket, Status: domain.ReferencePresent, Exists: true, Access: domain.ReferenceAccessReadTicket})
	check("architect:investigations/2026-10-27-tracing.md", domain.TicketReference{Kind: domain.ReferenceKindArchitect, Status: domain.ReferencePresent, Exists: true, Owner: "architect", Target: "investigations/2026-10-27-tracing.md", Location: domain.ReferenceLocal, PathKind: domain.PathKindFile, Access: domain.ReferenceAccessReadContext})
	check("daemon:docs", domain.TicketReference{Kind: domain.ReferenceKindRepo, Status: domain.ReferencePresent, Exists: true, Owner: "daemon", Target: "docs", Location: domain.ReferenceLocal, PathKind: domain.PathKindDirectory, Access: domain.ReferenceAccessListContext})
	check("infra:spec.md", domain.TicketReference{Kind: domain.ReferenceKindRepo, Status: domain.ReferencePresent, Exists: true, Owner: "infra", Target: "spec.md", Location: domain.ReferenceRemote, Machine: "bk", PathKind: domain.PathKindFile, Access: domain.ReferenceAccessReadContext})
	check("infra:absent.md", domain.TicketReference{Kind: domain.ReferenceKindRepo, Status: domain.ReferenceMissing, Owner: "infra", Target: "absent.md", Location: domain.ReferenceRemote, Machine: "bk", Access: domain.ReferenceAccessReadContext})
	check("architect:missing.md", domain.TicketReference{Kind: domain.ReferenceKindArchitect, Status: domain.ReferenceMissing, Owner: "architect", Target: "missing.md", Location: domain.ReferenceLocal, Access: domain.ReferenceAccessReadContext})
	check("2026-10-27-0102-nowhere", domain.TicketReference{Kind: domain.ReferenceKindTicket, Status: domain.ReferenceMissing, Access: domain.ReferenceAccessReadTicket})

	if codes := warningCodes(summary); !slices.Equal(codes, []string{warningMissingReference, warningMissingReference, warningBrokenReference}) {
		t.Fatalf("warnings = %#v", summary.Warnings)
	}
	if msg := summary.Warnings[0].Message; !strings.Contains(msg, `machine "bk" (SSH bk-alias)`) || !strings.Contains(msg, "infra:absent.md") || !strings.Contains(msg, "ticket.md line 11: ") {
		t.Fatalf("remote missing warning = %q", msg)
	}
	if msg := summary.Warnings[1].Message; !strings.Contains(msg, "this machine (local)") {
		t.Fatalf("architect missing warning = %q", msg)
	}
	// Only the remote repo's targets travel over SSH, once for the board.
	want := []string{"bk-alias: /remote/infra/absent.md,/remote/infra/spec.md"}
	if calls := remote.takeCalls(); !slices.Equal(calls, want) {
		t.Fatalf("remote calls = %#v, want %#v", calls, want)
	}

	// Within the TTL the answers are reused; after it the stale answers are
	// served while they are refreshed in the background.
	if _, err := service.ListTickets(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if calls := remote.takeCalls(); len(calls) != 0 {
		t.Fatalf("cached board read inspected again: %#v", calls)
	}
	clock.Advance(remoteInspectTTL)
	if _, err := service.GetTicket(context.Background(), root, remoteID); err != nil {
		t.Fatal(err)
	}
	service.waitForBackground()
	if calls := remote.takeCalls(); len(calls) != 1 {
		t.Fatalf("expired cache calls = %#v", calls)
	}
}

// A malformed entry is reported on its own, with the line and remediation; it
// neither hides the ticket nor its other references, and nothing is
// interpreted as a legacy absolute path.
func TestInvalidReferencesAreReportedIndividually(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	localRepo := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.md"), "x")
	mustWrite(t, filepath.Join(root, "OVERVIEW.md"), "x")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	service := newReferenceService(referenceConfig(root, localRepo), remote, &fakeClock{now: time.Now()})
	absolute := filepath.Join(root, "OVERVIEW.md")
	refTicket(t, root, remoteID, "infra",
		absolute,
		"OVERVIEW.md",
		"unknown:docs",
		"architect:../x",
		"architect:escape.md",
		"architect:OVERVIEW.md",
		"architect:OVERVIEW.md",
	)
	ticket, err := service.GetTicket(context.Background(), root, remoteID)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Title != remoteID {
		t.Fatalf("ticket hidden: %#v", ticket.TicketSummary)
	}
	statuses := make([]domain.ReferenceStatus, 0, len(ticket.ResolvedReferences))
	for _, ref := range ticket.ResolvedReferences {
		statuses = append(statuses, ref.Status)
	}
	wantStatuses := []domain.ReferenceStatus{domain.ReferenceInvalid, domain.ReferenceInvalid, domain.ReferenceInvalid, domain.ReferenceInvalid, domain.ReferenceInvalid, domain.ReferencePresent, domain.ReferenceInvalid}
	if !slices.Equal(statuses, wantStatuses) {
		t.Fatalf("statuses = %v, want %v (%#v)", statuses, wantStatuses, ticket.ResolvedReferences)
	}
	if codes := warningCodes(ticket.TicketSummary); len(codes) != 6 || slices.ContainsFunc(codes, func(c string) bool { return c != WarningInvalidReference }) {
		t.Fatalf("warnings = %#v", ticket.Warnings)
	}
	for i, want := range []string{"absolute and home-relative paths are not references", "unqualified path", `"unknown" is not architect or a repo key`, "'..' segments", "outside its root", "duplicate reference"} {
		if msg := ticket.Warnings[i].Message; !strings.Contains(msg, want) || !strings.Contains(msg, "ticket.md line ") {
			t.Fatalf("warning %d = %q, want it to mention %q and its line", i, msg, want)
		}
	}
	if !strings.Contains(ticket.Warnings[2].Message, "configured: daemon, infra") {
		t.Fatalf("unknown-key warning does not list the keys: %q", ticket.Warnings[2].Message)
	}
	if calls := remote.takeCalls(); len(calls) != 0 {
		t.Fatalf("invalid references inspected remotely: %#v", calls)
	}
}

// A machine that cannot be inspected leaves its targets unverified — never
// missing, and never answered from this machine even where the same path
// exists here; architect references are unaffected.
func TestUnreachableMachineLeavesTargetsUnverified(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existsLocally := t.TempDir()
	mustWrite(t, filepath.Join(existsLocally, "spec.md"), "x")
	mustWrite(t, filepath.Join(root, "OVERVIEW.md"), "x")
	remote := &fakeRemote{down: map[string]error{"bk-alias": errors.New("SSH bk-alias: exit status 255: Connection refused")}}
	service := newReferenceService(referenceConfigWithRemote(root, t.TempDir(), existsLocally), remote, &fakeClock{now: time.Now()})
	refTicket(t, root, remoteID, "infra", "infra:spec.md", "architect:OVERVIEW.md")

	for range 2 {
		ticket, err := service.GetTicket(context.Background(), root, remoteID)
		if err != nil {
			t.Fatal(err)
		}
		ref := ticket.ResolvedReferences[0]
		if ref.Exists || ref.PathKind != "" || ref.Status != domain.ReferenceUnverified || ref.Location != domain.ReferenceRemote || ref.Machine != "bk" || !strings.Contains(ref.Error, "Connection refused") {
			t.Fatalf("unreachable reference = %#v", ref)
		}
		if ref := ticket.ResolvedReferences[1]; ref.Status != domain.ReferencePresent || ref.Location != domain.ReferenceLocal {
			t.Fatalf("architect reference = %#v", ref)
		}
		if codes := warningCodes(ticket.TicketSummary); !slices.Equal(codes, []string{warningUnverifiedReference}) || !strings.Contains(ticket.Warnings[0].Message, `machine "bk" (SSH bk-alias)`) {
			t.Fatalf("warnings = %#v", ticket.Warnings)
		}
	}
	// The failure is remembered, so the second read did not wait on the host.
	if calls := remote.takeCalls(); len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
}

// A workspace that is not a registered architect cannot locate anything: its
// architect and repo references are unverified with an unknown location;
// ticket-ID references still resolve.
func TestUnregisteredWorkspaceLeavesTargetsUnverified(t *testing.T) {
	t.Parallel()
	other := t.TempDir()
	mustWrite(t, filepath.Join(other, "OVERVIEW.md"), "x")
	service := newReferenceService(referenceConfig(t.TempDir(), t.TempDir()), &fakeRemote{}, &fakeClock{now: time.Now()})
	refTicket(t, other, targetID, "daemon")
	refTicket(t, other, remoteID, "daemon", targetID, "architect:OVERVIEW.md")
	ticket, err := service.GetTicket(context.Background(), other, remoteID)
	if err != nil {
		t.Fatal(err)
	}
	if ref := ticket.ResolvedReferences[0]; !ref.Exists {
		t.Fatalf("ticket reference = %#v", ref)
	}
	if ref := ticket.ResolvedReferences[1]; ref.Status != domain.ReferenceUnverified || ref.Location != domain.ReferenceUnknown || !strings.Contains(ref.Error, "no architect is registered at "+other) {
		t.Fatalf("architect reference = %#v", ref)
	}
}

// Through a fake ssh: the board's remote targets travel in one SSH
// invocation, the answers distinguish directories, files, absence and
// symlink escapes, and a host that goes down makes them unverified.
func TestRemoteReferencesThroughSSH(t *testing.T) {
	fake := sshtest.Install(t)
	root := t.TempDir()
	remoteRepo := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(remoteRepo, "file.txt"), "")
	mustWrite(t, filepath.Join(remoteRepo, " leading space", "x"), "")
	mustWrite(t, filepath.Join(outside, "secret"), "")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(remoteRepo, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(remoteRepo, "inner-link")); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Now()}
	service := NewTicketService(ConfigLocator(config.StaticSource(referenceConfigWithRemote(root, t.TempDir(), remoteRepo))))
	service.now = clock.Now
	refTicket(t, root, targetID, "infra", "infra:.", "infra:file.txt", "infra:escape", "infra:inner-link")
	refTicket(t, root, remoteID, "infra", "infra:missing", `"infra: leading space"`, "infra:file.txt")

	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Commands("bk-alias")); n != 1 {
		t.Fatalf("ssh invocations = %d, want 1", n)
	}
	one := resolvedByValue(t, boardTicket(t, board, targetID))
	if one["infra:."].PathKind != domain.PathKindDirectory || one["infra:file.txt"].PathKind != domain.PathKindFile || one["infra:inner-link"].PathKind != domain.PathKindFile {
		t.Fatalf("one = %#v", one)
	}
	if ref := one["infra:escape"]; ref.Status != domain.ReferenceInvalid || !strings.Contains(ref.Error, "outside its root") {
		t.Fatalf("escape = %#v", ref)
	}
	two := boardTicket(t, board, remoteID)
	refs := resolvedByValue(t, two)
	if refs["infra:missing"].Status != domain.ReferenceMissing || refs["infra: leading space"].PathKind != domain.PathKindDirectory {
		t.Fatalf("two = %#v", refs)
	}

	fake.SetDown(t, "bk", true)
	fake.SetDown(t, "bk-alias", true)
	clock.Advance(remoteStaleLimit)
	ticket, err := service.GetTicket(context.Background(), root, targetID)
	if err != nil {
		t.Fatal(err)
	}
	service.waitForBackground()
	for _, ref := range ticket.ResolvedReferences {
		if ref.Status != domain.ReferenceUnverified {
			t.Fatalf("down host reference = %#v", ref)
		}
	}
}

func TestInspectRemoteReportsUnsearchableAncestors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can search any directory")
	}
	sshtest.Install(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	states, err := contextref.InspectRemote(context.Background(), "bk-alias", [][2]string{{dir, filepath.Join(locked, "inner", "x")}, {dir, filepath.Join(dir, "absent", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Err == nil || states[0].Missing || states[0].Kind != "" {
		t.Fatalf("behind unsearchable ancestor = %#v", states[0])
	}
	if !states[1].Missing {
		t.Fatalf("absent = %#v", states[1])
	}
}

// changeRecorder records the workspaces told that their references changed.
type changeRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *changeRecorder) record(architectPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, architectPath)
}

func (r *changeRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := r.seen
	r.seen = nil
	return seen
}

// A machine that has not answered within the wait budget leaves the board
// read responsive with its paths unverified; the inspection continues, its
// answer is cached for the next read and the workspace is told.
func TestSlowMachineDoesNotHoldUpTheBoard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	remote := &fakeRemote{gate: make(chan struct{})}
	remote.set("bk-alias", "/remote/infra/spec.md", domain.PathKindFile)
	clock := &fakeClock{now: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root, t.TempDir()), remote, clock)
	service.waitBudget = 50 * time.Millisecond
	changes := &changeRecorder{}
	service.SetReferencesChanged(changes.record)
	refTicket(t, root, remoteID, "infra", "infra:spec.md")

	// Two reads while the machine is silent: neither waits past the budget,
	// and they share one inspection.
	for range 2 {
		started := time.Now()
		board, err := service.ListTickets(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("board read took %s", elapsed)
		}
		summary := boardTicket(t, board, remoteID)
		ref := resolvedByValue(t, summary)["infra:spec.md"]
		if ref.Exists || ref.Status != domain.ReferenceUnverified || ref.Location != domain.ReferenceRemote || ref.Machine != "bk" || !strings.Contains(ref.Error, "no answer within 50ms") {
			t.Fatalf("pending reference = %#v", ref)
		}
		if codes := warningCodes(summary); !slices.Equal(codes, []string{warningUnverifiedReference}) || !strings.Contains(summary.Warnings[0].Message, `machine "bk" (SSH bk-alias)`) {
			t.Fatalf("pending warnings = %#v", summary.Warnings)
		}
	}
	if seen := changes.take(); len(seen) != 0 {
		t.Fatalf("told before the answer: %#v", seen)
	}

	close(remote.gate)
	service.waitForBackground()
	if calls := remote.takeCalls(); len(calls) != 1 {
		t.Fatalf("remote calls = %#v", calls)
	}
	if seen := changes.take(); !slices.Equal(seen, []string{root}) {
		t.Fatalf("told = %#v, want the workspace once", seen)
	}

	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if ref := resolvedByValue(t, boardTicket(t, board, remoteID))["infra:spec.md"]; !ref.Exists || ref.Status != domain.ReferencePresent {
		t.Fatalf("answered reference = %#v", ref)
	}
	if calls := remote.takeCalls(); len(calls) != 0 {
		t.Fatalf("answered read inspected again: %#v", calls)
	}
}

// A read whose request ends stops waiting at once.
func TestCancelledReadDoesNotWaitForTheMachine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	remote := &fakeRemote{gate: make(chan struct{})}
	clock := &fakeClock{now: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root, t.TempDir()), remote, clock)
	service.waitBudget = time.Hour
	refTicket(t, root, remoteID, "infra", "infra:spec.md")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ticket, err := service.GetTicket(ctx, root, remoteID)
	if err != nil {
		t.Fatal(err)
	}
	if ref := ticket.ResolvedReferences[0]; ref.Status != domain.ReferenceUnverified {
		t.Fatalf("reference = %#v", ref)
	}
	close(remote.gate)
	service.waitForBackground()
}

// A stale answer is served without waiting while it is refreshed; the
// workspace is told only when the refresh changes it.
func TestStaleAnswersAreServedWhileRefreshed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	remote := &fakeRemote{}
	remote.set("bk-alias", "/remote/infra/spec.md", domain.PathKindFile)
	clock := &fakeClock{now: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root, t.TempDir()), remote, clock)
	changes := &changeRecorder{}
	service.SetReferencesChanged(changes.record)
	refTicket(t, root, remoteID, "infra", "infra:spec.md")

	read := func() domain.TicketReference {
		t.Helper()
		ticket, err := service.GetTicket(context.Background(), root, remoteID)
		if err != nil {
			t.Fatal(err)
		}
		return ticket.ResolvedReferences[0]
	}
	if ref := read(); !ref.Exists {
		t.Fatalf("first read = %#v", ref)
	}
	remote.takeCalls()

	// Unchanged on refresh: nobody is told.
	clock.Advance(remoteInspectTTL)
	remote.gate = make(chan struct{})
	if ref := read(); !ref.Exists {
		t.Fatalf("stale read = %#v", ref)
	}
	close(remote.gate)
	service.waitForBackground()
	remote.gate = nil
	if calls := remote.takeCalls(); len(calls) != 1 {
		t.Fatalf("refresh calls = %#v", calls)
	}
	if seen := changes.take(); len(seen) != 0 {
		t.Fatalf("told of an unchanged answer: %#v", seen)
	}

	// Gone on refresh: the stale read still shows it, then the workspace is
	// told and the next read reports it missing.
	remote.mu.Lock()
	delete(remote.paths["bk-alias"], "/remote/infra/spec.md")
	remote.mu.Unlock()
	clock.Advance(remoteInspectTTL)
	if ref := read(); !ref.Exists {
		t.Fatalf("stale read = %#v", ref)
	}
	service.waitForBackground()
	if seen := changes.take(); !slices.Equal(seen, []string{root}) {
		t.Fatalf("told = %#v", seen)
	}
	if ref := read(); ref.Exists || ref.Status != domain.ReferenceMissing {
		t.Fatalf("refreshed read = %#v", ref)
	}

	// Past the stale limit an answer is no longer served.
	clock.Advance(remoteStaleLimit)
	service.waitBudget = 0
	remote.gate = make(chan struct{})
	if ref := read(); ref.Status != domain.ReferenceUnverified {
		t.Fatalf("expired read = %#v", ref)
	}
	close(remote.gate)
	service.waitForBackground()
}
