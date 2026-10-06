package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

type fakeQuestionService struct {
	asked    domain.AskQuestionRequest
	answered string
}

func (f *fakeQuestionService) Notify(_ context.Context, _ string, message string) (domain.NotifyResult, error) {
	if message == "" {
		return domain.NotifyResult{}, &domain.ValidationError{Field: "message", Message: "is required"}
	}
	return domain.NotifyResult{Text: domain.NotificationSentText}, nil
}

func (f *fakeQuestionService) AskQuestion(_ context.Context, _ string, req domain.AskQuestionRequest) (domain.AskQuestionResult, error) {
	f.asked = req
	return domain.AskQuestionResult{QuestionID: "q1", Status: domain.QuestionAnswered, Text: "yes"}, nil
}

func (f *fakeQuestionService) AnswerQuestion(_ context.Context, sessionID, questionID, answer string) (domain.AgentQuestion, error) {
	if questionID == "stale" {
		return domain.AgentQuestion{}, &domain.ConflictError{Resource: "question", Field: "status", Message: "no longer answerable: it expired after 1 hour without an answer"}
	}
	f.answered = sessionID + "/" + questionID + "=" + answer
	return domain.AgentQuestion{ID: questionID, Status: domain.QuestionAnswered, Answer: answer}, nil
}

func TestQuestionRoutes(t *testing.T) {
	svc := &fakeQuestionService{}
	handler := NewHandler(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Questions: svc})
	do := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec
	}

	if rec := do("/api/sessions/s1/notify", `{"message":"done"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), domain.NotificationSentText) {
		t.Fatalf("notify = %d %s", rec.Code, rec.Body)
	}
	if rec := do("/api/sessions/s1/notify", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank notify = %d %s", rec.Code, rec.Body)
	}
	rec := do("/api/sessions/s1/questions", `{"question":"Ship?","answers":["Yes","No"],"recommended_index":1}`)
	var env struct {
		Data domain.AskQuestionResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Data.Text != "yes" {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body)
	}
	if svc.asked.RecommendedIndex == nil || *svc.asked.RecommendedIndex != 1 || len(svc.asked.Answers) != 2 {
		t.Fatalf("asked = %+v", svc.asked)
	}
	if rec := do("/api/sessions/s1/questions/q1/answer", `{"answer":"Yes"}`); rec.Code != http.StatusOK || svc.answered != "s1/q1=Yes" {
		t.Fatalf("answer = %d %s (%s)", rec.Code, rec.Body, svc.answered)
	}
	if rec := do("/api/sessions/s1/questions/stale/answer", `{"answer":"Yes"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no longer answerable") {
		t.Fatalf("stale answer = %d %s", rec.Code, rec.Body)
	}
}
