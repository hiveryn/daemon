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
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

// referenceConfig registers the architect at root with a local repo
// "daemon" and a repo "infra" on machine bk (SSH alias bk-alias).
func referenceConfig(root string) config.Config {
	return config.Config{
		Machines: map[string]config.MachineConfig{"bk": {SSH: "bk-alias"}},
		Architects: map[string]config.ArchitectConfig{"hiveryn": {
			Path:         root,
			Repos:        map[string]string{"daemon": "/local/daemon", "infra": "/remote/infra"},
			RepoMachines: map[string]string{"infra": "bk"},
		}},
	}
}

// fakeRemote answers remote inspections from a per-alias table (absent
// entries are missing) and records every call.
type fakeRemote struct {
	mu    sync.Mutex
	paths map[string]map[string]domain.PathReferenceKind
	down  map[string]error
	calls []string // alias: sorted paths
	// gate, when set, holds every inspection until it is closed.
	gate chan struct{}
}

func (f *fakeRemote) inspect(_ context.Context, alias string, paths []string) ([]pathState, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	f.calls = append(f.calls, alias+": "+strings.Join(sorted, ","))
	if err := f.down[alias]; err != nil {
		return nil, err
	}
	states := make([]pathState, len(paths))
	for i, p := range paths {
		if kind, ok := f.paths[alias][p]; ok {
			states[i].kind = kind
		} else {
			states[i].missing = true
		}
	}
	return states, nil
}

func (f *fakeRemote) set(alias, path string, kind domain.PathReferenceKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paths == nil {
		f.paths = map[string]map[string]domain.PathReferenceKind{}
	}
	if f.paths[alias] == nil {
		f.paths[alias] = map[string]domain.PathReferenceKind{}
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

// The same absolute paths resolve differently for a remote-primary and a
// local-primary ticket: each is inspected on its own primary repo's machine.
func TestPathReferencesResolveOnThePrimaryRepoMachine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	local := t.TempDir()
	same := filepath.Join(local, "same")       // a local file; a remote directory
	localOnly := filepath.Join(local, "local") // a local directory only
	remoteOnly := "/srv/remote-only/spec.md"   // a remote file only
	if err := os.WriteFile(same, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(localOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{paths: map[string]map[string]domain.PathReferenceKind{"bk-alias": {
		same:       domain.PathReferenceDirectory,
		remoteOnly: domain.PathReferenceFile,
	}}}
	clock := &fakeClock{now: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root), remote, clock)

	refTicket(t, root, "target", "daemon")
	refTicket(t, root, "remote-ticket", "infra", "target", same, localOnly, remoteOnly)
	refTicket(t, root, "remote-ticket-2", "infra", same, remoteOnly)
	refTicket(t, root, "local-ticket", "daemon", "target", same, localOnly, remoteOnly)

	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	remoteRefs := resolvedByValue(t, boardTicket(t, board, "remote-ticket"))
	if ref := remoteRefs["target"]; ref.Type != domain.TicketReferenceTicket || !ref.Exists || ref.Status != "" || ref.Location != "" {
		t.Fatalf("ticket reference = %#v", ref)
	}
	if ref := remoteRefs[same]; !ref.Exists || ref.Kind != domain.PathReferenceDirectory || ref.Status != domain.PathReferencePresent || ref.Location != domain.PathReferenceRemote || ref.Machine != "bk" || ref.Error != "" {
		t.Fatalf("remote view of %s = %#v", same, ref)
	}
	if ref := remoteRefs[localOnly]; ref.Exists || ref.Kind != "" || ref.Status != domain.PathReferenceMissing || ref.Location != domain.PathReferenceRemote || ref.Machine != "bk" || ref.Error != "" {
		t.Fatalf("local-only path on remote = %#v", ref)
	}
	if ref := remoteRefs[remoteOnly]; !ref.Exists || ref.Kind != domain.PathReferenceFile {
		t.Fatalf("remote-only path on remote = %#v", ref)
	}
	remoteSummary := boardTicket(t, board, "remote-ticket")
	if codes := warningCodes(remoteSummary); !slices.Equal(codes, []string{warningMissingPathReference}) {
		t.Fatalf("remote warnings = %#v", remoteSummary.Warnings)
	}
	if msg := remoteSummary.Warnings[0].Message; !strings.Contains(msg, `machine "bk" (SSH bk-alias)`) || !strings.Contains(msg, localOnly) {
		t.Fatalf("missing warning does not name the location: %q", msg)
	}

	localRefs := resolvedByValue(t, boardTicket(t, board, "local-ticket"))
	if ref := localRefs[same]; !ref.Exists || ref.Kind != domain.PathReferenceFile || ref.Status != domain.PathReferencePresent || ref.Location != domain.PathReferenceLocal || ref.Machine != "" {
		t.Fatalf("local view of %s = %#v", same, ref)
	}
	if ref := localRefs[localOnly]; !ref.Exists || ref.Kind != domain.PathReferenceDirectory {
		t.Fatalf("local-only path locally = %#v", ref)
	}
	if ref := localRefs[remoteOnly]; ref.Exists || ref.Status != domain.PathReferenceMissing || ref.Location != domain.PathReferenceLocal {
		t.Fatalf("remote-only path locally = %#v", ref)
	}
	localSummary := boardTicket(t, board, "local-ticket")
	if len(localSummary.Warnings) != 1 || !strings.Contains(localSummary.Warnings[0].Message, "this machine (local)") {
		t.Fatalf("local warnings = %#v", localSummary.Warnings)
	}

	// One SSH inspection for the whole board, with each remote path once.
	want := []string{"bk-alias: " + strings.Join(slices.Sorted(slices.Values([]string{same, localOnly, remoteOnly})), ",")}
	if calls := remote.takeCalls(); !slices.Equal(calls, want) {
		t.Fatalf("remote calls = %#v, want %#v", calls, want)
	}

	// Within the TTL the answers are reused; after it they are asked again.
	if _, err := service.ListTickets(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if calls := remote.takeCalls(); len(calls) != 0 {
		t.Fatalf("cached board read inspected again: %#v", calls)
	}
	// After it the stale answers are served while they are refreshed in the
	// background.
	clock.Advance(remoteInspectTTL)
	if _, err := service.ListTickets(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	service.waitForBackground()
	if calls := remote.takeCalls(); len(calls) != 1 {
		t.Fatalf("expired cache calls = %#v", calls)
	}

	// A single-ticket read inspects only that ticket's paths.
	clock.Advance(remoteInspectTTL)
	ticket, err := service.GetTicket(context.Background(), root, "remote-ticket-2")
	if err != nil {
		t.Fatal(err)
	}
	service.waitForBackground()
	if !ticket.ResolvedReferences[0].Exists || len(ticket.Warnings) != 0 {
		t.Fatalf("remote ticket = %#v", ticket.TicketSummary)
	}
	want = []string{"bk-alias: " + strings.Join(slices.Sorted(slices.Values([]string{same, remoteOnly})), ",")}
	if calls := remote.takeCalls(); !slices.Equal(calls, want) {
		t.Fatalf("single-ticket calls = %#v, want %#v", calls, want)
	}
}

// A machine that cannot be inspected leaves its paths unverified — never
// missing, and never answered from this machine even where the path exists
// here.
func TestUnreachableMachineLeavesPathsUnverified(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existsLocally := t.TempDir()
	remote := &fakeRemote{down: map[string]error{"bk-alias": errors.New("SSH bk-alias: exit status 255: Connection refused")}}
	clock := &fakeClock{now: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root), remote, clock)
	refTicket(t, root, "remote-ticket", "infra", existsLocally)

	for range 2 {
		ticket, err := service.GetTicket(context.Background(), root, "remote-ticket")
		if err != nil {
			t.Fatal(err)
		}
		ref := ticket.ResolvedReferences[0]
		if ref.Exists || ref.Kind != "" || ref.Status != domain.PathReferenceUnverified || ref.Location != domain.PathReferenceRemote || ref.Machine != "bk" || !strings.Contains(ref.Error, "Connection refused") {
			t.Fatalf("unreachable reference = %#v", ref)
		}
		if codes := warningCodes(ticket.TicketSummary); !slices.Equal(codes, []string{warningUnverifiedPathReference}) {
			t.Fatalf("warnings = %#v", ticket.Warnings)
		}
		msg := ticket.Warnings[0].Message
		if !strings.Contains(msg, `machine "bk" (SSH bk-alias)`) || !strings.Contains(msg, "Connection refused") || !strings.Contains(msg, existsLocally) {
			t.Fatalf("unverified warning lacks location or cause: %q", msg)
		}
	}
	// The failure is remembered, so the second read did not wait on the host.
	if calls := remote.takeCalls(); len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
}

// A ticket whose primary repo cannot be located has unverified paths naming
// the cause; its ticket-ID references still resolve.
func TestUnlocatableRepoLeavesPathsUnverified(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existsLocally := t.TempDir()
	remote := &fakeRemote{}
	clock := &fakeClock{now: time.Now()}
	cfg := referenceConfig(root)
	service := newReferenceService(cfg, remote, clock)
	refTicket(t, root, "target", "daemon")
	refTicket(t, root, "gone-repo", "removed", "target", existsLocally)
	refTicket(t, root, "no-repo", "", existsLocally)
	if err := os.WriteFile(filepath.Join(root, "tickets/backlog/no-repo/ticket.md"), []byte("---\ntitle: No repo\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}

	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	gone := boardTicket(t, board, "gone-repo")
	if ref := resolvedByValue(t, gone)["target"]; !ref.Exists {
		t.Fatalf("ticket reference = %#v", ref)
	}
	// The machine is unknown, so the path is not treated as local even
	// though it exists here.
	if ref := resolvedByValue(t, gone)[existsLocally]; ref.Exists || ref.Status != domain.PathReferenceUnverified || ref.Location != domain.PathReferenceUnknown || ref.Machine != "" || !strings.Contains(ref.Error, `primary repo "removed" is not configured`) {
		t.Fatalf("path of unlocatable repo = %#v", ref)
	}
	if codes := warningCodes(gone); !slices.Equal(codes, []string{warningUnverifiedPathReference}) || !strings.Contains(gone.Warnings[0].Message, `primary repo "removed" is not configured`) {
		t.Fatalf("gone-repo warnings = %#v", gone.Warnings)
	}
	noRepo := boardTicket(t, board, "no-repo")
	if codes := warningCodes(noRepo); !slices.Equal(codes, []string{"INVALID_TICKET"}) || !strings.Contains(noRepo.Warnings[0].Message, "repo") {
		t.Fatalf("no-repo warnings = %#v", noRepo.Warnings)
	}
	if calls := remote.takeCalls(); len(calls) != 0 {
		t.Fatalf("remote calls = %#v", calls)
	}

	// A workspace that is not a registered architect cannot locate any repo.
	other := t.TempDir()
	refTicket(t, other, "orphan", "daemon", existsLocally)
	orphan, err := service.GetTicket(context.Background(), other, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if codes := warningCodes(orphan.TicketSummary); !slices.Equal(codes, []string{warningUnverifiedPathReference}) || !strings.Contains(orphan.Warnings[0].Message, "no architect is registered at "+other) {
		t.Fatalf("orphan warnings = %#v", orphan.Warnings)
	}
}

// Through a fake ssh: a board's remote paths travel in one SSH invocation,
// the answers distinguish directories, files and absence, and a host that
// goes down makes them unverified without consulting this machine.
func TestRemoteReferencesThroughSSH(t *testing.T) {
	fake := sshtest.Install(t)
	root := t.TempDir()
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	spaced := filepath.Join(dir, " leading space")
	if err := os.Mkdir(spaced, 0o755); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Now()}
	service := NewTicketService(ConfigLocator(config.StaticSource(referenceConfig(root))))
	service.now = clock.Now
	refTicket(t, root, "one", "infra", dir, file)
	refTicket(t, root, "two", "infra", missing, `"`+spaced+`"`, file)

	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Commands("bk-alias")); n != 1 {
		t.Fatalf("ssh invocations = %d, want 1", n)
	}
	one := resolvedByValue(t, boardTicket(t, board, "one"))
	if one[dir].Kind != domain.PathReferenceDirectory || one[file].Kind != domain.PathReferenceFile {
		t.Fatalf("one = %#v", one)
	}
	two := boardTicket(t, board, "two")
	refs := resolvedByValue(t, two)
	if refs[missing].Exists || refs[spaced].Kind != domain.PathReferenceDirectory || refs[file].Kind != domain.PathReferenceFile {
		t.Fatalf("two = %#v", refs)
	}
	if codes := warningCodes(two); !slices.Equal(codes, []string{warningMissingPathReference}) {
		t.Fatalf("two warnings = %#v", two.Warnings)
	}

	// A host that goes down: the first read past the TTL is served the
	// earlier answers while they are refreshed; the refresh fails and later
	// reads report the failure.
	fake.SetDown(t, "bk", true)
	fake.SetDown(t, "bk-alias", true)
	clock.Advance(remoteInspectTTL)
	ticket, err := service.GetTicket(context.Background(), root, "one")
	if err != nil {
		t.Fatal(err)
	}
	if !ticket.ResolvedReferences[0].Exists || !ticket.ResolvedReferences[1].Exists {
		t.Fatalf("stale references = %#v", ticket.ResolvedReferences)
	}
	service.waitForBackground()
	ticket, err = service.GetTicket(context.Background(), root, "one")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.ResolvedReferences[0].Exists || ticket.ResolvedReferences[1].Exists {
		t.Fatalf("down host references = %#v", ticket.ResolvedReferences)
	}
	if codes := warningCodes(ticket.TicketSummary); !slices.Equal(codes, []string{warningUnverifiedPathReference, warningUnverifiedPathReference}) || !strings.Contains(ticket.Warnings[0].Message, "Connection refused") {
		t.Fatalf("down host warnings = %#v", ticket.Warnings)
	}
}

func TestInspectRemotePathsReportsUnsearchableAncestors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can search any directory")
	}
	sshtest.Install(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	states, err := inspectRemotePaths(context.Background(), "bk-alias", []string{filepath.Join(locked, "inner", "x"), filepath.Join(dir, "absent", "x")})
	if err != nil {
		t.Fatal(err)
	}
	if states[0].err == nil || states[0].missing || states[0].kind != "" {
		t.Fatalf("behind unsearchable ancestor = %#v", states[0])
	}
	if !states[1].missing {
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
	remote.set("bk-alias", "/srv/spec.md", domain.PathReferenceFile)
	clock := &fakeClock{now: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root), remote, clock)
	service.waitBudget = 50 * time.Millisecond
	changes := &changeRecorder{}
	service.SetReferencesChanged(changes.record)
	refTicket(t, root, "remote-ticket", "infra", "/srv/spec.md")

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
		summary := boardTicket(t, board, "remote-ticket")
		ref := resolvedByValue(t, summary)["/srv/spec.md"]
		if ref.Exists || ref.Status != domain.PathReferenceUnverified || ref.Location != domain.PathReferenceRemote || ref.Machine != "bk" || !strings.Contains(ref.Error, "no answer within 50ms") {
			t.Fatalf("pending reference = %#v", ref)
		}
		if codes := warningCodes(summary); !slices.Equal(codes, []string{warningUnverifiedPathReference}) || !strings.Contains(summary.Warnings[0].Message, `machine "bk" (SSH bk-alias)`) {
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
	if ref := resolvedByValue(t, boardTicket(t, board, "remote-ticket"))["/srv/spec.md"]; !ref.Exists || ref.Status != domain.PathReferencePresent {
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
	service := newReferenceService(referenceConfig(root), remote, clock)
	service.waitBudget = time.Hour
	refTicket(t, root, "remote-ticket", "infra", "/srv/spec.md")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ticket, err := service.GetTicket(ctx, root, "remote-ticket")
	if err != nil {
		t.Fatal(err)
	}
	if ref := ticket.ResolvedReferences[0]; ref.Status != domain.PathReferenceUnverified {
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
	remote.set("bk-alias", "/srv/spec.md", domain.PathReferenceFile)
	clock := &fakeClock{now: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)}
	service := newReferenceService(referenceConfig(root), remote, clock)
	changes := &changeRecorder{}
	service.SetReferencesChanged(changes.record)
	refTicket(t, root, "remote-ticket", "infra", "/srv/spec.md")

	read := func() domain.TicketReference {
		t.Helper()
		ticket, err := service.GetTicket(context.Background(), root, "remote-ticket")
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
	delete(remote.paths["bk-alias"], "/srv/spec.md")
	remote.mu.Unlock()
	clock.Advance(remoteInspectTTL)
	if ref := read(); !ref.Exists {
		t.Fatalf("stale read = %#v", ref)
	}
	service.waitForBackground()
	if seen := changes.take(); !slices.Equal(seen, []string{root}) {
		t.Fatalf("told = %#v", seen)
	}
	if ref := read(); ref.Exists || ref.Status != domain.PathReferenceMissing {
		t.Fatalf("refreshed read = %#v", ref)
	}

	// Past the stale limit an answer is no longer served.
	clock.Advance(remoteStaleLimit)
	service.waitBudget = 0
	remote.gate = make(chan struct{})
	if ref := read(); ref.Status != domain.PathReferenceUnverified {
		t.Fatalf("expired read = %#v", ref)
	}
	close(remote.gate)
	service.waitForBackground()
}
