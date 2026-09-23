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

func TestNewServerDefaultsToArchitect(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	if server.SessionType() != SessionTypeArchitect {
		t.Fatalf("session type = %q, want %q", server.SessionType(), SessionTypeArchitect)
	}
}

func TestNewServerTicketSession(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
		SessionType:  SessionTypeTicket,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	if server.SessionType() != SessionTypeTicket {
		t.Fatalf("session type = %q, want %q", server.SessionType(), SessionTypeTicket)
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
	// Freeform sessions were removed; the role must not come back as a tool set.
	if _, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn", SessionID: "sess-test", SessionType: SessionType("freeform")}); err == nil {
		t.Fatal("expected freeform session type to be rejected")
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
		SessionID:    "sess-test",
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

func TestHandleListTicketsSuccess(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/architects/hiveryn/tickets") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("status") != "backlog" {
			t.Fatalf("status = %s", r.URL.Query().Get("status"))
		}
		if r.URL.Query().Get("limit") != "10" {
			t.Fatalf("limit = %s", r.URL.Query().Get("limit"))
		}
		writeEnvelope(t, w, http.StatusOK, []domain.TicketSummary{
			{ID: "ticket-1", Status: domain.TicketStatusBacklog, Title: "First", Created: &created, References: []string{}, Warnings: []domain.TicketWarning{}},
			{ID: "ticket-2", Status: domain.TicketStatusBacklog, Title: "Second", Created: &created, References: []string{}, Warnings: []domain.TicketWarning{}},
		})
	})

	_, output, err := server.handleListTickets(context.Background(), nil, ListTicketsInput{Status: "backlog", Limit: 10})
	if err != nil {
		t.Fatalf("handleListTickets failed: %v", err)
	}
	if len(output.Tickets) != 2 {
		t.Fatalf("expected 2 tickets, got %d", len(output.Tickets))
	}
	if output.Tickets[0].ID != "ticket-1" {
		t.Fatalf("unexpected first ticket: %#v", output.Tickets[0])
	}
}

func TestHandleListTicketsMissingStatus(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleListTickets(context.Background(), nil, ListTicketsInput{})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleListTicketsBadStatus(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleListTickets(context.Background(), nil, ListTicketsInput{Status: "invalid"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleListTicketsLimitApplied(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "5" {
			t.Fatalf("limit = %s, want 5", r.URL.Query().Get("limit"))
		}
		writeEnvelope(t, w, http.StatusOK, []domain.TicketSummary{})
	})

	_, _, err := server.handleListTickets(context.Background(), nil, ListTicketsInput{Status: "done", Limit: 5})
	if err != nil {
		t.Fatalf("handleListTickets failed: %v", err)
	}
}

func TestHandleCreateWorkTicketSuccess(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"intent_id": "intent-1",
			"outcome":   "approved",
			"result": domain.Ticket{
				TicketSummary: domain.TicketSummary{
					ID:            "2026-05-13-1430-new-ticket",
					Status:        domain.TicketStatusBacklog,
					Title:         "New Ticket",
					Repo:          "daemon",
					Created:       &created,
					References:    []string{},
					HasConclusion: false,
					Warnings:      []domain.TicketWarning{},
				},
				Body:       "ticket body",
				Conclusion: nil,
			},
		})
	})

	_, output, err := server.handleCreateWorkTicket(context.Background(), nil, CreateWorkTicketInput{
		Title: "New Ticket",
		Repo:  "daemon",
		Body:  "ticket body",
	})
	if err != nil {
		t.Fatalf("handleCreateWorkTicket failed: %v", err)
	}
	if output.Outcome != intentOutcomeApproved {
		t.Fatalf("expected an approved outcome, got %#v", output)
	}
	if output.Ticket == nil || output.Ticket.ID != "2026-05-13-1430-new-ticket" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleCreateWorkTicketMissingTitle(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleCreateWorkTicket(context.Background(), nil, CreateWorkTicketInput{})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleCreateWorkTicketInvalidRepo(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusBadRequest, &domain.ErrorBody{
			Code:    string(domain.ErrCodeValidation),
			Message: "repo key 'nonexistent' not found in architect config",
		})
	})

	_, _, err := server.handleCreateWorkTicket(context.Background(), nil, CreateWorkTicketInput{
		Title: "Some ticket",
		Repo:  "nonexistent",
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeValidation)
	}
}

func TestHandleCreateWorkTicketOptionalFieldsOmitted(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if _, hasRepo := body["repo"]; hasRepo {
			t.Fatalf("repo should be absent when empty")
		}
		if _, hasBody := body["body"]; hasBody {
			t.Fatalf("body should be absent when empty")
		}
		if _, hasRefs := body["references"]; hasRefs {
			t.Fatalf("references should be absent when empty")
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"intent_id": "intent-2",
			"outcome":   "approved",
			"result": domain.Ticket{
				TicketSummary: domain.TicketSummary{
					ID:     "minimal-ticket",
					Status: domain.TicketStatusBacklog,
					Title:  "Minimal",
				},
			},
		})
	})

	_, output, err := server.handleCreateWorkTicket(context.Background(), nil, CreateWorkTicketInput{
		Title: "Minimal",
	})
	if err != nil {
		t.Fatalf("handleCreateWorkTicket failed: %v", err)
	}
	if output.Ticket == nil || output.Ticket.ID != "minimal-ticket" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleDeleteTicketSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s", r.Method)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]bool{"deleted": true})
	})

	_, output, err := server.handleDeleteTicket(context.Background(), nil, DeleteTicketInput{ID: "ticket-to-delete"})
	if err != nil {
		t.Fatalf("handleDeleteTicket failed: %v", err)
	}
	if !output.Deleted {
		t.Fatalf("expected deleted=true, got %#v", output)
	}
}

func TestHandleDeleteTicketNotFound(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusNotFound, &domain.ErrorBody{
			Code:    string(domain.ErrCodeNotFound),
			Message: "ticket missing not found",
		})
	})

	_, _, err := server.handleDeleteTicket(context.Background(), nil, DeleteTicketInput{ID: "missing"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeNotFound {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleEditTicketBodySuccess(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/tickets/edit-me" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["oldString"] != "foo" || body["newString"] != "bar" {
			t.Fatalf("body = %v", body)
		}
		writeEnvelope(t, w, http.StatusOK, domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID:         "edit-me",
				Status:     domain.TicketStatusBacklog,
				Title:      "Edited Ticket",
				Repo:       "daemon",
				Created:    &created,
				References: []string{},
				Warnings:   []domain.TicketWarning{},
			},
			Body:       "markdown body",
			Conclusion: nil,
		})
	})

	_, output, err := server.handleEditTicketBody(context.Background(), nil, EditTicketBodyInput{
		ID:        "edit-me",
		OldString: "foo",
		NewString: "bar",
	})
	if err != nil {
		t.Fatalf("handleEditTicketBody failed: %v", err)
	}
	if output.ID != "edit-me" || output.Title != "Edited Ticket" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleEditTicketBodyMissingID(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleEditTicketBody(context.Background(), nil, EditTicketBodyInput{OldString: "foo", NewString: "bar"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleEditTicketBodyMissingOldString(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleEditTicketBody(context.Background(), nil, EditTicketBodyInput{ID: "edit-me", NewString: "bar"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleUpdateTicketSuccessAllFields(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/tickets/update-me/metadata" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["title"] != "Updated Title" || body["repo"] != "daemon" {
			t.Fatalf("body = %v", body)
		}
		refs, ok := body["references"].([]any)
		if !ok || len(refs) != 2 || refs[0] != "ref-1" || refs[1] != "ref-2" {
			t.Fatalf("references = %v", body["references"])
		}
		writeEnvelope(t, w, http.StatusOK, domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID:         "update-me",
				Status:     domain.TicketStatusBacklog,
				Title:      "Updated Title",
				Repo:       "daemon",
				Created:    &created,
				References: []string{"ref-1", "ref-2"},
				Warnings:   []domain.TicketWarning{},
			},
			Body:       "body",
			Conclusion: nil,
		})
	})

	_, output, err := server.handleUpdateTicket(context.Background(), nil, UpdateTicketInput{
		ID:         "update-me",
		Title:      "Updated Title",
		Repo:       "daemon",
		References: []string{"ref-1", "ref-2"},
	})
	if err != nil {
		t.Fatalf("handleUpdateTicket failed: %v", err)
	}
	if output.ID != "update-me" || output.Title != "Updated Title" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleUpdateTicketSuccessPartialFields(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/tickets/update-me/metadata" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if _, hasRepo := body["repo"]; hasRepo {
			t.Fatalf("repo should be absent when empty")
		}
		if _, hasRefs := body["references"]; hasRefs {
			t.Fatalf("references should be absent when empty")
		}
		if body["title"] != "Only Title" {
			t.Fatalf("title = %v", body["title"])
		}
		writeEnvelope(t, w, http.StatusOK, domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID:         "update-me",
				Status:     domain.TicketStatusBacklog,
				Title:      "Only Title",
				Created:    &created,
				References: []string{},
				Warnings:   []domain.TicketWarning{},
			},
			Body:       "body",
			Conclusion: nil,
		})
	})

	_, output, err := server.handleUpdateTicket(context.Background(), nil, UpdateTicketInput{
		ID:    "update-me",
		Title: "Only Title",
	})
	if err != nil {
		t.Fatalf("handleUpdateTicket failed: %v", err)
	}
	if output.Title != "Only Title" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandleUpdateTicketMissingID(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{
		DaemonURL:    "http://127.0.0.1:4200",
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handleUpdateTicket(context.Background(), nil, UpdateTicketInput{Title: "test"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestWorkerReadTicketRegisteredAndFunctional(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	updated := created.Add(30 * time.Minute)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/tickets/worker-ticket" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(t, w, http.StatusOK, domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID:            "worker-ticket",
				Status:        domain.TicketStatusProgress,
				Title:         "Worker Ticket",
				Repo:          "daemon",
				Created:       &created,
				Updated:       &updated,
				References:    []string{},
				HasConclusion: false,
				Warnings:      []domain.TicketWarning{},
			},
			Body:       "worker ticket body",
			Conclusion: nil,
		})
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{
		DaemonURL:    ts.URL,
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
		SessionType:  SessionTypeTicket,
		HTTPClient:   ts.Client(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	if server.SessionType() != SessionTypeTicket {
		t.Fatalf("session type = %q, want %q", server.SessionType(), SessionTypeTicket)
	}

	_, output, err := server.handleReadTicket(context.Background(), nil, ReadTicketInput{ID: "worker-ticket"})
	if err != nil {
		t.Fatalf("worker handleReadTicket failed: %v", err)
	}
	if output.ID != "worker-ticket" || output.Body != "worker ticket body" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func newTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{
		DaemonURL:    ts.URL,
		ArchitectKey: "hiveryn",
		SessionID:    "sess-test",
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
