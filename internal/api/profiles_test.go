package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

func TestAgentProfileCRUD(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	createStatus, createBody := requestJSON(t, handler, http.MethodPost, "/api/agent-profiles", map[string]any{
		"name":       "Claude Plan",
		"agent_kind": "claude",
		"args":       []string{"--permission-mode", "plan"},
		"env": map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
		},
	})
	if createStatus != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createStatus, string(createBody))
	}

	var created domain.AgentProfile
	decodeEnvelopeData(t, createBody, &created)
	if created.ID == "" {
		t.Fatal("expected created profile ID")
	}

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/agent-profiles", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		AgentProfiles []domain.AgentProfile `json:"agent_profiles"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.AgentProfiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(listed.AgentProfiles))
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/agent-profiles/"+created.ID, nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var fetched domain.AgentProfile
	decodeEnvelopeData(t, getBody, &fetched)
	if fetched.Name != created.Name {
		t.Fatalf("expected fetched name %q, got %q", created.Name, fetched.Name)
	}

	updateStatus, updateBody := requestJSON(t, handler, http.MethodPut, "/api/agent-profiles/"+created.ID, map[string]any{
		"name":       "Codex Exec",
		"agent_kind": "codex",
		"args":       []string{"--model", "gpt-5-codex"},
		"env": map[string]string{
			"OPENAI_API_KEY": "redacted",
		},
	})
	if updateStatus != http.StatusOK {
		t.Fatalf("expected update status %d, got %d: %s", http.StatusOK, updateStatus, string(updateBody))
	}

	var updated domain.AgentProfile
	decodeEnvelopeData(t, updateBody, &updated)
	if updated.Name != "Codex Exec" || updated.AgentKind != domain.AgentKindCodex {
		t.Fatalf("unexpected updated profile: %#v", updated)
	}

	deleteStatus, deleteBody := request(t, handler, http.MethodDelete, "/api/agent-profiles/"+created.ID, nil)
	if deleteStatus != http.StatusNoContent {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusNoContent, deleteStatus, string(deleteBody))
	}

	notFoundStatus, _ := request(t, handler, http.MethodGet, "/api/agent-profiles/"+created.ID, nil)
	if notFoundStatus != http.StatusNotFound {
		t.Fatalf("expected deleted profile get status %d, got %d", http.StatusNotFound, notFoundStatus)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/health", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]string
	decodeEnvelopeData(t, body, &payload)
	if payload["status"] != "ok" {
		t.Fatalf("expected health status ok, got %q", payload["status"])
	}
}

func TestCreateAgentProfileDuplicateName(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	payload := map[string]any{
		"name":       "Duplicate Name",
		"agent_kind": "claude",
	}

	status, _ := requestJSON(t, handler, http.MethodPost, "/api/agent-profiles", payload)
	if status != http.StatusCreated {
		t.Fatalf("first create should return %d, got %d", http.StatusCreated, status)
	}

	status, body := requestJSON(t, handler, http.MethodPost, "/api/agent-profiles", payload)
	if status != http.StatusConflict {
		t.Fatalf("expected duplicate create status %d, got %d: %s", http.StatusConflict, status, string(body))
	}

	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != string(domain.ErrCodeConflict) {
		t.Fatalf("expected error code %q, got %q", domain.ErrCodeConflict, errBody.Code)
	}
	if errBody.Message == "" {
		t.Fatal("expected non-empty error message")
	}
	if errBody.Stacktrace == "" {
		t.Fatal("expected non-empty stacktrace")
	}
}

func TestCreateAgentProfileValidation(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/agent-profiles", map[string]any{
		"name":       "Invalid",
		"agent_kind": "unknown",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}

	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != string(domain.ErrCodeValidation) {
		t.Fatalf("expected error code %q, got %q", domain.ErrCodeValidation, errBody.Code)
	}
	if errBody.Stacktrace == "" {
		t.Fatal("expected non-empty stacktrace")
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := store.NewProfileStore(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(repo, logger)
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, body any) (int, []byte) {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return request(t, handler, method, path, bytes.NewReader(payload))
}

func request(t *testing.T, handler http.Handler, method, path string, body io.Reader) (int, []byte) {
	t.Helper()

	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, respBody
}

func decodeEnvelopeData(t *testing.T, body []byte, dst any) {
	t.Helper()

	var env struct {
		Data json.RawMessage `json:"data"`
		Meta domain.Meta     `json:"meta"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, string(body))
	}
	if env.Meta.RequestID == "" {
		t.Fatal("expected non-empty meta.request_id")
	}
	if dst != nil {
		if err := json.Unmarshal(env.Data, dst); err != nil {
			t.Fatalf("decode envelope data: %v\ndata: %s", err, string(env.Data))
		}
	}
}

func decodeEnvelopeError(t *testing.T, body []byte) domain.ErrorBody {
	t.Helper()

	var env struct {
		Error domain.ErrorBody `json:"error"`
		Meta  domain.Meta      `json:"meta"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope error: %v\nbody: %s", err, string(body))
	}
	if env.Meta.RequestID == "" {
		t.Fatal("expected non-empty meta.request_id")
	}
	if env.Error.Code == "" {
		t.Fatal("expected non-empty error code in envelope")
	}
	return env.Error
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
