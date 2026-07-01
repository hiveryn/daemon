package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestHandleListReposSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/config/repos" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(t, w, http.StatusOK, map[string]any{
			"repos": []RepoConfigEntry{{Key: "daemon", Path: "/repos/daemon"}},
		})
	})

	_, output, err := server.handleListRepos(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("handleListRepos: %v", err)
	}
	if len(output.Repos) != 1 || output.Repos[0].Key != "daemon" {
		t.Fatalf("output = %#v", output)
	}
}

func TestHandleAddKickoffSendsBody(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/config/kickoffs" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body struct {
			Path  string   `json:"path"`
			Repos []string `json:"repos"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Path != "prompts/k.md" || len(body.Repos) != 1 || body.Repos[0] != "daemon" {
			t.Fatalf("unexpected body: %#v", body)
		}
		writeEnvelope(t, w, http.StatusOK, AddKickoffOutput{
			Path: "/ws/prompts/k.md", Repos: []string{"daemon"}, Default: false, Created: true,
		})
	})

	_, output, err := server.handleAddKickoff(context.Background(), nil, AddKickoffInput{
		Path: "prompts/k.md", Repos: []string{"daemon"},
	})
	if err != nil {
		t.Fatalf("handleAddKickoff: %v", err)
	}
	if !output.Created || output.Path != "/ws/prompts/k.md" {
		t.Fatalf("output = %#v", output)
	}
}

func TestHandleDescribePromptSchemaValidation(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	_, _, err = server.handleDescribePromptSchema(context.Background(), nil, DescribePromptSchemaInput{Kind: "bogus"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleAddRepoValidation(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	_, _, err = server.handleAddRepo(context.Background(), nil, AddRepoInput{Key: "", Path: "/x"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestHandleRemoveRepoMapsConflict(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/config/repos/daemon" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeErrorEnvelope(t, w, http.StatusConflict, &domain.ErrorBody{
			Code:    string(domain.ErrCodeConflict),
			Message: "repo referenced by a kickoff entry",
		})
	})

	_, _, err := server.handleRemoveRepo(context.Background(), nil, RemoveRepoInput{Key: "daemon"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeStateConflict {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeStateConflict)
	}
}
