package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

// fakeCompanionService records the calls the routes make.
type fakeCompanionService struct {
	list      domain.CompanionList
	calls     []string
	discarded string
	decision  domain.ActionDecisionRequest
}

func (f *fakeCompanionService) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeCompanionService) ListArchitectCompanions(_ context.Context, key string) (domain.CompanionList, error) {
	f.record("list " + key)
	return f.list, nil
}
func (f *fakeCompanionService) SendMessageToArchitect(_ context.Context, id string, req domain.CompanionMessageRequest) (domain.CompanionMessageResult, error) {
	f.record("toArchitect " + id + " " + req.Message)
	return domain.CompanionMessageResult{MessageID: "m", Status: "accepted"}, nil
}
func (f *fakeCompanionService) ReplyToArchitectMessage(_ context.Context, id string, req domain.CompanionReplyRequest) (domain.CompanionMessageResult, error) {
	f.record("replyArchitect " + id + " " + req.MessageID)
	return domain.CompanionMessageResult{MessageID: "m", Status: "accepted"}, nil
}
func (f *fakeCompanionService) SendMessageToCompanion(_ context.Context, id, name string, req domain.CompanionMessageRequest) (domain.CompanionMessageResult, error) {
	f.record("toCompanion " + id + " " + name)
	return domain.CompanionMessageResult{MessageID: "m", Status: "accepted"}, nil
}
func (f *fakeCompanionService) ReplyToCompanionMessage(_ context.Context, id string, req domain.CompanionReplyRequest) (domain.CompanionMessageResult, error) {
	f.record("replyCompanion " + id + " " + req.MessageID)
	return domain.CompanionMessageResult{}, &domain.NotFoundError{Resource: "message_id", ID: req.MessageID}
}
func (f *fakeCompanionService) RequestCompanionShutdown(_ context.Context, id string, req domain.CompanionShutdownRequest) (domain.CompanionShutdownResult, error) {
	f.record("shutdown " + id + " " + req.Companion)
	return domain.CompanionShutdownResult{RequestID: req.RequestID, Status: "accepted"}, nil
}
func (f *fakeCompanionService) CompleteCompanionShutdown(_ context.Context, id string, req domain.CompleteCompanionShutdownRequest) (domain.CompanionShutdownResult, error) {
	f.record("complete " + id)
	return domain.CompanionShutdownResult{RequestID: req.RequestID, Status: "accepted"}, nil
}
func (f *fakeCompanionService) RequestAction(_ context.Context, id string, req domain.RequestActionRequest) (domain.RequestActionResult, error) {
	f.record("action " + id + " " + req.Title)
	return domain.RequestActionResult{Outcome: "accepted"}, nil
}
func (f *fakeCompanionService) DecideAction(_ context.Context, id, actionID string, req domain.ActionDecisionRequest) (domain.ActionRequest, error) {
	f.record("decide " + id + " " + actionID)
	f.decision = req
	return domain.ActionRequest{ID: actionID, Status: domain.ActionAccepted}, nil
}
func (f *fakeCompanionService) ListActions(_ context.Context, id string) ([]domain.ActionRequest, error) {
	f.record("actions " + id)
	return []domain.ActionRequest{}, nil
}
func (f *fakeCompanionService) DiscardCompanion(_ context.Context, id string) error {
	f.discarded = id
	return nil
}

func TestCompanionRoutes(t *testing.T) {
	t.Parallel()
	companions := &fakeCompanionService{}
	sessions := &fakeSessionService{getSessionResult: domain.Session{ID: "comp", SessionType: domain.SessionTypeCompanion}}
	handler := NewHandler(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Sessions: sessions, Companions: companions})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/api/architects/hiveryn/companions", "", 200},
		{http.MethodPost, "/api/sessions/comp/companion/messages", `{"message":"hi"}`, 200},
		{http.MethodPost, "/api/sessions/comp/companion/replies", `{"message_id":"a1","message":"hi"}`, 200},
		{http.MethodPost, "/api/sessions/comp/companion/shutdown-completion", `{"request_id":"r","handoff":"h"}`, 200},
		{http.MethodPost, "/api/sessions/comp/actions", `{"title":"t","description":"d"}`, 200},
		{http.MethodGet, "/api/sessions/comp/actions", "", 200},
		{http.MethodPost, "/api/sessions/comp/actions/act-1/decision", `{"decision":"reply","message":"no"}`, 200},
		{http.MethodPost, "/api/sessions/arch/companions/browser/messages", `{"message":"go"}`, 200},
		{http.MethodPost, "/api/sessions/arch/companions/replies", `{"message_id":"x","message":"go"}`, 404},
		{http.MethodPost, "/api/sessions/arch/companions/shutdown", `{"companion":"browser","request_id":"r"}`, 200},
		{http.MethodPost, "/api/sessions/comp/discard", "", 200},
	} {
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		status, raw := request(t, handler, tc.method, tc.path, body)
		if status != tc.status {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, status, raw)
		}
	}
	want := "list hiveryn|toArchitect comp hi|replyArchitect comp a1|complete comp|action comp t|actions comp|decide comp act-1|toCompanion arch browser|replyCompanion arch x|shutdown arch browser"
	if got := strings.Join(companions.calls, "|"); got != want {
		t.Fatalf("calls = %s", got)
	}
	if companions.decision.Decision != domain.ActionDecisionReply || companions.decision.Message != "no" || companions.discarded != "comp" {
		t.Fatalf("decision = %+v discarded = %q", companions.decision, companions.discarded)
	}
}
