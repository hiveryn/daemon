package sessionruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// recordingAdapter is a fake provider adapter of one kind that records every
// launch; companions launch concurrently, so it is safe to share.
type recordingAdapter struct {
	kind     agentruntime.AgentKind
	mu       sync.Mutex
	launches []agentruntime.StartRequest
	fail     error
}

func (a *recordingAdapter) Agent() agentruntime.AgentKind       { return a.kind }
func (a *recordingAdapter) ConfigRoot(map[string]string) string { return "" }
func (a *recordingAdapter) RemoveSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}
func (a *recordingAdapter) EnsureSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}
func (a *recordingAdapter) NormalizeEvent(context.Context, []byte) (*agentruntime.Event, error) {
	return nil, nil
}
func (a *recordingAdapter) LocateTranscript(context.Context, agentruntime.LocateRequest) (string, error) {
	return "", nil
}
func (a *recordingAdapter) ParseUsage(context.Context, string) (agentruntime.Usage, error) {
	return agentruntime.Usage{}, nil
}
func (a *recordingAdapter) PrepareLaunch(_ context.Context, req agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return agentruntime.LaunchSpec{}, a.fail
	}
	a.launches = append(a.launches, req)
	return agentruntime.LaunchSpec{Command: string(a.kind), Workdir: req.Workdir}, nil
}
func (a *recordingAdapter) launch(sessionID string) (agentruntime.StartRequest, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, req := range a.launches {
		if req.ID == sessionID {
			return req, true
		}
	}
	return agentruntime.StartRequest{}, false
}

type companionFixture struct {
	*serviceFixture
	companions *store.CompanionStore
	claude     *recordingAdapter
	codex      *recordingAdapter
	publisher  *fakePublisher
	workspace  string
	repos      map[string]string
	architect  domain.Session
	events     *architectEventRecorder
}

// newCompanionFixture configures architect hiveryn with local repos alpha and
// beta, local claude and codex variants, and the given companions, and
// records a running architect session without launching it.
func newCompanionFixture(t *testing.T, companions map[string]config.CompanionConfig) *companionFixture {
	t.Helper()
	f := &companionFixture{serviceFixture: newServiceFixture(t)}
	f.reopen(t)
	f.repos = map[string]string{}
	for _, key := range []string{"alpha", "beta"} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.repos[key] = dir
	}
	f.workspace = testWorkspace(t, f.repos)
	f.service.cfg.Architects = map[string]config.ArchitectConfig{"hiveryn": {Name: "Hiveryn", Path: f.workspace, Repos: f.repos, RepoMachines: map[string]string{}, Companions: companions}}
	ctx := context.Background()
	session, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect, ContextID: "2026-10-30-0412", Prompt: "kickoff", Workdir: f.workspace, CreatedBy: domain.SessionCreatedByDesktop})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: "codex", ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"}, Workdir: f.workspace}); err != nil {
		t.Fatal(err)
	}
	f.architect, _ = f.sessions.GetSession(ctx, session.ID)
	return f
}

// reopen rebuilds the service over the same database, as a daemon restart.
func (f *companionFixture) reopen(t *testing.T) {
	t.Helper()
	cfg := f.service.cfg
	f.open(t)
	f.service.cfg = cfg
	f.service.cfg.Variants = map[string]config.VariantConfig{
		"codex":  {Agent: "codex"},
		"claude": {Agent: "claude"},
		"oc":     {Agent: "opencode"},
	}
	f.claude = &recordingAdapter{kind: agentruntime.AgentClaude}
	f.codex = &recordingAdapter{kind: agentruntime.AgentCodex}
	f.service.adapters = map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentClaude: f.claude, agentruntime.AgentCodex: f.codex}
	f.service.actions = newActionStore()
	f.service.questions = newQuestionStore()
	f.publisher = &fakePublisher{}
	f.service.notifier = f.publisher
	f.events = &architectEventRecorder{}
	f.service.SetArchitectPublisher(f.events.publish)
	f.companions = store.NewCompanionStore(f.db)
	f.service.SetCompanionRepository(f.companions)
}

func (f *companionFixture) start(t *testing.T) []domain.Companion {
	t.Helper()
	f.service.startCompanions(context.Background(), f.architect)
	f.service.companionRT.wg.Wait()
	return f.records(t)
}

func (f *companionFixture) records(t *testing.T) []domain.Companion {
	t.Helper()
	records, err := f.companions.ListCompanions(context.Background(), f.architect.ID)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func (f *companionFixture) record(t *testing.T, name string) domain.Companion {
	t.Helper()
	c, err := f.companions.GetCompanion(context.Background(), f.architect.ID, name)
	if err != nil {
		t.Fatalf("companion %s: %v", name, err)
	}
	return c
}

func (f *companionFixture) messagesTo(t *testing.T, sessionID string, kind domain.AgentMessageKind) []domain.AgentMessage {
	t.Helper()
	all, err := f.messages.ListAgentMessages(context.Background(), "hiveryn", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.AgentMessage
	for _, m := range all {
		if m.Recipient == domain.AgentMessageRecipientSession+sessionID && m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func (f *companionFixture) sessionExists(t *testing.T, id string) bool {
	t.Helper()
	_, err := f.sessions.GetSession(context.Background(), id)
	if err != nil && !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatal(err)
	}
	return err == nil
}

func twoCompanions() map[string]config.CompanionConfig {
	return map[string]config.CompanionConfig{
		"browser":  {Repo: "alpha", Variant: "claude"},
		"research": {Repo: "beta", Variant: "codex"},
	}
}

// Every configured companion starts with a new architect session, owned by
// it, with the approved instructions and a date-only kickoff, and the
// architect learns each named outcome in one message.
func TestCompanionsStartWithArchitectSessionAndReportOutcomes(t *testing.T) {
	f := newCompanionFixture(t, map[string]config.CompanionConfig{
		"browser":  {Repo: "alpha", Variant: "claude"},
		"research": {Repo: "beta", Variant: "codex"},
		"broken":   {Repo: "beta", Variant: "oc"},
		"missing":  {Repo: "alpha", Variant: "nope"},
	})
	records := f.start(t)
	if len(records) != 4 {
		t.Fatalf("records = %+v", records)
	}
	system, err := companionInstructions()
	if err != nil || !strings.HasPrefix(system, "## 1. Role and autonomy") {
		t.Fatalf("companion instructions: %v %q", err, system[:min(40, len(system))])
	}
	for name, adapter := range map[string]*recordingAdapter{"browser": f.claude, "research": f.codex} {
		c := f.record(t, name)
		if c.Status != domain.CompanionRunning || c.SessionID == "" || c.Agent != string(adapter.kind) {
			t.Fatalf("%s = %+v", name, c)
		}
		session, err := f.sessions.GetSession(context.Background(), c.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.SessionType != domain.SessionTypeCompanion || session.OwnerSessionID != f.architect.ID || session.ContextID != name ||
			session.Workdir != c.Workdir || session.CreatedBy != domain.SessionCreatedByCompanionStartup || session.CurrentRun == nil ||
			session.CurrentRun.ProfileName != c.Variant || session.CurrentRun.ProfileSnapshot.Agent != string(adapter.kind) {
			t.Fatalf("%s session = %+v", name, session)
		}
		launch, ok := adapter.launch(c.SessionID)
		if !ok {
			t.Fatalf("%s was not launched by its %s adapter", name, adapter.kind)
		}
		if launch.Instructions != system {
			t.Fatalf("%s instructions are not the approved companion SYSTEM text", name)
		}
		if _, err := time.Parse(time.RFC3339, strings.TrimPrefix(launch.Prompt, "Session started: ")); err != nil || strings.Contains(launch.Prompt, "\n") {
			t.Fatalf("%s kickoff is not date-only: %q", name, launch.Prompt)
		}
		if len(launch.AdditionalWorkdirs) != 0 {
			t.Fatalf("%s additional workdirs = %v", name, launch.AdditionalWorkdirs)
		}
		hiveryn := launch.MCPServers[0]
		if hiveryn.Env["HIVERYN_SESSION_TYPE"] != "companion" || hiveryn.Env["HIVERYN_SESSION_ID"] != c.SessionID {
			t.Fatalf("%s MCP env = %v", name, hiveryn.Env)
		}
	}
	if c := f.record(t, "broken"); c.Status != domain.CompanionStartFailed || !strings.Contains(c.Error, "claude and codex") || c.SessionID != "" {
		t.Fatalf("broken = %+v", c)
	}
	if c := f.record(t, "missing"); c.Status != domain.CompanionStartFailed || !strings.Contains(c.Error, "not a configured agent variant") {
		t.Fatalf("missing = %+v", c)
	}
	startup := f.messagesTo(t, f.architect.ID, domain.AgentMessageCompanionStartup)
	if len(startup) != 1 || startup[0].State != domain.AgentMessagePending {
		t.Fatalf("startup messages = %+v", startup)
	}
	for _, want := range []string{"- browser: started", "- research: started", "- broken: failed to start", "- missing: failed to start"} {
		if !strings.Contains(startup[0].Body, want) {
			t.Fatalf("startup body lacks %q:\n%s", want, startup[0].Body)
		}
	}
	// A second start of the same architect session starts nothing again.
	if again := f.start(t); len(again) != 4 || len(f.messagesTo(t, f.architect.ID, domain.AgentMessageCompanionStartup)) != 1 {
		t.Fatalf("restart of startup duplicated companions or reports")
	}
	// Configuration edits apply to later sessions: the listing keeps this
	// session's snapshot.
	conf := f.service.cfg.Architects["hiveryn"]
	conf.Companions = nil
	f.service.cfg.Architects["hiveryn"] = conf
	list, err := f.service.ListArchitectCompanions(context.Background(), "hiveryn")
	if err != nil || list.ArchitectSessionID != f.architect.ID || len(list.Companions) != 4 {
		t.Fatalf("list = %+v %v", list, err)
	}
	if b := list.Companions[0]; b.Name != "broken" || b.PendingActions == nil {
		t.Fatalf("listing order/shape: %+v", b)
	}
}

// The architect's own launch is what starts its companions.
func TestArchitectLaunchStartsCompanions(t *testing.T) {
	f := newCompanionFixture(t, twoCompanions())
	ctx := context.Background()
	architect, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{ArchitectKey: "other", SessionType: domain.SessionTypeArchitect, ContextID: "2026-10-30-0500", Prompt: "kickoff", Workdir: f.workspace, CreatedBy: domain.SessionCreatedByDesktop})
	if err != nil {
		t.Fatal(err)
	}
	f.service.cfg.Architects["other"] = f.service.cfg.Architects["hiveryn"]
	if _, err := f.service.CreateRun(ctx, architect.ID, domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
		t.Fatalf("architect launch: %v", err)
	}
	f.service.companionRT.wg.Wait()
	records, err := f.companions.ListCompanions(ctx, architect.ID)
	if err != nil || len(records) != 2 || records[0].Status != domain.CompanionRunning || records[1].Status != domain.CompanionRunning {
		t.Fatalf("records = %+v %v", records, err)
	}
}

// A launch that fails after its session was created leaves nothing behind.
func TestCompanionPartialStartFailureCleansUp(t *testing.T) {
	f := newCompanionFixture(t, twoCompanions())
	f.claude.fail = errors.New("provider refused")
	f.start(t)
	browser := f.record(t, "browser")
	if browser.Status != domain.CompanionStartFailed || !strings.Contains(browser.Error, "provider refused") || browser.SessionID != "" {
		t.Fatalf("browser = %+v", browser)
	}
	sessions, _ := f.sessions.ListSessions(context.Background())
	companionSessions := 0
	for _, s := range sessions {
		if s.SessionType == domain.SessionTypeCompanion {
			companionSessions++
		}
	}
	if companionSessions != 1 || f.record(t, "research").Status != domain.CompanionRunning {
		t.Fatalf("companion sessions = %d, research = %+v", companionSessions, f.record(t, "research"))
	}
	// A failed start does not block the conclusion; the running one does.
	err := f.service.companionConclusionConflict(context.Background(), f.architect.ID)
	if err == nil || !strings.Contains(err.Error(), "research (running)") || strings.Contains(err.Error(), "browser") {
		t.Fatalf("conclusion guard = %v", err)
	}
}

func TestCompanionMessagesRouteByOwnershipAndAreQueuedDurably(t *testing.T) {
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	browser, research := f.record(t, "browser"), f.record(t, "research")

	sent, err := f.service.SendMessageToCompanion(ctx, f.architect.ID, "browser", domain.CompanionMessageRequest{Message: "Check the login page."})
	if err != nil || sent.Status != domain.CompanionAccepted {
		t.Fatalf("send = %+v %v", sent, err)
	}
	m, err := f.messages.GetAgentMessage(ctx, sent.MessageID)
	if err != nil || m.State != domain.AgentMessagePending || m.Kind != domain.AgentMessageArchitectMessage ||
		m.Recipient != "session:"+browser.SessionID || !strings.Contains(m.Body, "message_id "+sent.MessageID) || !strings.Contains(m.Body, "Check the login page.") {
		t.Fatalf("queued = %+v %v", m, err)
	}

	// The companion replies to that message; the reply goes to its architect.
	reply, err := f.service.ReplyToArchitectMessage(ctx, browser.SessionID, domain.CompanionReplyRequest{MessageID: sent.MessageID, Message: "Login works."})
	if err != nil || reply.InReplyTo != sent.MessageID {
		t.Fatalf("reply = %+v %v", reply, err)
	}
	if m, _ := f.messages.GetAgentMessage(ctx, reply.MessageID); m.Recipient != "session:"+f.architect.ID || m.Kind != domain.AgentMessageCompanionMessage || m.Payload["in_reply_to"] != sent.MessageID {
		t.Fatalf("reply queued = %+v", m)
	}
	// Another companion cannot reply to a message it did not receive.
	if _, err := f.service.ReplyToArchitectMessage(ctx, research.SessionID, domain.CompanionReplyRequest{MessageID: sent.MessageID, Message: "x"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("cross-companion reply = %v", err)
	}
	// A companion message id is not an architect message.
	if _, err := f.service.ReplyToArchitectMessage(ctx, browser.SessionID, domain.CompanionReplyRequest{MessageID: reply.MessageID, Message: "x"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("reply to own message = %v", err)
	}

	// The architect replies to a companion message; the target is derived.
	news, err := f.service.SendMessageToArchitect(ctx, research.SessionID, domain.CompanionMessageRequest{Message: "Found it."})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := f.messages.GetAgentMessage(ctx, news.MessageID); m.Recipient != "session:"+f.architect.ID || !strings.Contains(m.Body, "From companion research") {
		t.Fatalf("companion message = %+v", m)
	}
	answer, err := f.service.ReplyToCompanionMessage(ctx, f.architect.ID, domain.CompanionReplyRequest{MessageID: news.MessageID, Message: "Thanks."})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := f.messages.GetAgentMessage(ctx, answer.MessageID); m.Recipient != "session:"+research.SessionID || m.Payload["in_reply_to"] != news.MessageID {
		t.Fatalf("architect reply = %+v", m)
	}

	// Another architect session cannot reply to this session's messages or
	// address its companions.
	otherArchitect, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{ArchitectKey: "other", SessionType: domain.SessionTypeArchitect, ContextID: "x", Prompt: "k", Workdir: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: otherArchitect.ID, ProfileName: "codex", ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"}, Workdir: f.workspace}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ReplyToCompanionMessage(ctx, otherArchitect.ID, domain.CompanionReplyRequest{MessageID: news.MessageID, Message: "x"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("cross-owner reply = %v", err)
	}
	if _, err := f.service.SendMessageToCompanion(ctx, otherArchitect.ID, "browser", domain.CompanionMessageRequest{Message: "x"}); !errors.As(err, new(*domain.NotFoundError)) || !strings.Contains(err.Error(), "no companions") {
		t.Fatalf("cross-owner send = %v", err)
	}
	// Unknown names list the session's companions; roles are enforced.
	if _, err := f.service.SendMessageToCompanion(ctx, f.architect.ID, "nobody", domain.CompanionMessageRequest{Message: "x"}); err == nil || !strings.Contains(err.Error(), "browser, research") {
		t.Fatalf("unknown companion = %v", err)
	}
	if _, err := f.service.SendMessageToCompanion(ctx, browser.SessionID, "research", domain.CompanionMessageRequest{Message: "x"}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("companion as architect = %v", err)
	}
	if _, err := f.service.SendMessageToArchitect(ctx, f.architect.ID, domain.CompanionMessageRequest{Message: "x"}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("architect as companion = %v", err)
	}
	if _, err := f.service.SendMessageToArchitect(ctx, browser.SessionID, domain.CompanionMessageRequest{Message: "  "}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("empty message = %v", err)
	}
}

func shutdownID() string { return uuid.NewString() }

func TestCompanionShutdownIsBoundIdempotentAndConfirmed(t *testing.T) {
	companionStopGrace = 0
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	browser, research := f.record(t, "browser"), f.record(t, "research")

	if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "browser", RequestID: "not-a-uuid"}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("bad id = %v", err)
	}
	id := shutdownID()
	res, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "browser", RequestID: strings.ToUpper(id)})
	if err != nil || res.RequestID != id || res.Status != domain.CompanionAccepted {
		t.Fatalf("request = %+v %v", res, err)
	}
	instruction := f.messagesTo(t, browser.SessionID, domain.AgentMessageCompanionShutdownRequested)
	if len(instruction) != 1 || instruction[0].State != domain.AgentMessagePending || !strings.Contains(instruction[0].Body, "request_id: "+id) {
		t.Fatalf("instruction = %+v", instruction)
	}
	if c := f.record(t, "browser"); c.Status != domain.CompanionShutdownRequested {
		t.Fatalf("browser = %+v", c)
	}
	// A repeat returns the recorded acceptance without queueing again.
	if again, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "browser", RequestID: id}); err != nil || again != res {
		t.Fatalf("repeat = %+v %v", again, err)
	}
	if n := len(f.messagesTo(t, browser.SessionID, domain.AgentMessageCompanionShutdownRequested)); n != 1 {
		t.Fatalf("instructions after repeat = %d", n)
	}
	// The same id for another target is an error; another id for a
	// companion already shutting down is a conflict.
	if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "research", RequestID: id}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("reused id = %v", err)
	}
	if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "browser", RequestID: shutdownID()}); err == nil || !strings.Contains(err.Error(), "already shutting down under request_id "+id) {
		t.Fatalf("second id = %v", err)
	}
	// No new assignments while shutting down; replies to its messages still
	// reach it until it stops.
	if _, err := f.service.SendMessageToCompanion(ctx, f.architect.ID, "browser", domain.CompanionMessageRequest{Message: "x"}); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("assignment while shutting down = %v", err)
	}

	// Only this companion's request completes it.
	if _, err := f.service.CompleteCompanionShutdown(ctx, research.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: id, Handoff: "x"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("foreign completion = %v", err)
	}
	if _, err := f.service.CompleteCompanionShutdown(ctx, browser.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: shutdownID(), Handoff: "x"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("arbitrary id = %v", err)
	}
	if _, err := f.service.CompleteCompanionShutdown(ctx, research.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: shutdownID(), Handoff: "x"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("completion without a request = %v", err)
	}
	if err := f.service.companionConclusionConflict(ctx, f.architect.ID); err == nil {
		t.Fatal("conclusion allowed while companions run")
	}

	done, err := f.service.CompleteCompanionShutdown(ctx, browser.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: id, Handoff: "Results in runs/x. Nothing in progress."})
	if err != nil || done.RequestID != id || done.Status != domain.CompanionAccepted {
		t.Fatalf("complete = %+v %v", done, err)
	}
	// The acknowledgement comes first; termination follows.
	f.service.companionRT.wg.Wait()
	if f.sessionExists(t, browser.SessionID) {
		t.Fatal("companion session not removed after confirmed termination")
	}
	if c := f.record(t, "browser"); c.Status != domain.CompanionStopped {
		t.Fatalf("browser after stop = %+v", c)
	}
	sd, err := f.companions.GetShutdown(ctx, id)
	if err != nil || sd.Status != domain.ShutdownStopped || sd.StoppedAt == nil || sd.Handoff != "Results in runs/x. Nothing in progress." {
		t.Fatalf("shutdown record = %+v %v", sd, err)
	}
	outcome := f.messagesTo(t, f.architect.ID, domain.AgentMessageCompanionShutdownOutcome)
	if len(outcome) != 1 || !strings.Contains(outcome[0].Body, "request_id "+id) || !strings.Contains(outcome[0].Body, "termination confirmed") || !strings.Contains(outcome[0].Body, "Results in runs/x") {
		t.Fatalf("outcome = %+v", outcome)
	}
	// Repeating the completion returns the recorded acceptance once the
	// session still exists to call; a changed handoff is refused. (The
	// session is gone here, so the call cannot even be made.)
	if _, err := f.service.CompleteCompanionShutdown(ctx, browser.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: id, Handoff: "x"}); err == nil {
		t.Fatal("completion from an ended session accepted")
	}

	// The architect cannot conclude until research stops too.
	err = f.service.companionConclusionConflict(ctx, f.architect.ID)
	if err == nil || !strings.Contains(err.Error(), "research (running)") || strings.Contains(err.Error(), "browser") {
		t.Fatalf("conclusion guard = %v", err)
	}
}

// Completing twice while termination is pending: the same handoff is the
// recorded acceptance, a different one an error.
func TestCompanionShutdownCompletionRepeat(t *testing.T) {
	companionStopGrace = 200 * time.Millisecond
	t.Cleanup(func() { companionStopGrace = 0 })
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	browser := f.record(t, "browser")
	id := shutdownID()
	if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "browser", RequestID: id}); err != nil {
		t.Fatal(err)
	}
	req := domain.CompleteCompanionShutdownRequest{RequestID: id, Handoff: "done"}
	if _, err := f.service.CompleteCompanionShutdown(ctx, browser.SessionID, req); err != nil {
		t.Fatal(err)
	}
	if res, err := f.service.CompleteCompanionShutdown(ctx, browser.SessionID, req); err != nil || res.RequestID != id {
		t.Fatalf("repeat = %+v %v", res, err)
	}
	if _, err := f.service.CompleteCompanionShutdown(ctx, browser.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: id, Handoff: "other"}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("changed handoff = %v", err)
	}
	f.service.companionRT.wg.Wait()
	if n := len(f.messagesTo(t, f.architect.ID, domain.AgentMessageCompanionShutdownOutcome)); n != 1 {
		t.Fatalf("outcome messages = %d", n)
	}
}

func TestArchitectConclusionBlockedUntilCompanionsStop(t *testing.T) {
	companionStopGrace = 0
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	params := domain.ConcludeSessionParams{Summary: "s", Narrative: "n", NextSteps: "None"}
	if _, err := f.service.ConcludeSession(ctx, f.architect.ID, params); err == nil || !strings.Contains(err.Error(), "companion(s) not stopped") {
		t.Fatalf("conclude with running companions = %v", err)
	}
	for _, name := range []string{"browser", "research"} {
		c := f.record(t, name)
		id := shutdownID()
		if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: name, RequestID: id}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.CompleteCompanionShutdown(ctx, c.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: id, Handoff: "h"}); err != nil {
			t.Fatal(err)
		}
	}
	f.service.companionRT.wg.Wait()
	if _, err := f.service.ConcludeSession(ctx, f.architect.ID, params); err != nil {
		t.Fatalf("conclude after companions stopped: %v", err)
	}
	if records, err := f.companions.ListCompanions(ctx, f.architect.ID); err != nil || len(records) != 0 {
		t.Fatalf("records after conclusion = %+v %v", records, err)
	}
}

type actionOutcome struct {
	res domain.RequestActionResult
	err error
}

func (f *companionFixture) requestAction(t *testing.T, sessionID string) (domain.ActionRequest, <-chan actionOutcome) {
	t.Helper()
	done := make(chan actionOutcome, 1)
	go func() {
		res, err := f.service.RequestAction(context.Background(), sessionID, domain.RequestActionRequest{Title: "Create three Jira tickets", Description: "Create ABC-1..3 with ..."})
		done <- actionOutcome{res, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending := f.service.actions.pendingFor(sessionID); len(pending) > 0 {
			return pending[0], done
		}
		select {
		case out := <-done:
			t.Fatalf("requestAction resolved before it was pending: %+v %v", out.res, out.err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	t.Fatal("action request never pending")
	return domain.ActionRequest{}, nil
}

func waitAction(t *testing.T, done <-chan actionOutcome) actionOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("requestAction did not return")
		return actionOutcome{}
	}
}

func TestRequestActionDecisions(t *testing.T) {
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	browser, research := f.record(t, "browser"), f.record(t, "research")

	for _, tc := range []struct {
		decision domain.ActionDecisionRequest
		want     domain.RequestActionResult
	}{
		{domain.ActionDecisionRequest{Decision: domain.ActionDecisionAccept}, domain.RequestActionResult{Outcome: "accepted"}},
		{domain.ActionDecisionRequest{Decision: domain.ActionDecisionDeny}, domain.RequestActionResult{Outcome: "denied"}},
		{domain.ActionDecisionRequest{Decision: domain.ActionDecisionReply, Message: "Use project XYZ."}, domain.RequestActionResult{Outcome: "replied", Message: "Use project XYZ."}},
	} {
		request, done := f.requestAction(t, browser.SessionID)
		if request.Origin.Companion != "browser" || request.Origin.ArchitectSessionID != f.architect.ID {
			t.Fatalf("origin = %+v", request.Origin)
		}
		// Another session's decision route does not reach it.
		if _, err := f.service.DecideAction(ctx, research.SessionID, request.ID, tc.decision); !errors.As(err, new(*domain.NotFoundError)) {
			t.Fatalf("foreign decision = %v", err)
		}
		if _, err := f.service.DecideAction(ctx, browser.SessionID, request.ID, tc.decision); err != nil {
			t.Fatal(err)
		}
		if out := waitAction(t, done); out.err != nil || out.res != tc.want {
			t.Fatalf("%s: %+v %v", tc.decision.Decision, out.res, out.err)
		}
		// The first decision wins.
		if _, err := f.service.DecideAction(ctx, browser.SessionID, request.ID, domain.ActionDecisionRequest{Decision: domain.ActionDecisionAccept}); !errors.As(err, new(*domain.ConflictError)) {
			t.Fatalf("second decision = %v", err)
		}
	}
	if sent := f.publisher.sent(); len(sent) != 3 || !strings.Contains(sent[0].Title, "browser") || !strings.Contains(sent[0].Body, "Create three Jira tickets") {
		t.Fatalf("phone alerts = %+v", sent)
	}
	if _, err := (domain.ActionDecisionRequest{Decision: domain.ActionDecisionReply}).Normalize(); err == nil {
		t.Fatal("reply without a message accepted")
	}
	if _, err := f.service.RequestAction(ctx, f.architect.ID, domain.RequestActionRequest{Title: "t", Description: "d"}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("architect requestAction = %v", err)
	}
	events, _ := f.sessions.ListSessionEvents(ctx, browser.SessionID)
	required, resolved := 0, 0
	for _, e := range events {
		if e.Type != domain.SessionEventTypeActionRequest {
			continue
		}
		switch e.Status {
		case "required":
			required++
		case "resolved":
			resolved++
		}
	}
	if required != 3 || resolved != 3 {
		t.Fatalf("action events: required=%d resolved=%d", required, resolved)
	}
}

func TestRequestActionTimeoutNotificationFailureAndShutdownCancellation(t *testing.T) {
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	browser := f.record(t, "browser")

	f.service.actionTimeout = 50 * time.Millisecond
	_, done := f.requestAction(t, browser.SessionID)
	if out := waitAction(t, done); out.err != nil || out.res.ErrorCode != domain.ActionRequestTimeoutCode || out.res.Error != domain.ActionRequestTimeoutText {
		t.Fatalf("timeout = %+v %v", out.res, out.err)
	}
	f.service.actionTimeout = 0

	f.publisher.err = errors.New("ntfy 500")
	res, err := f.service.RequestAction(ctx, browser.SessionID, domain.RequestActionRequest{Title: "t", Description: "d"})
	if err != nil || res.ErrorCode != domain.ActionRequestNotificationFailedCode || len(f.service.actions.pendingFor(browser.SessionID)) != 0 {
		t.Fatalf("notification failure = %+v %v", res, err)
	}
	f.publisher.err = nil

	request, done := f.requestAction(t, browser.SessionID)
	if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "browser", RequestID: shutdownID()}); err != nil {
		t.Fatal(err)
	}
	out := waitAction(t, done)
	if out.err != nil || out.res.ErrorCode != domain.ActionRequestCancelledCode || !strings.Contains(out.res.Error, "completeCompanionShutdown") {
		t.Fatalf("shutdown cancellation = %+v %v", out.res, out.err)
	}
	if _, err := f.service.DecideAction(ctx, browser.SessionID, request.ID, domain.ActionDecisionRequest{Decision: domain.ActionDecisionAccept}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("accepting a withdrawn request = %v", err)
	}
	// While shutting down, no new requests are raised.
	if res, err := f.service.RequestAction(ctx, browser.SessionID, domain.RequestActionRequest{Title: "t", Description: "d"}); err != nil || res.ErrorCode != domain.ActionRequestCancelledCode {
		t.Fatalf("request while shutting down = %+v %v", res, err)
	}
}

// A restart resolves an orphaned request, settles an interrupted launch and
// finishes a termination whose handoff was recorded.
func TestCompanionRecoveryAfterRestart(t *testing.T) {
	companionStopGrace = time.Hour // the stop never runs before the "crash"
	t.Cleanup(func() { companionStopGrace = 0 })
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	browser, research := f.record(t, "browser"), f.record(t, "research")

	_, _ = f.requestAction(t, research.SessionID)
	id := shutdownID()
	if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: "browser", RequestID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CompleteCompanionShutdown(ctx, browser.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: id, Handoff: "h"}); err != nil {
		t.Fatal(err)
	}
	// A companion whose launch the restart interrupts.
	interrupted := domain.Companion{ArchitectKey: "hiveryn", ArchitectSessionID: f.architect.ID, Name: "late", Repo: "alpha", Workdir: f.repos["alpha"], Variant: "claude", Status: domain.CompanionStarting, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := f.companions.CreateCompanion(ctx, interrupted); err != nil {
		t.Fatal(err)
	}

	f.reopen(t)
	if err := f.service.ReconcileCompanions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileActions(ctx); err != nil {
		t.Fatal(err)
	}
	if c := f.record(t, "browser"); c.Status != domain.CompanionStopped || f.sessionExists(t, browser.SessionID) {
		t.Fatalf("browser after restart = %+v", c)
	}
	if c := f.record(t, "late"); c.Status != domain.CompanionStartFailed || !strings.Contains(c.Error, "restarted") {
		t.Fatalf("late after restart = %+v", c)
	}
	if c := f.record(t, "research"); c.Status != domain.CompanionRunning {
		t.Fatalf("research after restart = %+v", c)
	}
	events, _ := f.sessions.ListSessionEvents(ctx, research.SessionID)
	interruptedAction := false
	for _, e := range events {
		if e.Type == domain.SessionEventTypeActionRequest && e.Status == "resolved" && e.Raw["status"] == string(domain.ActionInterrupted) {
			interruptedAction = true
		}
	}
	if !interruptedAction {
		t.Fatal("orphaned action request not resolved as interrupted")
	}
	if outcome := f.messagesTo(t, f.architect.ID, domain.AgentMessageCompanionShutdownOutcome); len(outcome) != 1 {
		t.Fatalf("shutdown outcome after restart = %+v", outcome)
	}
	// The interrupted launch is reported (the original report was already
	// sent, so the dedupe keeps one startup message).
	if startup := f.messagesTo(t, f.architect.ID, domain.AgentMessageCompanionStartup); len(startup) != 1 {
		t.Fatalf("startup messages = %d", len(startup))
	}
}

// A local companion whose process ends and cannot be resumed is reported
// failed and no longer blocks the conclusion.
func TestCompanionRunFailureIsReported(t *testing.T) {
	f := newCompanionFixture(t, twoCompanions())
	f.start(t)
	ctx := context.Background()
	research := f.record(t, "research")
	session, _ := f.sessions.GetSession(ctx, research.SessionID)
	f.service.markRunFailed(session.CurrentRun.ID, domain.SessionRunFailureLaunchFailed, "test")
	f.service.companionRunFailed(ctx, session, "its process ended and could not be resumed: gone")
	if c := f.record(t, "research"); c.Status != domain.CompanionFailed {
		t.Fatalf("research = %+v", c)
	}
	if failed := f.messagesTo(t, f.architect.ID, domain.AgentMessageCompanionFailed); len(failed) != 1 || !strings.Contains(failed[0].Body, "research") {
		t.Fatalf("failure notice = %+v", failed)
	}
	if err := f.service.DiscardCompanion(ctx, f.record(t, "browser").SessionID); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("discarding a running companion = %v", err)
	}
	if err := f.service.DiscardCompanion(ctx, research.SessionID); err != nil || f.sessionExists(t, research.SessionID) {
		t.Fatalf("discard failed companion = %v", err)
	}
}

// The companion instructions are the text Kareem approved
// (architect:drafts/companion-SYSTEM.md), byte for byte; a change needs his
// review first.
func TestCompanionSystemPromptIsTheApprovedText(t *testing.T) {
	raw, err := promptFS.ReadFile(companionSystemPromptName)
	if err != nil {
		t.Fatal(err)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(raw)); sum != "2d7e069db7fb620d2cd1a9ec70069ffb5a6b4cfd39db4bc6e630e131c5d6261c" {
		t.Fatalf("companion SYSTEM.md sha256 = %s, not the approved draft", sum)
	}
	kickoff, err := promptFS.ReadFile(companionKickoffPromptName)
	if err != nil || strings.TrimSpace(string(kickoff)) != "Session started: {{.CurrentDate}}" {
		t.Fatalf("companion kickoff = %q %v", kickoff, err)
	}
}
