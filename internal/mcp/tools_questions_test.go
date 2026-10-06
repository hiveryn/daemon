package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectClient runs server over in-memory transports and returns a client
// session, so tests see exactly what an agent's MCP client sees.
func connectClient(t *testing.T, server *Server) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s returned %d content blocks", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s returned %T", name, res.Content[0])
	}
	return text.Text, res.IsError
}

func TestEverySessionTypeGetsNotifyAndAskQuestion(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{DaemonURL: "http://127.0.0.1:1", ArchitectKey: "hiveryn", SessionID: "s", SessionType: SessionTypeArchitect},
		{DaemonURL: "http://127.0.0.1:1", ArchitectKey: "hiveryn", SessionID: "s", SessionType: SessionTypeTicket},
		{DaemonURL: "http://127.0.0.1:1", SessionID: "s", SessionType: SessionTypeAction},
	} {
		server, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer(%s): %v", cfg.SessionType, err)
		}
		names := registeredToolNames(t, server)
		if !names["notify"] || !names["askQuestion"] {
			t.Fatalf("%s session tools lack notify/askQuestion: %v", cfg.SessionType, names)
		}
	}
}

func TestNotifyAndAskQuestionReturnExactText(t *testing.T) {
	t.Parallel()

	var asked domain.AskQuestionRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/sessions/sess-1/notify":
			var body domain.NotifyRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Message == "unconfigured" {
				writeErrorEnvelope(t, w, http.StatusConflict, &domain.ErrorBody{Code: string(domain.ErrCodeConflict), Message: "notifications ntfy: phone notifications are not configured"})
				return
			}
			writeEnvelope(t, w, http.StatusOK, domain.NotifyResult{Text: domain.NotificationSentText})
		case "POST /api/sessions/sess-1/questions":
			if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
				t.Errorf("decode: %v", err)
			}
			switch asked.Question {
			case "expire":
				writeEnvelope(t, w, http.StatusOK, domain.AskQuestionResult{QuestionID: "q2", Status: domain.QuestionExpired, Text: domain.QuestionTimeoutText})
			case "daemon dies":
				// Drop the connection mid-wait, as a stopping daemon does.
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			default:
				writeEnvelope(t, w, http.StatusOK, domain.AskQuestionResult{QuestionID: "q1", Status: domain.QuestionAnswered, Text: "Use Postgres, but keep SQLite for tests"})
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{DaemonURL: ts.URL, ArchitectKey: "hiveryn", SessionID: "sess-1", SessionType: SessionTypeTicket, HTTPClient: ts.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	session := connectClient(t, server)

	if text, isErr := callText(t, session, "notify", map[string]any{"shortMessage": "Build is green"}); isErr || text != "Notification sent to user" {
		t.Fatalf("notify = %q (error=%v)", text, isErr)
	}
	if text, isErr := callText(t, session, "notify", map[string]any{"shortMessage": "unconfigured"}); !isErr || !strings.Contains(text, "not configured") {
		t.Fatalf("unconfigured notify = %q (error=%v)", text, isErr)
	}

	text, isErr := callText(t, session, "askQuestion", map[string]any{"question": "Which DB?", "answers": []string{"Postgres", "SQLite"}, "recommendedIndex": 0})
	if isErr || text != "Use Postgres, but keep SQLite for tests" {
		t.Fatalf("askQuestion = %q (error=%v)", text, isErr)
	}
	if asked.RecommendedIndex == nil || *asked.RecommendedIndex != 0 || !slices.Equal(asked.Answers, []string{"Postgres", "SQLite"}) {
		t.Fatalf("daemon received %+v", asked)
	}
	if text, isErr := callText(t, session, "askQuestion", map[string]any{"question": "expire", "answers": []string{"a", "b"}, "recommendedIndex": 1}); isErr || text != "User didn't respond within 1 hour, stop here and wait for user to get back" {
		t.Fatalf("expired askQuestion = %q (error=%v)", text, isErr)
	}
	if text, isErr := callText(t, session, "askQuestion", map[string]any{"question": "daemon dies", "answers": []string{"a", "b"}, "recommendedIndex": 1}); !isErr || !strings.Contains(text, "no longer pending") {
		t.Fatalf("askQuestion on a dropped daemon = %q (error=%v)", text, isErr)
	}

	// answers and recommendedIndex reach the daemon's validation instead of a
	// client-side schema rejection, so the agent gets the precise message.
	if _, isErr := callText(t, session, "askQuestion", map[string]any{"question": "no answers"}); isErr {
		t.Fatal("fake daemon answers everything; a schema rejection would surface as a call error instead")
	}
	if asked.Answers != nil || asked.RecommendedIndex != nil {
		t.Fatalf("missing fields were invented: %+v", asked)
	}
}
