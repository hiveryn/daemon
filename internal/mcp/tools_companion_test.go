package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestCompanionToolRegistriesAreRoleScoped(t *testing.T) {
	t.Parallel()
	companion, err := NewServer(Config{DaemonURL: "http://127.0.0.1:1", ArchitectKey: "hiveryn", SessionID: "c", SessionType: SessionTypeCompanion})
	if err != nil {
		t.Fatal(err)
	}
	names := registeredToolNames(t, companion)
	want := []string{"sendMessageToArchitect", "replyToArchitectMessage", "requestAction", "completeCompanionShutdown"}
	if len(names) != len(want) {
		t.Fatalf("companion tools = %v", names)
	}
	for _, name := range want {
		if !names[name] {
			t.Fatalf("companion lacks %s: %v", name, names)
		}
	}
	architect, err := NewServer(Config{DaemonURL: "http://127.0.0.1:1", ArchitectKey: "hiveryn", SessionID: "a", SessionType: SessionTypeArchitect})
	if err != nil {
		t.Fatal(err)
	}
	names = registeredToolNames(t, architect)
	for _, name := range []string{"sendMessageToCompanion", "replyToCompanionMessage", "requestCompanionShutdown", "readContext", "listContext"} {
		if !names[name] {
			t.Fatalf("architect lacks %s: %v", name, names)
		}
	}
	for _, name := range []string{"listCompanions", "requestAction", "completeCompanionShutdown", "sendMessageToArchitect"} {
		if names[name] {
			t.Fatalf("architect has %s", name)
		}
	}
	worker, err := NewServer(Config{DaemonURL: "http://127.0.0.1:1", ArchitectKey: "hiveryn", SessionID: "w", SessionType: SessionTypeTicket})
	if err != nil {
		t.Fatal(err)
	}
	for name := range registeredToolNames(t, worker) {
		if strings.Contains(strings.ToLower(name), "companion") || name == "requestAction" || name == "sendMessageToArchitect" {
			t.Fatalf("worker has companion tool %s", name)
		}
	}
}

func TestCompanionToolsCallTheirSessionRoutes(t *testing.T) {
	t.Parallel()
	var paths []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions"):
			switch body["title"] {
			case "timeout":
				_, _ = w.Write([]byte(`{"data":{"error_code":"ACTION_REQUEST_TIMEOUT","error":"` + domain.ActionRequestTimeoutText + `"}}`))
			case "reply":
				_, _ = w.Write([]byte(`{"data":{"outcome":"replied","message":"Use XYZ."}}`))
			default:
				_, _ = w.Write([]byte(`{"data":{"outcome":"accepted"}}`))
			}
		case strings.HasSuffix(r.URL.Path, "shutdown-completion"), strings.HasSuffix(r.URL.Path, "/shutdown"):
			_, _ = w.Write([]byte(`{"data":{"request_id":"` + body["request_id"].(string) + `","status":"accepted"}}`))
		case strings.HasSuffix(r.URL.Path, "/replies") && body["message_id"] == "unknown":
			_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"message_id unknown not found"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"message_id":"m-2","in_reply_to":"` + str(body["message_id"]) + `","status":"accepted"}}`))
		}
	}))
	t.Cleanup(daemon.Close)

	companion, err := NewServer(Config{DaemonURL: daemon.URL, ArchitectKey: "hiveryn", SessionID: "comp-1", SessionType: SessionTypeCompanion})
	if err != nil {
		t.Fatal(err)
	}
	cs := connectClient(t, companion)
	if text, isErr := callText(t, cs, "sendMessageToArchitect", map[string]any{"message": "done"}); isErr || !strings.Contains(text, `"message_id":"m-2"`) || !strings.Contains(text, `"status":"accepted"`) {
		t.Fatalf("send = %s", text)
	}
	if text, isErr := callText(t, cs, "replyToArchitectMessage", map[string]any{"message_id": "unknown", "message": "x"}); !isErr || !strings.Contains(text, "NOT_FOUND") {
		t.Fatalf("unknown reply = %s", text)
	}
	if text, isErr := callText(t, cs, "requestAction", map[string]any{"title": "ok", "description": "d"}); isErr || text != `{"outcome":"accepted"}` {
		t.Fatalf("accepted = %s", text)
	}
	if text, isErr := callText(t, cs, "requestAction", map[string]any{"title": "reply", "description": "d"}); isErr || text != `{"message":"Use XYZ.","outcome":"replied"}` {
		t.Fatalf("replied = %s", text)
	}
	if text, isErr := callText(t, cs, "requestAction", map[string]any{"title": "timeout", "description": "d"}); !isErr || !strings.HasPrefix(text, "ACTION_REQUEST_TIMEOUT: ") || !strings.Contains(text, "No action is authorized") {
		t.Fatalf("timeout = %s", text)
	}
	if text, isErr := callText(t, cs, "completeCompanionShutdown", map[string]any{"request_id": "id-1", "handoff": "h"}); isErr || !strings.Contains(text, `"request_id":"id-1"`) {
		t.Fatalf("complete = %s", text)
	}

	architect, err := NewServer(Config{DaemonURL: daemon.URL, ArchitectKey: "hiveryn", SessionID: "arch-1", SessionType: SessionTypeArchitect})
	if err != nil {
		t.Fatal(err)
	}
	as := connectClient(t, architect)
	if _, isErr := callText(t, as, "sendMessageToCompanion", map[string]any{"companion": "browser", "message": "go"}); isErr {
		t.Fatal("sendMessageToCompanion failed")
	}
	if _, isErr := callText(t, as, "replyToCompanionMessage", map[string]any{"message_id": "m-1", "message": "ok"}); isErr {
		t.Fatal("replyToCompanionMessage failed")
	}
	if _, isErr := callText(t, as, "requestCompanionShutdown", map[string]any{"companion": "browser", "request_id": "id-2"}); isErr {
		t.Fatal("requestCompanionShutdown failed")
	}
	want := []string{
		"POST /api/sessions/comp-1/companion/messages",
		"POST /api/sessions/comp-1/companion/replies",
		"POST /api/sessions/comp-1/actions",
		"POST /api/sessions/comp-1/actions",
		"POST /api/sessions/comp-1/actions",
		"POST /api/sessions/comp-1/companion/shutdown-completion",
		"POST /api/sessions/arch-1/companions/browser/messages",
		"POST /api/sessions/arch-1/companions/replies",
		"POST /api/sessions/arch-1/companions/shutdown",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes:\n%s", strings.Join(paths, "\n"))
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// A broken connection while waiting for a decision is a delivery error that
// authorizes nothing.
func TestRequestActionDeliveryFailureIsAToolError(t *testing.T) {
	t.Parallel()
	companion, err := NewServer(Config{DaemonURL: "http://127.0.0.1:1", ArchitectKey: "hiveryn", SessionID: "c", SessionType: SessionTypeCompanion})
	if err != nil {
		t.Fatal(err)
	}
	text, isErr := callText(t, connectClient(t, companion), "requestAction", map[string]any{"title": "t", "description": "d"})
	if !isErr || !strings.HasPrefix(text, string(ErrorCodeActionDeliveryFailed)) || !strings.Contains(text, "No action is authorized") {
		t.Fatalf("delivery failure = %s", text)
	}
}
