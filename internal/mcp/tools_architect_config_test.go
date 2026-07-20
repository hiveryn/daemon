package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestHandleReadArchitectConfigSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/config" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(t, w, http.StatusOK, ReadArchitectConfigOutput{
			Config:  ArchitectConfigDoc{Repos: map[string]string{"daemon": "/repos/daemon"}},
			Version: "abc123",
		})
	})

	_, output, err := server.handleReadArchitectConfig(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("handleReadArchitectConfig: %v", err)
	}
	if output.Version != "abc123" || output.Config.Repos["daemon"] != "/repos/daemon" {
		t.Fatalf("output = %#v", output)
	}
}

func TestHandleUpdateArchitectConfigSendsBody(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/config" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body UpdateArchitectConfigInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Version != "v1" || body.Config.Repos["daemon"] != "/repos/daemon" {
			t.Fatalf("unexpected body: %#v", body)
		}
		writeEnvelope(t, w, http.StatusOK, UpdateArchitectConfigOutput{
			Config:  body.Config,
			Version: "v2",
			Created: []string{"/ws/prompts/k.md"},
		})
	})

	_, output, err := server.handleUpdateArchitectConfig(context.Background(), nil, UpdateArchitectConfigInput{
		Config:  ArchitectConfigDoc{Repos: map[string]string{"daemon": "/repos/daemon"}},
		Version: "v1",
	})
	if err != nil {
		t.Fatalf("handleUpdateArchitectConfig: %v", err)
	}
	if output.Version != "v2" || len(output.Created) != 1 {
		t.Fatalf("output = %#v", output)
	}
}

func TestHandleUpdateArchitectConfigVersionValidation(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn", SessionID: "sess-test"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	_, _, err = server.handleUpdateArchitectConfig(context.Background(), nil, UpdateArchitectConfigInput{Version: "  "})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleUpdateArchitectConfigMapsConflict(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusConflict, &domain.ErrorBody{
			Code:    string(domain.ErrCodeConflict),
			Message: "config changed since you read it",
		})
	})

	_, _, err := server.handleUpdateArchitectConfig(context.Background(), nil, UpdateArchitectConfigInput{
		Config:  ArchitectConfigDoc{},
		Version: "stale",
	})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeStateConflict {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeStateConflict)
	}
}

func TestHandleReadDefaultPromptValidation(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn", SessionID: "sess-test"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	_, _, err = server.handleReadDefaultPrompt(context.Background(), nil, ReadDefaultPromptInput{Kind: "bogus"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleReadDefaultPromptSendsKind(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/architects/hiveryn/config/default-prompt" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("kind"); got != "ticket-kickoff" {
			t.Fatalf("kind = %q", got)
		}
		writeEnvelope(t, w, http.StatusOK, ReadDefaultPromptOutput{
			Template:  "hello",
			Variables: []PromptVariableEntry{{Name: "TicketTitle", Description: "The ticket title."}},
		})
	})

	_, output, err := server.handleReadDefaultPrompt(context.Background(), nil, ReadDefaultPromptInput{Kind: "ticket-kickoff"})
	if err != nil {
		t.Fatalf("handleReadDefaultPrompt: %v", err)
	}
	if output.Template != "hello" || len(output.Variables) != 1 {
		t.Fatalf("output = %#v", output)
	}
}
