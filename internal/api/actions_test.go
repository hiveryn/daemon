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

type fakeActionService struct {
	domain.ActionService
	launched domain.LaunchActionRequest
	busy     bool
	limit    int
}

func (f *fakeActionService) LaunchAction(_ context.Context, name string, req domain.LaunchActionRequest) (domain.LaunchActionResult, error) {
	if f.busy {
		return domain.LaunchActionResult{}, &domain.ConflictError{Resource: "action", Field: "action", Message: "action " + name + " is already running"}
	}
	f.launched = req
	return domain.LaunchActionResult{Run: domain.ActionRun{ID: "run-1", Action: name, Status: domain.ActionRunRunning, OutputDir: "/out/run-1"}}, nil
}

func (f *fakeActionService) ListActionRuns(_ context.Context, _ string, limit int) ([]domain.ActionRun, error) {
	f.limit = limit
	return []domain.ActionRun{}, nil
}

func TestActionLaunchRoutes(t *testing.T) {
	svc := &fakeActionService{}
	handler := NewHandler(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Actions: svc})

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/actions/demo-evidence/runs", `{"prompt":"Run AMS and LDN","profile_name":"codex"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("launch status = %d body %s", rec.Code, rec.Body)
	}
	var env struct {
		Data domain.LaunchActionResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Run.ID != "run-1" || env.Data.Run.OutputDir != "/out/run-1" || svc.launched.Prompt != "Run AMS and LDN" || svc.launched.ProfileName != "codex" {
		t.Fatalf("launch = %+v, request %+v", env.Data, svc.launched)
	}

	svc.busy = true
	if rec := do(http.MethodPost, "/api/actions/demo-evidence/runs", `{"prompt":"x","profile_name":"codex"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already running") {
		t.Fatalf("busy launch status = %d body %s", rec.Code, rec.Body)
	}

	if rec := do(http.MethodGet, "/api/action-runs?limit=0", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit status = %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/action-runs?action=demo-evidence&limit=20", ""); rec.Code != http.StatusOK || svc.limit != 20 {
		t.Fatalf("list status = %d limit %d", rec.Code, svc.limit)
	}
}
