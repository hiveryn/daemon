package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestActionSessionRegistersOnlyActionTools(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", SessionID: "sess-action", SessionType: SessionTypeAction})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	names := registeredToolNames(t, server)
	got := make([]string, 0, len(names))
	for name := range names {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "concludeSession,readRecentConclusions" {
		t.Fatalf("action tools = %v", got)
	}

	if _, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn", SessionID: "sess-action", SessionType: SessionTypeAction}); err == nil {
		t.Fatal("action session accepted an architect key")
	}
}

func TestActionToolsCallSessionScopedEndpoints(t *testing.T) {
	t.Parallel()

	ended := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	var concluded domain.ConcludeActionRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/sessions/sess-action/action/conclusions":
			writeEnvelope(t, w, http.StatusOK, map[string]any{"conclusions": []domain.ActionConclusion{{ExecutionID: "run-1", Status: domain.ActionRunCompleted, Summary: "ok", EndedAt: &ended}}})
		case "POST /api/sessions/sess-action/intents/conclude-action":
			if err := json.NewDecoder(r.Body).Decode(&concluded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if concluded.Summary == "rejected" {
				writeEnvelope(t, w, http.StatusOK, map[string]any{"intent_id": "intent-1", "outcome": "denied_by_user", "reason": "add FRA"})
				return
			}
			writeEnvelope(t, w, http.StatusOK, map[string]any{"intent_id": "intent-2", "outcome": "auto_approved", "result": domain.ActionRun{ID: "run-2", Status: domain.ActionRunCompleted, OutputDir: "/out/run-2"}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(ts.Close)

	server, err := NewServer(Config{DaemonURL: ts.URL, SessionID: "sess-action", SessionType: SessionTypeAction, HTTPClient: ts.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	_, history, err := server.handleReadRecentActionConclusions(context.Background(), nil, ReadRecentActionConclusionsInput{})
	if err != nil || len(history.Conclusions) != 1 || history.Conclusions[0].ExecutionID != "run-1" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	if _, _, err := server.handleConcludeAction(context.Background(), nil, ConcludeActionInput{Outcome: "done", Summary: "x"}); err == nil {
		t.Fatal("invalid outcome accepted")
	}
	// A denial is a successful result carrying the verdict, not an error.
	_, denied, err := server.handleConcludeAction(context.Background(), nil, ConcludeActionInput{Outcome: "completed", Summary: "rejected"})
	if err != nil || denied.Outcome != "denied_by_user" || denied.Reason != "add FRA" || denied.Guidance == "" || denied.ExecutionID != "" {
		t.Fatalf("denied conclude = %+v, %v", denied, err)
	}
	_, out, err := server.handleConcludeAction(context.Background(), nil, ConcludeActionInput{Outcome: "completed", Summary: "delivered"})
	if err != nil || out.Outcome != "auto_approved" || out.ExecutionID != "run-2" || out.Status != "completed" || out.OutputDir != "/out/run-2" {
		t.Fatalf("conclude = %+v, %v", out, err)
	}
	if concluded.Outcome != domain.ActionConclusionCompleted || concluded.Summary != "delivered" {
		t.Fatalf("daemon received %+v", concluded)
	}
}
