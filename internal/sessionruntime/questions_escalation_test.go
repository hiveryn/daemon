package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// escalationFixture is a question service with a real SQLite outbox, a
// running ticket worker (session-1, the asker) and a running architect
// (arch-1) of the same project, plus an architect of another project.
type escalationFixture struct {
	service   *Service
	repo      *fakeSessionRepository
	publisher *fakePublisher
	messages  *store.AgentMessageStore
}

func newEscalationFixture(t *testing.T, delay time.Duration) *escalationFixture {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	worker := domain.Session{
		ID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "2026-10-23-0412-escalate",
		CurrentRun: &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	architect := domain.Session{
		ID: "arch-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect,
		CurrentRun: &domain.SessionRun{ID: "run-a", Status: domain.SessionRunStatusRunning},
	}
	other := domain.Session{
		ID: "arch-other", ArchitectKey: "other", SessionType: domain.SessionTypeArchitect,
		CurrentRun: &domain.SessionRun{ID: "run-o", Status: domain.SessionRunStatusRunning},
	}
	repo := newFakeSessionRepository()
	repo.listedSessions = []domain.Session{worker, architect, other}
	publisher := &fakePublisher{}
	service := &Service{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:    repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{TicketSummary: domain.TicketSummary{Title: "Escalation ticket"}}},
		cfg: config.Config{Architects: map[string]config.ArchitectConfig{
			"hiveryn": {Name: "Hiveryn", Path: t.TempDir()},
			"other":   {Name: "Other", Path: t.TempDir()},
		}},
		intents:                 newIntentStore(),
		questions:               newQuestionStore(),
		notifier:                publisher,
		eventStreams:            map[string]map[uint64]chan domain.SessionEvent{},
		questionEscalationDelay: delay,
	}
	messages := store.NewAgentMessageStore(db)
	service.SetAgentMessageRepository(messages)
	return &escalationFixture{service: service, repo: repo, publisher: publisher, messages: messages}
}

// waitEscalation polls the outbox for the question's committed escalation
// notice (EnqueueAgentMessage writes it as pending_commit first).
func (f *escalationFixture) waitEscalation(t *testing.T, questionID string) domain.AgentMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, err := f.messages.GetAgentMessageByDedupeKey(context.Background(), questionEscalationDedupeKey(questionID))
		if err == nil && m.State != domain.AgentMessagePendingCommit {
			return m
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("escalation notice never appeared")
	return domain.AgentMessage{}
}

func (f *escalationFixture) escalationExists(questionID string) bool {
	_, err := f.messages.GetAgentMessageByDedupeKey(context.Background(), questionEscalationDedupeKey(questionID))
	return err == nil
}

func TestWorkerQuestionEscalatesToArchitectWhoMayAnswer(t *testing.T) {
	f := newEscalationFixture(t, 30*time.Millisecond)
	id, out := ask(t, context.Background(), f.service, f.repo)

	// The phone alert went first, as before.
	if sent := f.publisher.sent(); len(sent) != 1 || sent[0].Title != "Hiveryn · Escalation ticket" {
		t.Fatalf("phone alert = %#v", sent)
	}
	// Before the delay the architect is told the question is not its to answer.
	early, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "Yes, staging first")
	if err != nil || early.Outcome != domain.ArchitectAnswerNotAnswerable || !strings.Contains(early.Reason, "not been escalated") || early.Question.Status != domain.QuestionPending {
		t.Fatalf("early architect answer = %+v, %v", early, err)
	}

	m := f.waitEscalation(t, id)
	if m.Kind != domain.AgentMessageQuestionEscalated || m.Recipient != "architect:hiveryn" || m.ArchitectKey != "hiveryn" || m.State != domain.AgentMessagePending {
		t.Fatalf("escalation = %+v", m)
	}
	for _, want := range []string{
		"ticket 2026-10-23-0412-escalate", "worker session session-1", "Question id: " + id,
		"Question: Deploy to staging first?", "1. Yes, staging first (recommended); 2. No, straight to prod",
		"The user has not answered this worker question after ", " seconds. If you already know the answer and are confident, you may answer using answerQuestion. Otherwise, no action is required. The user has already been notified and can answer when they return.",
		"The question expires at ", "after that nobody can answer it",
	} {
		if !strings.Contains(m.Body, want) {
			t.Fatalf("escalation body lacks %q:\n%s", want, m.Body)
		}
	}
	if m.Payload["question_id"] != id || m.Payload["ticket_id"] != "2026-10-23-0412-escalate" || m.Payload["session_id"] != "session-1" || m.Payload["expires_at"] == nil {
		t.Fatalf("escalation payload = %#v", m.Payload)
	}
	if _, ok := m.Payload["elapsed_seconds"]; !ok {
		t.Fatalf("escalation payload lacks elapsed_seconds: %#v", m.Payload)
	}

	// Another project's architect cannot see it; a worker cannot answer as an architect.
	if _, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-other", id, "x"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("foreign architect err = %v", err)
	}
	if _, err := f.service.AnswerQuestionAsArchitect(context.Background(), "session-1", id, "x"); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("worker-as-architect err = %v", err)
	}
	if _, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "  "); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("blank architect answer err = %v", err)
	}

	// The owning architect answers; the worker learns who answered.
	res, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, " Yes, staging first ")
	if err != nil || res.Outcome != domain.ArchitectAnswerAnswered || res.Question.AnsweredBy != domain.QuestionAnsweredByArchitect || res.Question.Answer != "Yes, staging first" {
		t.Fatalf("architect answer = %+v, %v", res, err)
	}
	o := waitOutcome(t, out)
	if o.err != nil || o.res.Status != domain.QuestionAnswered || o.res.AnsweredBy != domain.QuestionAnsweredByArchitect ||
		o.res.Text != "Answered by the project architect, not the user: Yes, staging first" {
		t.Fatalf("worker result = %+v, %v", o.res, o.err)
	}
	resolved := questionEvents(f.repo, sessionEventStatusResolvd)
	if len(resolved) != 1 || resolved[0].Raw["answered_by"] != "architect" || resolved[0].Raw["answer"] != "Yes, staging first" {
		t.Fatalf("resolved events = %#v", resolved)
	}

	// Late submissions, from either side, are refused and say who won.
	_, err = f.service.AnswerQuestion(context.Background(), "session-1", id, "No, straight to prod")
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "already answered by the project architect") {
		t.Fatalf("late user answer err = %v", err)
	}
	again, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "No")
	if err != nil || again.Outcome != domain.ArchitectAnswerNotAnswerable || !strings.Contains(again.Reason, "already answered by the project architect") {
		t.Fatalf("duplicate architect answer = %+v, %v", again, err)
	}

	// The notice was never delivered (no dispatcher ran): the next pass
	// withdraws it instead of announcing a settled question.
	if err := f.service.dispatchAgentMessages(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	m, _ = f.messages.GetAgentMessage(context.Background(), m.ID)
	if m.State != domain.AgentMessageFailed || !strings.Contains(m.LastError, "withdrawn") || !strings.Contains(m.LastError, "answered") {
		t.Fatalf("stale escalation = state %s, error %q", m.State, m.LastError)
	}
}

func TestUserAnswerBeatsArchitectAnswer(t *testing.T) {
	f := newEscalationFixture(t, 10*time.Millisecond)
	id, out := ask(t, context.Background(), f.service, f.repo)
	f.waitEscalation(t, id)

	// Both race; exactly one wins and the loser is told which.
	var wg sync.WaitGroup
	var userErr error
	var archRes domain.ArchitectAnswerResult
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, userErr = f.service.AnswerQuestion(context.Background(), "session-1", id, "No, straight to prod")
	}()
	go func() {
		defer wg.Done()
		archRes, _ = f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "Yes, staging first")
	}()
	wg.Wait()
	o := waitOutcome(t, out)
	resolved := questionEvents(f.repo, sessionEventStatusResolvd)
	if len(resolved) != 1 {
		t.Fatalf("resolved events = %d, want exactly 1", len(resolved))
	}
	switch {
	case userErr == nil && archRes.Outcome == domain.ArchitectAnswerNotAnswerable:
		if o.res.AnsweredBy != domain.QuestionAnsweredByUser || o.res.Text != "No, straight to prod" || !strings.Contains(archRes.Reason, "already answered by the user") {
			t.Fatalf("user won but result = %+v, architect = %+v", o.res, archRes)
		}
	case userErr != nil && archRes.Outcome == domain.ArchitectAnswerAnswered:
		if o.res.AnsweredBy != domain.QuestionAnsweredByArchitect || !strings.HasPrefix(o.res.Text, domain.QuestionArchitectAnswerPrefix) || !strings.Contains(userErr.Error(), "project architect") {
			t.Fatalf("architect won but result = %+v, user err = %v", o.res, userErr)
		}
	default:
		t.Fatalf("no single winner: user err %v, architect %+v", userErr, archRes)
	}
}

func TestEscalationDoesNotExtendOrReplaceExpiry(t *testing.T) {
	f := newEscalationFixture(t, 20*time.Millisecond)
	f.service.questionTimeout = 80 * time.Millisecond
	id, out := ask(t, context.Background(), f.service, f.repo)
	f.waitEscalation(t, id)

	o := waitOutcome(t, out)
	if o.err != nil || o.res.Status != domain.QuestionExpired || o.res.Text != domain.QuestionTimeoutText {
		t.Fatalf("expired result = %+v, %v", o.res, o.err)
	}
	res, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "too late")
	if err != nil || res.Outcome != domain.ArchitectAnswerNotAnswerable || !strings.Contains(res.Reason, "expired") {
		t.Fatalf("architect answer after expiry = %+v, %v", res, err)
	}
	if err := f.service.dispatchAgentMessages(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	m := f.waitEscalation(t, id)
	if m.State != domain.AgentMessageFailed || !strings.Contains(m.LastError, "expired") {
		t.Fatalf("escalation after expiry = state %s, error %q", m.State, m.LastError)
	}

	// A delay that would end after the expiry never escalates at all.
	f2 := newEscalationFixture(t, 500*time.Millisecond)
	f2.service.questionTimeout = 40 * time.Millisecond
	id2, out2 := ask(t, context.Background(), f2.service, f2.repo)
	if o := waitOutcome(t, out2); o.res.Status != domain.QuestionExpired {
		t.Fatalf("result = %+v", o.res)
	}
	if f2.escalationExists(id2) {
		t.Fatal("a question escalated although the delay ends after its expiry")
	}
}

func TestOnlyTicketWorkerQuestionsEscalate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session domain.Session
	}{
		{"architect", domain.Session{ID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect,
			CurrentRun: &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning}}},
		{"action", domain.Session{ID: "session-1", SessionType: domain.SessionTypeAction, ContextID: "exec-1",
			CurrentRun: &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEscalationFixture(t, 10*time.Millisecond)
			f.repo.listedSessions[0] = tc.session
			id, out := ask(t, context.Background(), f.service, f.repo)
			time.Sleep(60 * time.Millisecond)
			if f.escalationExists(id) {
				t.Fatalf("%s question was escalated", tc.name)
			}
			if _, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "x"); !errors.As(err, new(*domain.NotFoundError)) {
				t.Fatalf("architect answer to a %s question err = %v", tc.name, err)
			}
			f.service.cancelSessionQuestions(context.Background(), "session-1")
			waitOutcome(t, out)
		})
	}

	t.Run("disabled", func(t *testing.T) {
		f := newEscalationFixture(t, 0)
		id, out := ask(t, context.Background(), f.service, f.repo)
		time.Sleep(40 * time.Millisecond)
		if f.escalationExists(id) {
			t.Fatal("escalation ran although it is disabled")
		}
		res, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "x")
		if err != nil || res.Outcome != domain.ArchitectAnswerNotAnswerable {
			t.Fatalf("architect answer with escalation disabled = %+v, %v", res, err)
		}
		f.service.cancelSessionQuestions(context.Background(), "session-1")
		waitOutcome(t, out)
	})
}

func TestEscalationFailureLeavesTheQuestionWithTheUser(t *testing.T) {
	f := newEscalationFixture(t, 10*time.Millisecond)
	// No outbox: the notice cannot be recorded.
	f.service.messages = nil
	id, out := ask(t, context.Background(), f.service, f.repo)
	time.Sleep(50 * time.Millisecond)

	res, err := f.service.AnswerQuestionAsArchitect(context.Background(), "arch-1", id, "x")
	if err != nil || res.Outcome != domain.ArchitectAnswerNotAnswerable || !strings.Contains(res.Reason, "not been escalated") {
		t.Fatalf("architect answer after a failed escalation = %+v, %v", res, err)
	}
	final, err := f.service.AnswerQuestion(context.Background(), "session-1", id, "Yes, staging first")
	if err != nil || final.Status != domain.QuestionAnswered || final.AnsweredBy != domain.QuestionAnsweredByUser {
		t.Fatalf("user answer = %+v, %v", final, err)
	}
	if o := waitOutcome(t, out); o.err != nil || o.res.Text != "Yes, staging first" || o.res.AnsweredBy != domain.QuestionAnsweredByUser {
		t.Fatalf("worker result = %+v, %v", o.res, o.err)
	}
}

// A notice whose question the daemon no longer knows (a restart interrupted
// it) is withdrawn by reconciliation and by dispatch, never delivered.
func TestEscalationForAnUnknownQuestionIsWithdrawn(t *testing.T) {
	f := newMessageFixture(t)
	f.service.questions = newQuestionStore()
	params := AgentMessageParams{
		Kind: domain.AgentMessageQuestionEscalated, Recipient: "architect:hiveryn", ArchitectKey: "hiveryn",
		DedupeKey: questionEscalationDedupeKey("q-gone"), Body: "stale", Payload: map[string]any{"question_id": "q-gone"},
	}
	begun, err := f.service.BeginAgentMessage(context.Background(), params)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := f.service.reconcileAgentMessages(context.Background(), true); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if m := f.get(begun.ID); m.State != domain.AgentMessageFailed || !strings.Contains(m.LastError, "no longer pending") {
		t.Fatalf("reconciled pending_commit escalation = %s %q", m.State, m.LastError)
	}

	params.DedupeKey = questionEscalationDedupeKey("q-gone-2")
	params.Payload = map[string]any{"question_id": "q-gone-2"}
	committed, err := f.service.EnqueueAgentMessage(context.Background(), params)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", "claude", "native-arch-1")}
	f.dispatch()
	if m := f.get(committed.ID); m.State != domain.AgentMessageFailed || !strings.Contains(m.LastError, "withdrawn") {
		t.Fatalf("dispatched stale escalation = %s %q", m.State, m.LastError)
	}
	if got := f.adapters["claude"].deliveries(); len(got) != 0 {
		t.Fatalf("stale escalation was delivered: %+v", got)
	}
}

func TestSystemPromptsGuideQuestionEscalation(t *testing.T) {
	architect, err := builtinPrompt(architectSystemPromptName)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"answerQuestion(questionId, answer)", "never as the user's answer or approval", "not_answerable"} {
		if !strings.Contains(architect, want) {
			t.Fatalf("architect prompt lacks %q", want)
		}
	}
	worker, err := builtinPrompt(workerSystemPromptName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(worker, domain.QuestionArchitectAnswerPrefix[:len(domain.QuestionArchitectAnswerPrefix)-1]) {
		t.Fatalf("worker prompt lacks the architect answer prefix")
	}
}
