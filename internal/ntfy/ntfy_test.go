package ntfy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
)

func TestPublishPostsJSONWithAuth(t *testing.T) {
	var got publishBody
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	p := New(config.NtfyConfig{Server: srv.URL, Topic: "hiveryn-test", Token: "tk_secret"}, srv.Client())
	if err := p.Publish(context.Background(), Message{Title: "Hiveryn · architect", Body: "Déjà vu ✓", Priority: PriorityHigh, Tags: []string{"question"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if auth != "Bearer tk_secret" {
		t.Fatalf("Authorization = %q", auth)
	}
	if got.Topic != "hiveryn-test" || got.Title != "Hiveryn · architect" || got.Message != "Déjà vu ✓" || got.Priority != PriorityHigh || len(got.Tags) != 1 {
		t.Fatalf("body = %+v", got)
	}

	basic := New(config.NtfyConfig{Server: srv.URL, Topic: "t", Username: "kareem", Password: "pw"}, srv.Client())
	if err := basic.Publish(context.Background(), Message{Body: "x"}); err != nil {
		t.Fatalf("Publish basic: %v", err)
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("basic Authorization = %q", auth)
	}
}

func TestPublishErrorsKeepServerReasonAndHideCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":40301,"http":403,"error":"forbidden","link":"https://ntfy.sh/docs/publish/#authentication"}`))
	}))
	defer srv.Close()

	p := New(config.NtfyConfig{Server: srv.URL, Topic: "t", Token: "tk_secret", Password: "pw_secret"}, srv.Client())
	err := p.Publish(context.Background(), Message{Body: "x"})
	if err == nil {
		t.Fatal("refused publication reported success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTP 403") || !strings.Contains(msg, "forbidden") || !strings.Contains(msg, "notifications.ntfy token") {
		t.Fatalf("error lacks context: %s", msg)
	}
	if strings.Contains(msg, "tk_secret") || strings.Contains(msg, "pw_secret") {
		t.Fatalf("error leaks credentials: %s", msg)
	}

	srv.Close()
	err = p.Publish(context.Background(), Message{Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "tk_secret") {
		t.Fatalf("unreachable error = %v", err)
	}
}
