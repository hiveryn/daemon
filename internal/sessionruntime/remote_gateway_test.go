package sessionruntime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/adapter/claude"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
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
	for _, name := range []string{"readTicket", "readContext", "listContext", "concludeTicketSession", "notify", "askQuestion"} {
		if !strings.Contains(result, `"name":"`+name+`"`) {
			t.Fatalf("missing %s: %s", name, result)
		}
	}
	for _, name := range []string{"spawnTicketWorker", "checkWorkspace", "concludeArchitectSession", "executeAction", "getActionResult"} {
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

// contextSessions is the one session lookup the context API needs.
type contextSessions struct {
	domain.SessionService
	sessions map[string]domain.Session
}

func (c contextSessions) GetSession(_ context.Context, id string) (domain.Session, error) {
	if s, ok := c.sessions[id]; ok {
		return s, nil
	}
	return domain.Session{}, &domain.NotFoundError{Resource: "session", ID: id}
}

// A remote worker, over its authenticated gateway, reads Overview-linked
// laptop context and a repo on another machine through the daemon API, while
// paths outside its own project are refused. The project comes from the
// gateway's fixed session, not from anything the worker sends.
func TestRemoteGatewayReadsProjectContext(t *testing.T) {
	sshtest.Install(t)
	workspace, remoteRepo, otherProject := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(workspace, "OVERVIEW.md"), "See architect:investigations/tracing.md.\n")
	writeFile(filepath.Join(workspace, "investigations", "tracing.md"), "laptop-only tracing notes\n")
	writeFile(filepath.Join(remoteRepo, "README.md"), "remote repo readme\n")
	writeFile(filepath.Join(otherProject, "OVERVIEW.md"), "other project secret\n")
	if err := os.Symlink(filepath.Join(otherProject, "OVERVIEW.md"), filepath.Join(workspace, "escape.md")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Machines: map[string]config.MachineConfig{"bk": {SSH: "bk-alias"}},
		Architects: map[string]config.ArchitectConfig{
			"project": {Path: workspace, Repos: map[string]string{"infra": remoteRepo}, RepoMachines: map[string]string{"infra": "bk"}},
			"other":   {Path: otherProject, Repos: map[string]string{"elsewhere": otherProject}},
		},
	}
	session := domain.Session{ID: "worker", ArchitectKey: "project", SessionType: domain.SessionTypeTicket, Machine: "bk", RemoteToken: strings.Repeat("c", 64)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	daemonAPI := httptest.NewServer(api.NewHandler(api.Dependencies{Config: cfg, ConfigSource: config.StaticSource(cfg), Logger: logger, Sessions: contextSessions{sessions: map[string]domain.Session{"worker": session}}}))
	defer daemonAPI.Close()
	service := &Service{baseURL: daemonAPI.URL, logger: logger, adapters: map[agentruntime.AgentKind]agentruntime.Adapter{}}
	server, port, err := service.remoteGateway(session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()

	callTool := func(token, name, args string) (int, string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/mcp", port), strings.NewReader(body))
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
	expect := func(name, args string, wants ...string) string {
		t.Helper()
		status, out := callTool(session.RemoteToken, name, args)
		if status != http.StatusOK {
			t.Fatalf("%s %s: %d %s", name, args, status, out)
		}
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Fatalf("%s %s lacks %q: %s", name, args, want, out)
			}
		}
		return out
	}

	expect("readContext", `{"reference":"architect:OVERVIEW.md"}`, `"status":"ok"`, "investigations/tracing.md", `"location":"local"`)
	expect("readContext", `{"reference":"architect:investigations/tracing.md"}`, "laptop-only tracing notes", `"sha256":"`)
	expect("listContext", `{"reference":"architect:investigations"}`, `"reference":"architect:investigations/tracing.md"`)
	expect("readContext", `{"reference":"infra:README.md"}`, "remote repo readme", `"location":"remote"`, `"machine":"bk"`)
	expect("readContext", `{"reference":"architect:escape.md"}`, `"status":"outside_root"`)
	for _, args := range []string{
		`{"reference":"` + filepath.Join(workspace, "OVERVIEW.md") + `"}`,
		`{"reference":"elsewhere:OVERVIEW.md"}`,
		`{"reference":"architect:../` + filepath.Base(otherProject) + `/OVERVIEW.md"}`,
	} {
		out := expect("readContext", args, `"isError":true`)
		if strings.Contains(out, "secret") || strings.Contains(out, "laptop-only") {
			t.Fatalf("refused read leaked content: %s", out)
		}
	}
	if status, _ := callTool(strings.Repeat("d", 64), "readContext", `{"reference":"architect:OVERVIEW.md"}`); status != http.StatusUnauthorized {
		t.Fatalf("wrong token status %d", status)
	}
}

// A remote Claude worker's asyncRewake waiter reaches the daemon's mailbox only
// through its own gateway's token path and only for its own session: a waiter
// naming another session, a wrong token or a GET never arms anything.
func TestRemoteGatewayWaiterIsBoundToItsSession(t *testing.T) {
	t.Parallel()
	messages := newMessageRuntime()
	t.Cleanup(messages.mailbox.Close)
	service := &Service{baseURL: "http://127.0.0.1:1", logger: slog.New(slog.NewTextHandler(io.Discard, nil)), messages: messages, adapters: map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentClaude: claude.New(claude.DefaultOptions())}}
	session := domain.Session{ID: "worker", ArchitectKey: "project", SessionType: domain.SessionTypeTicket, RemoteToken: strings.Repeat("c", 64)}
	server, port, err := service.remoteGateway(session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	url := func(token string) string {
		return fmt.Sprintf("http://127.0.0.1:%d/hooks/%s/claude/wait", port, token)
	}
	envelope := func(sessionID string) string {
		return `{"agent":"claude","hook":{"hook_event_name":"Stop","session_id":"native"},"env":{"AGENTRUNTIME_SESSION_ID":"` + sessionID + `"}}`
	}
	post := func(target, body string) int {
		t.Helper()
		resp, err := http.Post(target, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if status := post(url(session.RemoteToken), envelope("other-worker")); status != http.StatusBadRequest {
		t.Fatalf("cross-session waiter: %d", status)
	}
	if status := post(url(strings.Repeat("d", 64)), envelope("worker")); status != http.StatusUnauthorized {
		t.Fatalf("wrong token waiter: %d", status)
	}
	if resp, err := http.Get(url(session.RemoteToken)); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET waiter: %v %v", resp, err)
	}
	if _, armed := messages.mailbox.Armed("other-worker"); armed {
		t.Fatal("another session's waiter was armed")
	}
	got := make(chan string, 1)
	go func() {
		resp, err := http.Post(url(session.RemoteToken), "application/json", strings.NewReader(envelope("worker")))
		if err != nil {
			got <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		got <- fmt.Sprint(resp.StatusCode, " ", string(b))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, armed := messages.mailbox.Armed("worker"); armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("own waiter not armed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, delivered := messages.mailbox.Deliver("worker", ingest.MailboxMessage{ID: "m1", Text: "hello"}); !delivered {
		t.Fatal("not delivered to the armed waiter")
	}
	if answer := <-got; !strings.HasPrefix(answer, "200 ") || !strings.Contains(answer, `"text":"hello"`) {
		t.Fatalf("waiter answer = %s", answer)
	}
}
