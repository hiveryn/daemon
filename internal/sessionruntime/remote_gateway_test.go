package sessionruntime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/adapter/claude"
	"github.com/hiveryn/daemon/internal/domain"
)

func TestRemoteGatewayScopesToolsAndHooks(t *testing.T) {
	t.Parallel()
	service := &Service{baseURL: "http://127.0.0.1:1", logger: slog.New(slog.NewTextHandler(io.Discard, nil)), adapters: map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentClaude: claude.New(claude.DefaultOptions())}}
	session := domain.Session{ID: "worker", ArchitectKey: "project", SessionType: domain.SessionTypeTicket, RemoteToken: strings.Repeat("a", 64)}
	server, port, err := service.remoteGateway(session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	call := func(path, token, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		b, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(b)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for _, token := range []string{"", strings.Repeat("b", 64)} {
		if status, _ := call("/mcp", token, body); status != 401 {
			t.Fatalf("unauthenticated status %d", status)
		}
	}
	status, result := call("/mcp", session.RemoteToken, body)
	if status != 200 {
		t.Fatalf("%d %s", status, result)
	}
	for _, name := range []string{"readTicket", "createWorkTicket", "concludeTicketSession", "notify", "askQuestion", "executeAction"} {
		if !strings.Contains(result, `"name":"`+name+`"`) {
			t.Fatalf("missing %s: %s", name, result)
		}
	}
	for _, name := range []string{"spawnTicketWorker", "addAvailableAction", "checkWorkspace", "concludeArchitectSession"} {
		if strings.Contains(result, `"name":"`+name+`"`) {
			t.Fatalf("architect tool leaked: %s", name)
		}
	}
	if status, _ := call("/api/sessions", session.RemoteToken, body); status != 404 {
		t.Fatalf("API reachable: %d", status)
	}
	hook := `{"env":{"AGENTRUNTIME_SESSION_ID":"other-worker"},"hook":{"hook_event_name":"PreToolUse","session_id":"native","tool_name":"Read"}}`
	if status, _ := call("/hooks/"+session.RemoteToken+"/claude", "", hook); status != 403 {
		t.Fatalf("cross-worker hook accepted: %d", status)
	}
	if status, _ := call("/hooks/wrong/claude", "", hook); status != 401 {
		t.Fatalf("wrong hook token accepted: %d", status)
	}
}

func TestRemoteAttachmentSignalsAcrossChunks(t *testing.T) {
	ready := &remoteReady{marker: []byte("connected-unique"), endMarker: []byte("detached-unique"), ready: make(chan struct{})}
	detached := make(chan struct{}, 1)
	ready.onDetached = func() { detached <- struct{}{} }
	ready.Output([]byte("SSH failure detail"))
	if !strings.Contains(ready.failure(), "SSH failure detail") {
		t.Fatal("lost SSH error")
	}
	ready.Output([]byte("connected-"))
	ready.Output([]byte("unique"))
	select {
	case <-ready.ready:
	default:
		t.Fatal("missing ready signal")
	}
	ready.Output([]byte("private agent output"))
	if ready.failure() != "" {
		t.Fatal("agent output retained as SSH error")
	}
	ready.Output([]byte("detached-"))
	ready.Output([]byte("unique"))
	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("missing detach signal")
	}
}
