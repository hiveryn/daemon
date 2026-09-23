package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckWorkspaceRoutesToDaemon(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeEnvelope(t, w, http.StatusOK, domain.WorkspaceReport{
			ArchitectKey:  "hiveryn",
			WorkspacePath: "/workspace",
			CheckedAt:     time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC),
			Valid:         false,
			Nodes: []domain.WorkspaceNode{{
				Kind:     domain.ArtifactProjectState,
				Type:     domain.WorkspaceNodeFile,
				Path:     "PROJECT_STATE.md",
				Required: true,
				Diagnostics: []domain.WorkspaceDiagnostic{{
					Code:     domain.DiagMissingRequiredFile,
					Severity: domain.DiagnosticError,
					Path:     "PROJECT_STATE.md",
					Message:  "PROJECT_STATE.md is required but does not exist",
				}},
			}},
			Tickets: domain.WorkspaceTickets{
				Totals:   domain.WorkspaceTicketTotals{Backlog: 2, Progress: 1, Done: 4},
				Warnings: []domain.WorkspaceTicketWarning{{TicketID: "t-1", Code: "BROKEN_REFERENCE", Message: "broken"}},
			},
		})
	})

	_, output, err := server.handleCheckWorkspace(context.Background(), nil, CheckWorkspaceInput{})
	if err != nil {
		t.Fatalf("handleCheckWorkspace: %v", err)
	}

	// The workspace is resolved from the session's architect key; the agent
	// passes nothing and cannot address another workspace.
	if want := "/api/architects/hiveryn/workspace/check"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if output.Valid {
		t.Error("expected the report's invalid verdict to survive the round trip")
	}
	if len(output.Nodes) != 1 || output.Nodes[0].Diagnostics[0].Code != domain.DiagMissingRequiredFile {
		t.Errorf("nodes = %+v", output.Nodes)
	}
	if output.Tickets.Totals.Done != 4 || len(output.Tickets.Warnings) != 1 {
		t.Errorf("tickets = %+v", output.Tickets)
	}
}

func TestCheckWorkspacePropagatesDaemonErrors(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusNotFound, &domain.ErrorBody{
			Code:    string(domain.ErrCodeNotFound),
			Message: "architect hiveryn not found",
		})
	})

	_, _, err := server.handleCheckWorkspace(context.Background(), nil, CheckWorkspaceInput{})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T: %v", err, err)
	}
	if toolErr.Code != ErrorCodeNotFound {
		t.Errorf("code = %q", toolErr.Code)
	}
}

func TestDescribeArtifactRoutesToDaemon(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeEnvelope(t, w, http.StatusOK, domain.ArtifactSchema{
			Kind:          domain.ArtifactWorkflow,
			SchemaVersion: 1,
			Title:         "Workflow",
			Fields:        []domain.ArtifactField{{Name: "attach", Required: true, Type: domain.ArtifactFieldEnum}},
			Rules:         []string{"Readable UTF-8 markdown with a nonempty body."},
			Example:       "---\nattach: manual\n---\n\n# Example\n",
		})
	})

	_, output, err := server.handleDescribeArtifact(context.Background(), nil, DescribeArtifactInput{Kind: "WORKFLOW"})
	if err != nil {
		t.Fatalf("handleDescribeArtifact: %v", err)
	}
	if want := "/api/architects/hiveryn/workspace/artifacts/WORKFLOW"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if output.Kind != domain.ArtifactWorkflow || output.Example == "" {
		t.Errorf("output = %+v", output)
	}
}

// The kind is validated before any request is made, so a bad kind costs no
// round trip and yields a message naming the valid values.
func TestDescribeArtifactRejectsUnknownKindWithoutCallingDaemon(t *testing.T) {
	t.Parallel()

	called := false
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeEnvelope(t, w, http.StatusOK, domain.ArtifactSchema{})
	})

	for _, kind := range []string{"", "TICKET", "CONCLUSION", "workflow"} {
		_, _, err := server.handleDescribeArtifact(context.Background(), nil, DescribeArtifactInput{Kind: kind})
		toolErr, ok := err.(*ToolError)
		if !ok {
			t.Fatalf("%q: expected ToolError, got %T", kind, err)
		}
		if toolErr.Code != ErrorCodeValidation {
			t.Errorf("%q: code = %q", kind, toolErr.Code)
		}
	}
	if called {
		t.Error("an invalid kind must not reach the daemon")
	}
}

func TestDescribeArtifactAcceptsEveryKind(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusOK, domain.ArtifactSchema{Kind: domain.ArtifactWorkflow})
	})

	for _, kind := range domain.ArtifactKinds() {
		if _, _, err := server.handleDescribeArtifact(context.Background(), nil, DescribeArtifactInput{Kind: string(kind)}); err != nil {
			t.Errorf("%s rejected: %v", kind, err)
		}
	}
}

// The workspace tools are architect-only: a worker maintains no workspace.
func TestWorkspaceToolsAreArchitectOnly(t *testing.T) {
	t.Parallel()

	for _, sessionType := range []SessionType{SessionTypeArchitect, SessionTypeTicket} {
		server, err := NewServer(Config{
			DaemonURL:    "http://127.0.0.1:4200",
			ArchitectKey: "hiveryn",
			SessionID:    "sess-test",
			SessionType:  sessionType,
		})
		if err != nil {
			t.Fatalf("NewServer(%s): %v", sessionType, err)
		}

		tools := registeredToolNames(t, server)
		wantRegistered := sessionType == SessionTypeArchitect
		for _, name := range []string{"checkWorkspace", "describeArtifact"} {
			if got := tools[name]; got != wantRegistered {
				t.Errorf("%s session: %s registered = %v, want %v", sessionType, name, got, wantRegistered)
			}
		}
		// Prompts are built in and only the user launches workers: no session
		// type gets a config-authoring, prompt or spawn tool.
		for _, name := range []string{"readArchitectConfig", "updateArchitectConfig", "readDefaultPrompt", "listAgentProfiles", "spawnTicketSession"} {
			if tools[name] {
				t.Errorf("%s session: removed tool %s is still registered", sessionType, name)
			}
		}
	}
}

// TestCheckWorkspaceOutputValidatesWithNullDocumentTimestamp exercises the real
// MCP output-validation boundary (not just the handler): a node reports no
// document-declared timestamp — as ARCHITECT_SYSTEM.md and a freshly
// discovered workflow legitimately do — while ModifiedAt still carries the
// filesystem mtime. The generated output schema must accept the resulting
// JSON null rather than rejecting the whole response.
func TestCheckWorkspaceOutputValidatesWithNullDocumentTimestamp(t *testing.T) {
	t.Parallel()

	modifiedAt := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusOK, domain.WorkspaceReport{
			ArchitectKey:  "hiveryn",
			WorkspacePath: "/workspace",
			CheckedAt:     time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC),
			Valid:         true,
			Nodes: []domain.WorkspaceNode{{
				Kind:              domain.ArtifactArchitectSystem,
				Type:              domain.WorkspaceNodeFile,
				Path:              "ARCHITECT_SYSTEM.md",
				Required:          false,
				Exists:            true,
				Valid:             true,
				DocumentUpdatedAt: nil,
				ModifiedAt:        &modifiedAt,
				Children: []domain.WorkspaceEntry{{
					Kind:              domain.ArtifactWorkflow,
					Path:              "workflows/example.md",
					Valid:             true,
					DocumentUpdatedAt: nil,
					ModifiedAt:        &modifiedAt,
				}},
			}},
		})
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverSession, err := server.mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "checkWorkspace"})
	if err != nil {
		t.Fatalf("CallTool(checkWorkspace): %v", err)
	}
	if result.IsError {
		t.Fatalf("checkWorkspace returned a tool error: %+v", result.Content)
	}

	structuredJSON, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var report domain.WorkspaceReport
	if err := json.Unmarshal(structuredJSON, &report); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if got := len(report.Nodes); got != 1 {
		t.Fatalf("nodes = %d, want 1", got)
	}
	node := report.Nodes[0]
	if node.DocumentUpdatedAt != nil {
		t.Errorf("node.DocumentUpdatedAt = %v, want nil", node.DocumentUpdatedAt)
	}
	if node.ModifiedAt == nil || !node.ModifiedAt.Equal(modifiedAt) {
		t.Errorf("node.ModifiedAt = %v, want %v", node.ModifiedAt, modifiedAt)
	}
	if len(node.Children) != 1 || node.Children[0].DocumentUpdatedAt != nil {
		t.Errorf("children = %+v", node.Children)
	}
}

// registeredToolNames connects an in-memory MCP client to the server and lists
// the tools it actually advertises, so role scoping is asserted against the
// real registration rather than against the registration code.
func registeredToolNames(t *testing.T, server *Server) map[string]bool {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverSession, err := server.mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	result, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	names := make(map[string]bool, len(result.Tools))
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	return names
}
