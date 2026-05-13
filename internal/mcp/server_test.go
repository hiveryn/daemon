package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestNewServerDefaultsToArchitect(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	if server.SessionType() != SessionTypeArchitect {
		t.Fatalf("session type = %q, want %q", server.SessionType(), SessionTypeArchitect)
	}
}

func TestNewServerWorkSession(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionType:  SessionTypeWork,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	if server.SessionType() != SessionTypeWork {
		t.Fatalf("session type = %q, want %q", server.SessionType(), SessionTypeWork)
	}
}

func TestNewServerRequiresConfig(t *testing.T) {
	t.Parallel()

	if _, err := NewServer(Config{ArchitectKey: "hiveryn"}); err == nil {
		t.Fatal("expected daemon url error")
	}
	if _, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200"}); err == nil {
		t.Fatal("expected architect key error")
	}
	if _, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn", SessionType: SessionType("bad")}); err == nil {
		t.Fatal("expected session type error")
	}
}

func TestHandleReadTicketSuccess(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	updated := created.Add(30 * time.Minute)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/tickets/2026-05-13-1430-some-title" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(t, w, http.StatusOK, domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID:            "2026-05-13-1430-some-title",
				Status:        domain.TicketStatusBacklog,
				Title:         "Some title",
				Repo:          "daemon",
				Created:       &created,
				Updated:       &updated,
				References:    []string{"abc"},
				HasConclusion: false,
				Warnings:      []domain.TicketWarning{},
			},
			Body:       "markdown string",
			Conclusion: nil,
		})
	})

	_, output, err := server.handleReadTicket(context.Background(), nil, ReadTicketInput{ID: "2026-05-13-1430-some-title"})
	if err != nil {
		t.Fatalf("handleReadTicket failed: %v", err)
	}
	if output.ID != "2026-05-13-1430-some-title" || output.Body != "markdown string" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleReadTicketValidation(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleReadTicket(context.Background(), nil, ReadTicketInput{})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleReadTicketNotFound(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusNotFound, &domain.ErrorBody{
			Code:    string(domain.ErrCodeNotFound),
			Message: "ticket missing not found",
		})
	})

	_, _, err := server.handleReadTicket(context.Background(), nil, ReadTicketInput{ID: "missing"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeNotFound {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleReadTicketMalformedEnvelope(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{"))
	})

	_, _, err := server.handleReadTicket(context.Background(), nil, ReadTicketInput{ID: "broken"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeInternal {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func newTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{
		DaemonURL:    ts.URL,
		ArchitectKey: "hiveryn",
		HTTPClient:   ts.Client(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	return server
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, status int, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(domain.Envelope{Data: data, Logs: []domain.LogEntry{}, Commands: []any{}, Meta: domain.Meta{RequestID: "req-1"}}); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
}

func writeErrorEnvelope(t *testing.T, w http.ResponseWriter, status int, errBody *domain.ErrorBody) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(domain.Envelope{Error: errBody, Logs: []domain.LogEntry{}, Commands: []any{}, Meta: domain.Meta{RequestID: "req-1"}}); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
}
