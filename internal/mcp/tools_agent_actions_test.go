package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestActionToolsAreRegisteredPerRole(t *testing.T) {
	t.Parallel()

	requestTools := []string{"getAvailableActions", "executeAction", "getActionResult", "waitForActionResult"}
	cases := []struct {
		name        string
		cfg         Config
		wantRequest bool
		wantAdd     bool
	}{
		{"architect", Config{ArchitectKey: "hiveryn", SessionID: "sess-arch", SessionType: SessionTypeArchitect}, true, true},
		{"ticket", Config{ArchitectKey: "hiveryn", SessionID: "sess-ticket", SessionType: SessionTypeTicket}, true, false},
		{"action", Config{SessionID: "sess-action", SessionType: SessionTypeAction}, false, false},
	}
	for _, tc := range cases {
		tc.cfg.DaemonURL = "http://127.0.0.1:4200"
		server, err := NewServer(tc.cfg)
		if err != nil {
			t.Fatalf("%s: NewServer: %v", tc.name, err)
		}
		names := registeredToolNames(t, server)
		for _, tool := range requestTools {
			if _, ok := names[tool]; ok != tc.wantRequest {
				t.Errorf("%s: has %s = %v, want %v", tc.name, tool, ok, tc.wantRequest)
			}
		}
		if _, ok := names["addAvailableAction"]; ok != tc.wantAdd {
			t.Errorf("%s: has addAvailableAction = %v, want %v", tc.name, ok, tc.wantAdd)
		}
	}
}

func TestAddAvailableActionCallsSessionScopedEndpoint(t *testing.T) {
	t.Parallel()

	var got domain.AddAvailableActionRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /api/sessions/sess-arch/available-actions" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		writeEnvelope(t, w, http.StatusOK, domain.AddAvailableActionResult{Changed: true, AvailableActions: []string{"demo", "nova"}})
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{DaemonURL: ts.URL, ArchitectKey: "hiveryn", SessionID: "sess-arch", SessionType: SessionTypeArchitect, HTTPClient: ts.Client()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if _, _, err := server.handleAddAvailableAction(context.Background(), nil, AddAvailableActionInput{Name: "  "}); err == nil {
		t.Fatal("blank name accepted")
	}
	_, out, err := server.handleAddAvailableAction(context.Background(), nil, AddAvailableActionInput{Name: " nova "})
	if err != nil || !out.Changed || strings.Join(out.AvailableActions, ",") != "demo,nova" {
		t.Fatalf("addAvailableAction = %+v, %v", out, err)
	}
	if got.Name != "nova" {
		t.Fatalf("daemon received %+v", got)
	}
}

func TestAgentActionToolsCallSessionScopedEndpoints(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	elapsed := int64(12)
	var executed domain.ExecuteActionRequest
	var waitQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/sessions/sess-arch/available-actions":
			writeEnvelope(t, w, http.StatusOK, domain.AvailableActionList{Actions: []domain.ActionDefinition{{Name: "demo", Valid: true, Description: "d", Artifacts: "a"}}})
		case "POST /api/sessions/sess-arch/intents/execute-action":
			if err := json.NewDecoder(r.Body).Decode(&executed); err != nil {
				t.Fatalf("decode: %v", err)
			}
			writeEnvelope(t, w, http.StatusAccepted, domain.ActionResult{ExecutionID: "exec-1", Action: "demo", Status: domain.ActionRunPendingApproval})
		case "GET /api/sessions/sess-arch/action-results/exec-1":
			writeEnvelope(t, w, http.StatusOK, domain.ActionResult{ExecutionID: "exec-1", Status: domain.ActionRunRunning, StartedAt: &started, ElapsedSeconds: &elapsed, Activity: domain.ActionAgentActivity{Available: true, Status: "active"}})
		case "GET /api/sessions/sess-arch/action-results/exec-1/wait":
			waitQuery = r.URL.RawQuery
			writeEnvelope(t, w, http.StatusOK, domain.ActionWaitResult{Result: domain.ActionResult{ExecutionID: "exec-1", Status: domain.ActionRunRunning}, TimedOut: true})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{DaemonURL: ts.URL, ArchitectKey: "hiveryn", SessionID: "sess-arch", SessionType: SessionTypeArchitect, HTTPClient: ts.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx := context.Background()

	_, list, err := server.handleGetAvailableActions(ctx, nil, GetAvailableActionsInput{})
	if err != nil || len(list.Actions) != 1 || list.Actions[0].Problems == nil {
		t.Fatalf("available = %+v, %v", list, err)
	}

	if _, _, err := server.handleExecuteAction(ctx, nil, ExecuteActionInput{Name: "demo"}); err == nil {
		t.Fatal("blank prompt accepted")
	}
	_, pending, err := server.handleExecuteAction(ctx, nil, ExecuteActionInput{Name: "demo", Prompt: "compare"})
	if err != nil || pending.Result.ExecutionID != "exec-1" || pending.Result.Status != domain.ActionRunPendingApproval || !strings.Contains(pending.Guidance, "approve") {
		t.Fatalf("executeAction = %+v, %v", pending, err)
	}
	if executed.Name != "demo" || executed.Prompt != "compare" {
		t.Fatalf("daemon received %+v", executed)
	}

	_, result, err := server.handleGetActionResult(ctx, nil, ActionResultInput{ExecutionID: "exec-1"})
	if err != nil || result.Result.Status != domain.ActionRunRunning || *result.Result.ElapsedSeconds != 12 || !result.Result.Activity.Available {
		t.Fatalf("getActionResult = %+v, %v", result, err)
	}

	for _, bad := range []int{-1, 31} {
		if _, _, err := server.handleWaitForActionResult(ctx, nil, WaitForActionResultInput{ExecutionID: "exec-1", TimeoutSeconds: bad}); err == nil {
			t.Errorf("timeout %d accepted", bad)
		}
	}
	_, waited, err := server.handleWaitForActionResult(ctx, nil, WaitForActionResultInput{ExecutionID: "exec-1"})
	if err != nil || !waited.TimedOut || !strings.Contains(waited.Guidance, "again") {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	if waitQuery != "timeout_seconds=30" {
		t.Fatalf("wait query = %q, want the 30s default", waitQuery)
	}
}
