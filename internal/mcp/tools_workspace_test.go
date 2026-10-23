package mcp

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"testing"
	"time"
)

func TestWorkspaceToolsAreNotExposed(t *testing.T) {
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
		wantRegistered := false
		for _, name := range []string{"checkWorkspace", "describeArtifact", "createWorkTicket"} {
			if got := tools[name]; got != wantRegistered {
				t.Errorf("%s session: %s registered = %v, want %v", sessionType, name, got, wantRegistered)
			}
		}
		// Prompts are built in: no session type gets a config-authoring or
		// prompt tool, and the obsolete spawnTicketSession contract stays
		// gone (architects get spawnTicketWorker instead).
		for _, name := range []string{"readArchitectConfig", "updateArchitectConfig", "readDefaultPrompt", "listAgentProfiles", "spawnTicketSession"} {
			if tools[name] {
				t.Errorf("%s session: removed tool %s is still registered", sessionType, name)
			}
		}
	}
}

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
