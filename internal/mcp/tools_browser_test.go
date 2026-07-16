package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func newSessionScopedTestServer(t *testing.T, sessionID string, handler func(w http.ResponseWriter, r *http.Request)) *Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{
		DaemonURL:    ts.URL,
		ArchitectKey: "hiveryn",
		SessionID:    sessionID,
		HTTPClient:   ts.Client(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	return server
}

func TestHandlePreviewInBrowserTabCreate(t *testing.T) {
	t.Parallel()

	server := newSessionScopedTestServer(t, "session-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sessions/session-1/browser-tabs" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(t, w, http.StatusOK, domain.BrowserTabInfo{
			TabID:     "tab-1",
			SessionID: "session-1",
			Target:    "https://example.com",
		})
	})

	_, output, err := server.handlePreviewInBrowserTab(context.Background(), nil, PreviewInBrowserTabInput{Target: "https://example.com"})
	if err != nil {
		t.Fatalf("handlePreviewInBrowserTab failed: %v", err)
	}
	if output.TabID != "tab-1" || output.Target != "https://example.com" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestHandlePreviewInBrowserTabRequiresTarget(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn"})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	_, _, err = server.handlePreviewInBrowserTab(context.Background(), nil, PreviewInBrowserTabInput{})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleGetBrowserTabsFiltersType(t *testing.T) {
	t.Parallel()

	server := newSessionScopedTestServer(t, "session-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/sessions/session-1/tabs" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(t, w, http.StatusOK, []domain.SessionTab{
			{Type: "terminal", ID: "term-1"},
			{Type: "browser", ID: "tab-1", Target: "https://example.com"},
		})
	})

	_, output, err := server.handleGetBrowserTabs(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("handleGetBrowserTabs failed: %v", err)
	}
	if len(output.Tabs) != 1 || output.Tabs[0].TabID != "tab-1" || output.Tabs[0].Target != "https://example.com" {
		t.Fatalf("unexpected output: %#v", output)
	}
}
