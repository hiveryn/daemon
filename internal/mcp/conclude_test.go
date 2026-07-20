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

	"github.com/hiveryn/daemon/internal/domain"
)

func TestHandleArchitectConcludeSessionSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sessions/sess-1/intents/conclude-session" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body struct {
			Summary   string `json:"summary"`
			Narrative string `json:"narrative"`
			NextSteps string `json:"next_steps"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Summary != "Wrapped up." || body.Narrative != "Did the work." {
			t.Fatalf("unexpected payload: %#v", body)
		}
		if body.NextSteps != "None" {
			t.Fatalf("unexpected next_steps: %#v", body.NextSteps)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"intent_id": "intent-1",
			"outcome":   "approved",
			"result":    map[string]any{"session_id": "sess-1"},
		})
	})
	server.sessionID = "sess-1"

	_, output, err := server.handleArchitectConcludeSession(context.Background(), nil, ArchitectConcludeSessionInput{
		Summary:   "Wrapped up.",
		Narrative: "Did the work.",
		NextSteps: "None",
	})
	if err != nil {
		t.Fatalf("handleArchitectConcludeSession failed: %v", err)
	}
	if output.Outcome != intentOutcomeApproved || output.Session == nil || output.Session.SessionID != "sess-1" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleTicketConcludeSessionSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sessions/sess-2/intents/conclude-session" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body struct {
			Commits []domain.CommitRef `json:"commits"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		wantCommits := []domain.CommitRef{{SHA: "abc123", Repo: "daemon"}, {SHA: "def456", Repo: "desktop"}}
		if len(body.Commits) != len(wantCommits) || body.Commits[0] != wantCommits[0] || body.Commits[1] != wantCommits[1] {
			t.Fatalf("unexpected commits payload: %#v", body.Commits)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"intent_id": "intent-2",
			"outcome":   "approved",
			"result":    map[string]any{"session_id": "sess-2", "ticket_id": "ticket-1"},
		})
	})
	server.sessionID = "sess-2"
	server.sessionType = SessionTypeTicket

	_, output, err := server.handleTicketConcludeSession(context.Background(), nil, TicketConcludeSessionInput{
		Summary:        "Implemented feature.",
		Outcome:        "completed",
		Implementation: "Built the thing.",
		Commits:        []domain.CommitRef{{SHA: "abc123", Repo: "daemon"}, {SHA: "def456", Repo: "desktop"}},
	})
	if err != nil {
		t.Fatalf("handleTicketConcludeSession failed: %v", err)
	}
	if output.Outcome != intentOutcomeApproved || output.Session == nil || output.Session.TicketID != "ticket-1" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleFreeformConcludeSessionSuccessWithoutCommits(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sessions/sess-3/intents/conclude-session" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body struct {
			Commits []domain.CommitRef `json:"commits"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if len(body.Commits) != 0 {
			t.Fatalf("expected no commits payload, got %#v", body.Commits)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"intent_id": "intent-3",
			"outcome":   "approved",
			"result":    map[string]any{"session_id": "sess-3"},
		})
	})
	server.sessionID = "sess-3"
	server.sessionType = SessionTypeFreeform

	_, output, err := server.handleFreeformConcludeSession(context.Background(), nil, FreeformConcludeSessionInput{
		Summary:         "Exploration concluded.",
		Findings:        "Found some things.",
		Recommendations: "None",
		OpenQuestions:   "None",
	})
	if err != nil {
		t.Fatalf("handleFreeformConcludeSession failed: %v", err)
	}
	if output.Outcome != intentOutcomeApproved || output.Session == nil || output.Session.SessionID != "sess-3" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

// Intents are addressed by session id, and every session type registers at
// least one intent-routed tool, so a missing session id is rejected at
// construction rather than lazily per handler.
func TestNewServerRequiresSessionID(t *testing.T) {
	t.Parallel()

	for _, sessionType := range []SessionType{SessionTypeArchitect, SessionTypeTicket, SessionTypeFreeform} {
		_, err := NewServer(Config{
			DaemonURL:    "http://127.0.0.1:4200",
			ArchitectKey: "hiveryn",
			SessionType:  sessionType,
		})
		if err == nil {
			t.Fatalf("%s: expected NewServer to reject an empty SessionID", sessionType)
		}
		if !strings.Contains(err.Error(), "HIVERYN_SESSION_ID") {
			t.Fatalf("%s: error should name the missing env var, got %v", sessionType, err)
		}
	}
}

func TestHandleTicketConcludeSessionDaemonError(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusBadRequest, &domain.ErrorBody{
			Code:    string(domain.ErrCodeValidation),
			Message: "commits are required when outcome is completed",
		})
	})
	server.sessionID = "sess-1"
	server.sessionType = SessionTypeTicket

	_, _, err := server.handleTicketConcludeSession(context.Background(), nil, TicketConcludeSessionInput{
		Summary:        "done",
		Outcome:        "completed",
		Implementation: "done",
		Commits:        []domain.CommitRef{{SHA: "abc123", Repo: "daemon"}},
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleTicketConcludeSessionExploratorySuccessWithoutCommits(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Outcome string             `json:"outcome"`
			Commits []domain.CommitRef `json:"commits"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Outcome != "exploratory" {
			t.Fatalf("unexpected outcome: %q", body.Outcome)
		}
		if len(body.Commits) != 0 {
			t.Fatalf("expected no commits payload, got %#v", body.Commits)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"intent_id": "intent-4",
			"outcome":   "approved",
			"result":    map[string]any{"session_id": "sess-4", "ticket_id": "ticket-2"},
		})
	})
	server.sessionID = "sess-4"
	server.sessionType = SessionTypeTicket

	_, output, err := server.handleTicketConcludeSession(context.Background(), nil, TicketConcludeSessionInput{
		Summary:        "Investigated the flake.",
		Outcome:        "exploratory",
		Implementation: "Found the root cause; no commits produced.",
	})
	if err != nil {
		t.Fatalf("handleTicketConcludeSession failed: %v", err)
	}
	if output.Outcome != intentOutcomeApproved || output.Session == nil || output.Session.TicketID != "ticket-2" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleTicketConcludeSessionRejectsInvalidOutcome(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-1",
		SessionType:  SessionTypeTicket,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleTicketConcludeSession(context.Background(), nil, TicketConcludeSessionInput{
		Summary:        "done",
		Implementation: "done",
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleTicketConcludeSessionRejectsCommitWithoutRepo(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-1",
		SessionType:  SessionTypeTicket,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleTicketConcludeSession(context.Background(), nil, TicketConcludeSessionInput{
		Summary:        "done",
		Implementation: "done",
		Commits:        []domain.CommitRef{{SHA: "abc123"}},
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleMoveTicketToDoneRejectsInvalidOutcome(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleMoveTicketToDone(context.Background(), nil, MoveTicketToDoneInput{
		ID:   "ticket-1",
		Body: "done",
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleMoveTicketToDoneRejectsMissingRejectionReason(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleMoveTicketToDone(context.Background(), nil, MoveTicketToDoneInput{
		ID:      "ticket-1",
		Body:    "done",
		Outcome: "rejected",
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleMoveTicketToDoneExploratorySuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Outcome string             `json:"outcome"`
			Commits []domain.CommitRef `json:"commits"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Outcome != "exploratory" {
			t.Fatalf("unexpected outcome: %q", body.Outcome)
		}
		if len(body.Commits) != 0 {
			t.Fatalf("expected no commits payload, got %#v", body.Commits)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"success":   true,
			"ticket_id": "ticket-1",
		})
	})

	_, output, err := server.handleMoveTicketToDone(context.Background(), nil, MoveTicketToDoneInput{
		ID:      "ticket-1",
		Body:    "Investigated; no commits produced.",
		Outcome: "exploratory",
	})
	if err != nil {
		t.Fatalf("handleMoveTicketToDone failed: %v", err)
	}
	if !output.Success || output.TicketID != "ticket-1" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestSessionIDPassedToMCPServer(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "my-session-id",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	if server.sessionID != "my-session-id" {
		t.Fatalf("sessionID = %q, want %q", server.sessionID, "my-session-id")
	}
}

func TestWorkerSessionRegistersConcludeTool(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"intent_id": "intent-worker",
			"outcome":   "approved",
			"result":    map[string]any{"session_id": "worker-sess"},
		})
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{
		DaemonURL:    ts.URL,
		ArchitectKey: "hiveryn",
		SessionType:  SessionTypeTicket,
		SessionID:    "worker-sess",
		HTTPClient:   ts.Client(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	if server.SessionType() != SessionTypeTicket {
		t.Fatalf("session type = %q, want %q", server.SessionType(), SessionTypeTicket)
	}

	_, _, err = server.handleTicketConcludeSession(context.Background(), nil, TicketConcludeSessionInput{
		Summary:        "Worker concluded.",
		Outcome:        "completed",
		Implementation: "Built it.",
		Commits:        []domain.CommitRef{{SHA: "def456", Repo: "daemon"}},
	})
	if err != nil {
		t.Fatalf("worker concludeSession failed: %v", err)
	}
}
