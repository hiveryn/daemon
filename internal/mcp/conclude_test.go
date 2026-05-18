package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestHandleConcludeSessionArchitectSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sessions/sess-1/conclude" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"success":    true,
			"session_id": "sess-1",
		})
	})
	server.sessionID = "sess-1"

	_, output, err := server.handleArchitectConcludeSession(context.Background(), nil, ArchitectConcludeSessionInput{
		Body: "All tasks completed.",
	})
	if err != nil {
		t.Fatalf("handleConcludeSession failed: %v", err)
	}
	if !output.Success || output.SessionID != "sess-1" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleConcludeSessionWorkerSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sessions/sess-2/conclude" {
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
			"success":    true,
			"session_id": "sess-2",
			"ticket_id":  "ticket-1",
		})
	})
	server.sessionID = "sess-2"

	_, output, err := server.handleConcludeSession(context.Background(), nil, ConcludeSessionInput{
		Body:    "Implemented feature.",
		Commits: []domain.CommitRef{{SHA: "abc123", Repo: "daemon"}, {SHA: "def456", Repo: "desktop"}},
	})
	if err != nil {
		t.Fatalf("handleConcludeSession failed: %v", err)
	}
	if !output.Success || output.TicketID != "ticket-1" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleConcludeSessionMissingBody(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-1",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleConcludeSession(context.Background(), nil, ConcludeSessionInput{})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleArchitectConcludeSessionMissingBody(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-1",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleArchitectConcludeSession(context.Background(), nil, ArchitectConcludeSessionInput{})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleConcludeSessionNoSessionID(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleConcludeSession(context.Background(), nil, ConcludeSessionInput{
		Body: "done",
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeInternal {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeInternal)
	}
}

func TestHandleConcludeSessionDaemonError(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusBadRequest, &domain.ErrorBody{
			Code:    string(domain.ErrCodeValidation),
			Message: "commits are required when not rejected",
		})
	})
	server.sessionID = "sess-1"

	_, _, err := server.handleConcludeSession(context.Background(), nil, ConcludeSessionInput{
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

func TestHandleConcludeSessionRejectsCommitWithoutRepo(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-1",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleConcludeSession(context.Background(), nil, ConcludeSessionInput{
		Body:    "done",
		Commits: []domain.CommitRef{{SHA: "abc123"}},
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
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
		writeEnvelope(t, w, http.StatusOK, domain.Ticket{})
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{
		DaemonURL:    ts.URL,
		ArchitectKey: "hiveryn",
		SessionType:  SessionTypeWork,
		SessionID:    "worker-sess",
		HTTPClient:   ts.Client(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	if server.SessionType() != SessionTypeWork {
		t.Fatalf("session type = %q, want %q", server.SessionType(), SessionTypeWork)
	}

	_, _, err = server.handleConcludeSession(context.Background(), nil, ConcludeSessionInput{
		Body:    "Worker concluded.",
		Commits: []domain.CommitRef{{SHA: "def456", Repo: "daemon"}},
	})
	if err != nil {
		t.Fatalf("worker concludeSession failed: %v", err)
	}
}
