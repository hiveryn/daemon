package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestActionToolsAreRegisteredPerRole(t *testing.T) {
	t.Parallel()

	requestTools := []string{"getAvailableActions", "executeAction", "getActionResult"}
	cases := []struct {
		name        string
		cfg         Config
		wantRequest bool
		wantSpawn   bool
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
		for _, obsolete := range []string{"addAvailableAction", "waitForActionResult"} {
			if _, ok := names[obsolete]; ok {
				t.Errorf("%s: obsolete %s tool is registered", tc.name, obsolete)
			}
		}
		if _, ok := names["spawnTicketWorker"]; ok != tc.wantSpawn {
			t.Errorf("%s: has spawnTicketWorker = %v, want %v", tc.name, ok, tc.wantSpawn)
		}
	}
}

func TestAgentActionToolsCallSessionScopedEndpoints(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	elapsed := int64(12)
	var executed domain.ExecuteActionRequest
	var notification *domain.ActionNotification
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/sessions/sess-arch/available-actions":
			writeEnvelope(t, w, http.StatusOK, domain.AvailableActionList{Actions: []domain.ActionDefinition{{Name: "demo", Valid: true, Description: "d", Artifacts: "a", Suggestions: []string{"Compare three runs"}}}})
		case "POST /api/sessions/sess-arch/intents/execute-action":
			if err := json.NewDecoder(r.Body).Decode(&executed); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if executed.Variant == "" {
				writeErrorEnvelope(t, w, http.StatusBadRequest, &domain.ErrorBody{Code: string(domain.ErrCodeValidation), Message: missingVariantMessage})
				return
			}
			writeEnvelope(t, w, http.StatusOK, domain.ExecuteActionResponse{Outcome: domain.IntentOutcomeAutoApproved, Result: domain.ActionResult{ExecutionID: "exec-1", Action: "demo", Status: domain.ActionRunRunning, ProfileName: executed.Variant}, Notification: notification})
		case "GET /api/sessions/sess-arch/action-results/exec-1":
			writeEnvelope(t, w, http.StatusOK, domain.ActionResult{ExecutionID: "exec-1", Status: domain.ActionRunRunning, StartedAt: &started, ElapsedSeconds: &elapsed, Activity: domain.ActionAgentActivity{Available: true, Status: "active"}})
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
	if err != nil || len(list.Actions) != 1 || list.Actions[0].Problems == nil || len(list.Actions[0].Suggestions) != 1 || list.Actions[0].Suggestions[0] != "Compare three runs" {
		t.Fatalf("available = %+v, %v", list, err)
	}

	if _, _, err := server.handleExecuteAction(ctx, nil, ExecuteActionInput{Name: "demo"}); err == nil {
		t.Fatal("blank prompt accepted")
	}
	// A missing variant reaches the daemon, whose error lists the choices.
	if _, _, err := server.handleExecuteAction(ctx, nil, ExecuteActionInput{Name: "demo", Prompt: "compare"}); err == nil || !strings.Contains(err.Error(), missingVariantMessage) {
		t.Fatalf("missing variant err = %v, want the daemon's message", err)
	}
	// A launched execution's guidance names the execution id and promises
	// the notice — or says why it cannot reach this session.
	notification = &domain.ActionNotification{Available: true}
	_, launched, err := server.handleExecuteAction(ctx, nil, ExecuteActionInput{Name: "demo", Prompt: "compare", Variant: "codex"})
	if err != nil || launched.Outcome != "auto_approved" || launched.IntentID != "exec-1" || launched.Result.ExecutionID != "exec-1" || launched.Result.Status != domain.ActionRunRunning ||
		launched.Notification == nil || !launched.Notification.Available ||
		!strings.Contains(launched.Guidance, "execution exec-1 now runs independently") || !strings.Contains(launched.Guidance, "You will be notified in this conversation when it ends") || strings.Contains(launched.Guidance, "wait") {
		t.Fatalf("executeAction = %+v, %v", launched, err)
	}
	if executed.Name != "demo" || executed.Prompt != "compare" || executed.Variant != "codex" {
		t.Fatalf("daemon received %+v", executed)
	}
	notification = &domain.ActionNotification{Reason: "this session runs on machine bk, and message delivery to remote sessions is not available"}
	_, launched, err = server.handleExecuteAction(ctx, nil, ExecuteActionInput{Name: "demo", Prompt: "compare again", Variant: "codex"})
	if err != nil || launched.Notification == nil || launched.Notification.Available || strings.Contains(launched.Guidance, "You will be notified") ||
		!strings.Contains(launched.Guidance, "Notices cannot reach this session (this session runs on machine bk, and message delivery to remote sessions is not available): read getActionResult") {
		t.Fatalf("executeAction without delivery = %+v, %v", launched, err)
	}
	notification = nil
	if _, _, err := server.handleExecuteAction(ctx, nil, ExecuteActionInput{Name: "demo", Prompt: "compare once more", Variant: "codex"}); err == nil {
		t.Fatal("a launched execution without notification information was accepted")
	}

	_, result, err := server.handleGetActionResult(ctx, nil, ActionResultInput{ExecutionID: "exec-1"})
	if err != nil || result.Result.Status != domain.ActionRunRunning || *result.Result.ElapsedSeconds != 12 || !result.Result.Activity.Available {
		t.Fatalf("getActionResult = %+v, %v", result, err)
	}

	if strings.Contains(result.Guidance, "wait") {
		t.Fatalf("running guidance tells the agent to wait: %q", result.Guidance)
	}
}

const missingVariantMessage = "variant is required and has no default. Ask the user which agent variant should run this Action, then request it again with that variant. Variants configured for local: codex (codex)"

// Over a real MCP connection, an executeAction call without a variant must
// reach the daemon's diagnostic instead of failing input-schema validation.
func TestExecuteActionWithoutVariantReachesDaemonDiagnostic(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /api/sessions/sess-ticket/intents/execute-action" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		writeErrorEnvelope(t, w, http.StatusBadRequest, &domain.ErrorBody{Code: string(domain.ErrCodeValidation), Message: missingVariantMessage})
	}))
	t.Cleanup(ts.Close)
	server, err := NewServer(Config{DaemonURL: ts.URL, ArchitectKey: "hiveryn", SessionID: "sess-ticket", SessionType: SessionTypeTicket, HTTPClient: ts.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := server.mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "dev"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "executeAction" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode input schema: %v", err)
		}
		if _, ok := schema.Properties["variant"]; !ok || slices.Contains(schema.Required, "variant") {
			t.Fatalf("executeAction input schema = %s, want variant described but not schema-required", raw)
		}
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "executeAction", Arguments: map[string]any{"name": "demo", "prompt": "compare"}})
	if err != nil {
		t.Fatalf("CallTool(executeAction): %v", err)
	}
	if !result.IsError {
		t.Fatalf("executeAction without variant succeeded: %+v", result)
	}
	text := ""
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if !strings.Contains(text, "Ask the user") || !strings.Contains(text, "codex (codex)") {
		t.Fatalf("tool error = %q, want the daemon's variant diagnostic", text)
	}
}

func TestSpawnTicketWorkerCallsSessionScopedEndpoint(t *testing.T) {
	t.Parallel()

	var got domain.SpawnTicketWorkerRequest
	reply := domain.SpawnTicketWorkerResponse{
		IntentID: "intent-1",
		Outcome:  domain.IntentOutcomeAutoApproved,
		Worker:   &domain.SpawnedTicketWorker{SessionID: "sess-w", RunID: "run-w", TicketID: "ticket-1", Variant: "claude", Workflows: []string{"AUTONOMOUS_COMMIT"}, WorkflowPaths: []string{"/ws/workflows/AUTONOMOUS_COMMIT.md"}},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /api/sessions/sess-arch/intents/spawn-ticket-worker" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Variant == "" {
			writeEnvelope(t, w, http.StatusOK, domain.SpawnTicketWorkerResponse{IntentID: "intent-2", Outcome: domain.IntentOutcomeError, Reason: "launch worker for ticket ticket-1: boom"})
			return
		}
		writeEnvelope(t, w, http.StatusOK, reply)
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{DaemonURL: ts.URL, ArchitectKey: "hiveryn", SessionID: "sess-arch", SessionType: SessionTypeArchitect, HTTPClient: ts.Client()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx := context.Background()
	if _, _, err := server.handleSpawnTicketWorker(ctx, nil, SpawnTicketWorkerInput{TicketID: " "}); err == nil {
		t.Fatal("blank ticketId accepted")
	}
	_, out, err := server.handleSpawnTicketWorker(ctx, nil, SpawnTicketWorkerInput{TicketID: "ticket-1", Variant: "claude", Workflows: []string{"AUTONOMOUS_COMMIT.md"}})
	if err != nil {
		t.Fatalf("spawnTicketWorker: %v", err)
	}
	if got.TicketID != "ticket-1" || got.Variant != "claude" || !slices.Equal(got.Workflows, []string{"AUTONOMOUS_COMMIT.md"}) {
		t.Fatalf("daemon received %+v", got)
	}
	if out.Outcome != string(domain.IntentOutcomeAutoApproved) || out.IntentID != "intent-1" || out.Worker == nil || out.Worker.SessionID != "sess-w" || !strings.Contains(out.Guidance, "launched") {
		t.Fatalf("output = %+v", out)
	}

	// A failed launch is reported as such, with its cause, and no worker.
	_, out, err = server.handleSpawnTicketWorker(ctx, nil, SpawnTicketWorkerInput{TicketID: "ticket-1"})
	if err != nil {
		t.Fatalf("spawnTicketWorker error outcome: %v", err)
	}
	if out.Outcome != string(domain.IntentOutcomeError) || out.Worker != nil || !strings.Contains(out.Reason, "boom") {
		t.Fatalf("error output = %+v", out)
	}
}
