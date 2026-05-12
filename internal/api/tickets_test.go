package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

func TestTicketsAPIFlow(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFixture(t, root, domain.TicketStatusBacklog, "2026-05-11-0913-fix-toctou-race-in-ptyfile-read-write-resize", "---\ntitle: Referenced ticket\n---\n\nbody\n")
	handler := newTicketTestHandler(t, root)

	createStatus, createBody := requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/tickets", map[string]any{
		"title":      "Implement board",
		"repo":       "daemon",
		"body":       "alpha\nbeta\n",
		"references": []string{"2026-05-11-0913-fix-toctou-race-in-ptyfile-read-write-resize"},
	})
	if createStatus != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createStatus, string(createBody))
	}

	var created domain.Ticket
	decodeEnvelopeData(t, createBody, &created)
	if created.ID == "" || created.Status != domain.TicketStatusBacklog || created.Title != "Implement board" {
		t.Fatalf("unexpected created ticket: %#v", created)
	}

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn/tickets", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var board domain.TicketBoard
	decodeEnvelopeData(t, listBody, &board)
	if len(board.Backlog) != 2 || board.Backlog[0].ID != created.ID {
		t.Fatalf("unexpected board payload: %#v", board)
	}

	editStatus, editBody := requestJSON(t, handler, http.MethodPatch, "/api/architects/hiveryn/tickets/"+created.ID, map[string]any{
		"oldString": "beta",
		"newString": "omega",
	})
	if editStatus != http.StatusOK {
		t.Fatalf("expected edit status %d, got %d: %s", http.StatusOK, editStatus, string(editBody))
	}

	var edited domain.Ticket
	decodeEnvelopeData(t, editBody, &edited)
	if edited.Body != "alpha\nomega\n" {
		t.Fatalf("unexpected edited ticket: %#v", edited)
	}

	metadataStatus, metadataBody := requestJSON(t, handler, http.MethodPatch, "/api/architects/hiveryn/tickets/"+created.ID+"/metadata", map[string]any{
		"title":      "Board implemented",
		"repo":       "desktop",
		"references": []string{"2026-05-11-0913-fix-toctou-race-in-ptyfile-read-write-resize"},
	})
	if metadataStatus != http.StatusOK {
		t.Fatalf("expected metadata status %d, got %d: %s", http.StatusOK, metadataStatus, string(metadataBody))
	}

	var metadataUpdated domain.Ticket
	decodeEnvelopeData(t, metadataBody, &metadataUpdated)
	if metadataUpdated.Title != "Board implemented" || metadataUpdated.Repo != "desktop" {
		t.Fatalf("unexpected metadata-updated ticket: %#v", metadataUpdated)
	}

	moveStatus, moveBody := requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/tickets/"+created.ID+"/move?to=progress", map[string]any{})
	if moveStatus != http.StatusOK {
		t.Fatalf("expected move status %d, got %d: %s", http.StatusOK, moveStatus, string(moveBody))
	}

	var moved domain.Ticket
	decodeEnvelopeData(t, moveBody, &moved)
	if moved.Status != domain.TicketStatusProgress {
		t.Fatalf("unexpected moved ticket: %#v", moved)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn/tickets/"+created.ID, nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var fetched domain.Ticket
	decodeEnvelopeData(t, getBody, &fetched)
	if fetched.Status != domain.TicketStatusProgress || fetched.Body != "alpha\nomega\n" || fetched.Title != "Board implemented" || fetched.Repo != "desktop" {
		t.Fatalf("unexpected fetched ticket: %#v", fetched)
	}

	deleteStatus, deleteBody := request(t, handler, http.MethodDelete, "/api/architects/hiveryn/tickets/"+created.ID, nil)
	if deleteStatus != http.StatusOK {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusOK, deleteStatus, string(deleteBody))
	}

	var deleted map[string]bool
	decodeEnvelopeData(t, deleteBody, &deleted)
	if !deleted["deleted"] {
		t.Fatalf("unexpected delete payload: %#v", deleted)
	}

	missingStatus, _ := request(t, handler, http.MethodGet, "/api/architects/hiveryn/tickets/"+created.ID, nil)
	if missingStatus != http.StatusNotFound {
		t.Fatalf("expected deleted ticket to be not found, got %d", missingStatus)
	}
}

func TestTicketsAPIValidationAndNotFound(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFixture(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-edit", "---\ntitle: Edit me\n---\n\nrepeat\nrepeat\n")
	handler := newTicketTestHandler(t, root)

	status, body := request(t, handler, http.MethodGet, "/api/architects/missing/tickets", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected missing architect status %d, got %d: %s", http.StatusNotFound, status, string(body))
	}

	status, body = requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/tickets", map[string]any{
		"title": "x",
		"extra": true,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected unknown field validation status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}

	status, body = requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/tickets", map[string]any{
		"title":      "bad refs",
		"references": []string{"missing-ticket"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected broken references validation status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}

	status, body = requestJSON(t, handler, http.MethodPatch, "/api/architects/hiveryn/tickets/2026-05-12-0900-edit/metadata", map[string]any{
		"title":      "",
		"references": []string{"missing-ticket"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected metadata validation status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}

	status, body = requestJSON(t, handler, http.MethodPatch, "/api/architects/hiveryn/tickets/2026-05-12-0900-edit", map[string]any{
		"oldString": "repeat",
		"newString": "done",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected ambiguous edit validation status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}

	status, body = requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/tickets/2026-05-12-0900-edit/move?to=invalid", map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("expected invalid move status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
}

func TestTicketResponsesAlwaysIncludeCollections(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFixture(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-no-refs", "---\ntitle: No refs or warnings\n---\n\nbody\n")
	handler := newTicketTestHandler(t, root)

	t.Run("board summaries include empty references and warnings arrays", func(t *testing.T) {
		_, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/tickets", nil)
		summary := firstBoardSummary(t, body)
		assertJSONArray(t, summary, "references")
		assertJSONArray(t, summary, "warnings")
	})

	t.Run("ticket detail includes null conclusion when absent", func(t *testing.T) {
		_, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/tickets/2026-05-12-0900-no-refs", nil)
		ticket := envelopeDataMap(t, body)
		assertJSONNull(t, ticket, "conclusion")
		assertJSONArray(t, ticket, "references")
		assertJSONArray(t, ticket, "warnings")
	})

	t.Run("ticket with conclusion includes empty commits array", func(t *testing.T) {
		writeTicketFixture(t, root, domain.TicketStatusDone, "2026-05-12-1000-has-conclusion", "---\ntitle: Has conclusion\n---\n\ndone\n")
		dir := filepath.Join(root, "tickets", string(domain.TicketStatusDone), "2026-05-12-1000-has-conclusion")
		if err := os.WriteFile(filepath.Join(dir, "conclusion.md"), []byte("---\nstarted_at: 2026-05-12T10:00:00Z\nconcluded_at: 2026-05-12T10:30:00Z\nrejected: false\n---\n\nsummary\n"), 0o644); err != nil {
			t.Fatalf("write conclusion: %v", err)
		}

		_, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/tickets/2026-05-12-1000-has-conclusion", nil)
		ticket := envelopeDataMap(t, body)
		conclusion, _ := ticket["conclusion"].(map[string]any)
		if conclusion == nil {
			t.Fatal("expected non-null conclusion")
		}
		assertJSONArrayRaw(t, conclusion, "commits")
	})
}

func firstBoardSummary(t *testing.T, body []byte) map[string]any {
	t.Helper()
	board := envelopeDataMap(t, body)
	backlog, _ := board["backlog"].([]any)
	if len(backlog) == 0 {
		t.Fatal("expected at least one backlog ticket")
	}
	summary, _ := backlog[0].(map[string]any)
	if summary == nil {
		t.Fatal("expected summary to be a JSON object")
	}
	return summary
}

func envelopeDataMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, string(body))
	}
	if env.Data == nil {
		t.Fatal("expected non-nil data in envelope")
	}
	return env.Data
}

func assertJSONArray(t *testing.T, obj map[string]any, key string) {
	t.Helper()
	assertJSONArrayRaw(t, obj, key)
}

func assertJSONArrayRaw(t *testing.T, obj map[string]any, key string) {
	t.Helper()
	val, exists := obj[key]
	if !exists {
		t.Fatalf("expected key %q to be present in JSON object, but it was omitted", key)
	}
	arr, ok := val.([]any)
	if !ok {
		t.Fatalf("expected key %q to be a JSON array, got type %T with value %v", key, val, val)
	}
	if arr == nil {
		t.Fatalf("expected key %q to be a non-null JSON array, but it was null", key)
	}
	// ensure empty arrays are non-nil (just checked above)
}

func assertJSONNull(t *testing.T, obj map[string]any, key string) {
	t.Helper()
	val, exists := obj[key]
	if !exists {
		t.Fatalf("expected key %q to be present in JSON object, but it was omitted", key)
	}
	if val != nil {
		t.Fatalf("expected key %q to be null, got %v", key, val)
	}
}

func newTicketTestHandler(t *testing.T, architectPath string) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig()
	cfg.Architects = cloneArchitects(cfg.Architects)
	architect := cfg.Architects["hiveryn"]
	architect.Path = architectPath
	cfg.Architects["hiveryn"] = architect

	return NewHandler(Dependencies{
		Config:  cfg,
		Logger:  logger,
		Tickets: architectfs.NewTicketService(),
	})
}

func cloneArchitects(input map[string]config.ArchitectConfig) map[string]config.ArchitectConfig {
	cloned := make(map[string]config.ArchitectConfig, len(input))
	for key, value := range input {
		repos := make(map[string]string, len(value.Repos))
		for repoKey, repoPath := range value.Repos {
			repos[repoKey] = repoPath
		}
		value.Repos = repos
		cloned[key] = value
	}
	return cloned
}

func writeTicketFixture(t *testing.T, root string, status domain.TicketStatus, id, content string) {
	t.Helper()
	dir := filepath.Join(root, "tickets", string(status), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ticket.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}
