package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/ntfy"
)

type fakePublisher struct {
	mu       sync.Mutex
	messages []ntfy.Message
	err      error
}

func (f *fakePublisher) Publish(_ context.Context, msg ntfy.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, msg)
	return nil
}

func (f *fakePublisher) Server() string { return "https://ntfy.example" }

func (f *fakePublisher) sent() []ntfy.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ntfy.Message(nil), f.messages...)
}

func newQuestionService(t *testing.T, session domain.Session) (*Service, *fakeSessionRepository, *fakePublisher) {
	t.Helper()
	repo := newFakeSessionRepository()
	repo.createdSession = session
	publisher := &fakePublisher{}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		cfg: config.Config{Architects: map[string]config.ArchitectConfig{
			"hiveryn": {Name: "Hiveryn", Path: t.TempDir()},
		}},
		intents:      newIntentStore(),
		questions:    newQuestionStore(),
		notifier:     publisher,
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}
	return service, repo, publisher
}

func runningArchitect() domain.Session {
	return domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
}

func idx(v int) *int { return &v }

type askOutcome struct {
	res domain.AskQuestionResult
	err error
}

// ask starts a blocking AskQuestion and returns once the question is pending.
func ask(t *testing.T, ctx context.Context, service *Service, repo *fakeSessionRepository) (string, <-chan askOutcome) {
	t.Helper()
	before := len(questionEvents(repo, sessionEventStatusReqd))
	out := make(chan askOutcome, 1)
	go func() {
		res, err := service.AskQuestion(ctx, "session-1", domain.AskQuestionRequest{
			Question:         "Deploy to staging first?",
			Answers:          []string{"Yes, staging first", "No, straight to prod"},
			RecommendedIndex: idx(0),
		})
		out <- askOutcome{res, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if required := questionEvents(repo, sessionEventStatusReqd); len(required) > before {
			return required[len(required)-1].Raw["question_id"].(string), out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("question never became pending")
	return "", nil
}

func questionEvents(repo *fakeSessionRepository, status string) []domain.AppendSessionEventParams {
	var out []domain.AppendSessionEventParams
	for _, e := range repo.events() {
		if e.Type == sessionEventTypeQuestion && e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

func waitOutcome(t *testing.T, ch <-chan askOutcome) askOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("AskQuestion did not return")
		return askOutcome{}
	}
}

func TestAskQuestionReturnsTheDesktopAnswer(t *testing.T) {
	service, repo, publisher := newQuestionService(t, runningArchitect())
	id, out := ask(t, context.Background(), service, repo)

	required := questionEvents(repo, sessionEventStatusReqd)[0]
	if required.Raw["recommended_index"] != 0 || required.Raw["question"] != "Deploy to staging first?" {
		t.Fatalf("required event raw = %#v", required.Raw)
	}
	sent := publisher.sent()
	if len(sent) != 1 || sent[0].Title != "Hiveryn · architect" || sent[0].Priority != ntfy.PriorityHigh ||
		!strings.Contains(sent[0].Body, "1. Yes, staging first (recommended)") || !strings.Contains(sent[0].Body, "2. No, straight to prod\n") {
		t.Fatalf("phone alert = %#v", sent)
	}

	// Another session cannot answer it.
	if _, err := service.AnswerQuestion(context.Background(), "session-2", id, "Yes"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("cross-session answer err = %v", err)
	}
	final, err := service.AnswerQuestion(context.Background(), "session-1", id, "  Free text: staging, then wait for me  ")
	if err != nil || final.Status != domain.QuestionAnswered {
		t.Fatalf("AnswerQuestion = %+v, %v", final, err)
	}
	o := waitOutcome(t, out)
	if o.err != nil || o.res.Text != "Free text: staging, then wait for me" || o.res.Status != domain.QuestionAnswered {
		t.Fatalf("AskQuestion = %+v, %v", o.res, o.err)
	}

	// Duplicate answers are rejected and say why.
	_, err = service.AnswerQuestion(context.Background(), "session-1", id, "Yes, staging first")
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "already answered") {
		t.Fatalf("duplicate answer err = %v", err)
	}
	resolved := questionEvents(repo, sessionEventStatusResolvd)
	if len(resolved) != 1 || resolved[0].Raw["status"] != "answered" || resolved[0].Raw["answer"] != "Free text: staging, then wait for me" {
		t.Fatalf("resolved events = %#v", resolved)
	}
}

func TestAskQuestionExpiresServerSide(t *testing.T) {
	service, repo, _ := newQuestionService(t, runningArchitect())
	service.questionTimeout = 50 * time.Millisecond
	// The caller's context never ends: expiry must not depend on the client.
	id, out := ask(t, context.Background(), service, repo)

	o := waitOutcome(t, out)
	if o.err != nil || o.res.Text != "User didn't respond within 1 hour, stop here and wait for user to get back" || o.res.Status != domain.QuestionExpired {
		t.Fatalf("expired AskQuestion = %+v, %v", o.res, o.err)
	}
	_, err := service.AnswerQuestion(context.Background(), "session-1", id, "late")
	if !errors.As(err, new(*domain.ConflictError)) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("late answer err = %v", err)
	}
}

func TestAskQuestionAbandonedByTheAgentIsNoLongerAnswerable(t *testing.T) {
	service, repo, _ := newQuestionService(t, runningArchitect())
	ctx, cancel := context.WithCancel(context.Background())
	id, out := ask(t, ctx, service, repo)
	cancel()

	if o := waitOutcome(t, out); o.err == nil {
		t.Fatalf("abandoned AskQuestion returned %+v", o.res)
	}
	resolved := questionEvents(repo, sessionEventStatusResolvd)
	if len(resolved) != 1 || resolved[0].Raw["status"] != "cancelled" {
		t.Fatalf("resolved events = %#v", resolved)
	}
	if _, err := service.AnswerQuestion(context.Background(), "session-1", id, "too late"); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("answer to abandoned question err = %v", err)
	}
}

func TestSessionEndAndShutdownResolvePendingQuestions(t *testing.T) {
	service, repo, _ := newQuestionService(t, runningArchitect())

	_, out := ask(t, context.Background(), service, repo)
	service.cancelSessionQuestions(context.Background(), "session-1")
	if o := waitOutcome(t, out); o.err != nil || o.res.Status != domain.QuestionCancelled || !strings.Contains(o.res.Text, "Stop here and wait for the user") {
		t.Fatalf("session-end AskQuestion = %+v, %v", o.res, o.err)
	}

	_, out = ask(t, context.Background(), service, repo)
	service.interruptQuestions(context.Background())
	if o := waitOutcome(t, out); o.err != nil || o.res.Status != domain.QuestionInterrupted {
		t.Fatalf("shutdown AskQuestion = %+v, %v", o.res, o.err)
	}
	if got := len(questionEvents(repo, sessionEventStatusResolvd)); got != 2 {
		t.Fatalf("resolved events = %d, want 2", got)
	}
}

func TestOneResolutionWinsAcrossConcurrentAnswers(t *testing.T) {
	service, repo, _ := newQuestionService(t, runningArchitect())
	id, out := ask(t, context.Background(), service, repo)

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := service.AnswerQuestion(context.Background(), "session-1", id, string(rune('a'+i))); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	service.cancelSessionQuestions(context.Background(), "session-1")
	wg.Wait()
	o := waitOutcome(t, out)
	if got := len(questionEvents(repo, sessionEventStatusResolvd)); got != 1 {
		t.Fatalf("resolved events = %d, want exactly 1", got)
	}
	if (o.res.Status == domain.QuestionAnswered) != (wins == 1) || wins > 1 {
		t.Fatalf("status %s with %d winning answers", o.res.Status, wins)
	}
}

func TestFailedPhoneAlertAsksNothing(t *testing.T) {
	service, repo, publisher := newQuestionService(t, runningArchitect())
	publisher.err = errors.New("ntfy server https://ntfy.example refused the notification: HTTP 403")

	_, err := service.AskQuestion(context.Background(), "session-1", domain.AskQuestionRequest{Question: "q", Answers: []string{"a", "b"}, RecommendedIndex: idx(1)})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "NOT asked") {
		t.Fatalf("AskQuestion err = %v", err)
	}
	if len(questionEvents(repo, sessionEventStatusReqd)) != 0 {
		t.Fatal("a question was raised although the phone was never alerted")
	}

	if _, err := service.Notify(context.Background(), "session-1", "done"); err == nil || !strings.Contains(err.Error(), "NOT notified") {
		t.Fatalf("Notify err = %v", err)
	}
	events := repo.events()
	if last := events[len(events)-1]; last.Type != sessionEventTypeNotification || last.Status != "failed" {
		t.Fatalf("notification event = %#v", last)
	}
}

func TestNotifyAndAskWithoutConfigurationFail(t *testing.T) {
	service, repo, _ := newQuestionService(t, runningArchitect())
	service.SetNotificationPublisher(nil)

	if _, err := service.Notify(context.Background(), "session-1", "done"); !errors.As(err, new(*domain.ConflictError)) || !strings.Contains(err.Error(), "notifications.ntfy") {
		t.Fatalf("Notify err = %v", err)
	}
	if _, err := service.AskQuestion(context.Background(), "session-1", domain.AskQuestionRequest{Question: "q", Answers: []string{"a", "b"}, RecommendedIndex: idx(0)}); err == nil {
		t.Fatal("AskQuestion succeeded without a phone alert")
	}
	if len(questionEvents(repo, sessionEventStatusReqd)) != 0 {
		t.Fatal("a question was raised without notifications configured")
	}
}

func TestNotifySendsAndValidates(t *testing.T) {
	session := runningArchitect()
	session.SessionType = domain.SessionTypeAction
	session.ArchitectKey = ""
	session.ContextID = "0f3c9a1e-5d4b-4c1a-9a7e-123456789abc"
	service, _, publisher := newQuestionService(t, session)

	res, err := service.Notify(context.Background(), "session-1", "  Package delivered  ")
	if err != nil || res.Text != "Notification sent to user" {
		t.Fatalf("Notify = %+v, %v", res, err)
	}
	if sent := publisher.sent(); len(sent) != 1 || sent[0].Body != "Package delivered" || sent[0].Title != "Action · 0f3c9a1e" {
		t.Fatalf("phone alert = %#v", sent)
	}
	if _, err := service.Notify(context.Background(), "session-1", strings.Repeat("x", 501)); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("over-long notify err = %v", err)
	}
	if _, err := service.AskQuestion(context.Background(), "session-1", domain.AskQuestionRequest{Question: "q", Answers: []string{"a", "b"}, RecommendedIndex: idx(2)}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("out-of-range recommendation err = %v", err)
	}
}

func TestQuestionsRequireARunningSession(t *testing.T) {
	session := runningArchitect()
	session.CurrentRun = nil
	service, _, _ := newQuestionService(t, session)
	if _, err := service.Notify(context.Background(), "session-1", "x"); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("Notify on a stopped session err = %v", err)
	}
}

func TestReconcileQuestionsInterruptsOrphans(t *testing.T) {
	service, repo, _ := newQuestionService(t, runningArchitect())
	repo.sessionEvents = map[string][]domain.SessionEvent{"session-1": {
		{Type: sessionEventTypeQuestion, Status: sessionEventStatusReqd, Raw: map[string]any{"question_id": "q-open"}},
		{Type: sessionEventTypeQuestion, Status: sessionEventStatusReqd, Raw: map[string]any{"question_id": "q-done"}},
		{Type: sessionEventTypeQuestion, Status: sessionEventStatusResolvd, Raw: map[string]any{"question_id": "q-done", "status": "answered"}},
	}}
	if err := service.ReconcileQuestions(context.Background()); err != nil {
		t.Fatalf("ReconcileQuestions: %v", err)
	}
	resolved := questionEvents(repo, sessionEventStatusResolvd)
	if len(resolved) != 1 || resolved[0].Raw["question_id"] != "q-open" || resolved[0].Raw["status"] != "interrupted" {
		t.Fatalf("reconciled = %#v", resolved)
	}
}

// Every role is told to use notify and askQuestion over native question tools.
func TestSystemPromptsGuideNotifyAndAskQuestion(t *testing.T) {
	for _, name := range []string{architectSystemPromptName, workerSystemPromptName, actionSystemPromptName} {
		text, err := builtinPrompt(name)
		if err != nil {
			t.Fatalf("builtinPrompt(%s): %v", name, err)
		}
		for _, want := range []string{"notify(shortMessage)", "askQuestion(question, answers, recommendedIndex)", "rather than a built-in question tool"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s lacks %q", name, want)
			}
		}
	}
}
