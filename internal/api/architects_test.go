package api

import (
	"net/http"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestArchitectRegistrationAPI(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)
	architectPath := t.TempDir()
	repoPath := t.TempDir()

	groupStatus, groupBody := requestJSON(t, handler, http.MethodPost, "/api/architect-groups", map[string]any{"name": "Core"})
	if groupStatus != http.StatusCreated {
		t.Fatalf("expected group create status %d, got %d: %s", http.StatusCreated, groupStatus, string(groupBody))
	}

	var group domain.ArchitectGroup
	decodeEnvelopeData(t, groupBody, &group)

	architectStatus, architectBody := requestJSON(t, handler, http.MethodPost, "/api/architects", map[string]any{
		"path":     architectPath,
		"title":    "Hiveryn",
		"group_id": group.ID,
	})
	if architectStatus != http.StatusCreated {
		t.Fatalf("expected architect create status %d, got %d: %s", http.StatusCreated, architectStatus, string(architectBody))
	}

	var architect domain.Architect
	decodeEnvelopeData(t, architectBody, &architect)
	if architect.Group == nil || architect.Group.ID != group.ID {
		t.Fatalf("expected architect group %q, got %#v", group.ID, architect.Group)
	}

	repoStatus, repoBody := requestJSON(t, handler, http.MethodPost, "/api/architects/"+architect.ID+"/repos", map[string]any{
		"key":  "daemon",
		"path": repoPath,
	})
	if repoStatus != http.StatusCreated {
		t.Fatalf("expected repo create status %d, got %d: %s", http.StatusCreated, repoStatus, string(repoBody))
	}

	var repo domain.Repo
	decodeEnvelopeData(t, repoBody, &repo)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/architects", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected architect list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		Architects []domain.Architect `json:"architects"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.Architects) != 1 || listed.Architects[0].RepoCount != 1 {
		t.Fatalf("unexpected architect list payload: %#v", listed.Architects)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/architects/"+architect.ID, nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected architect get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var fetched domain.Architect
	decodeEnvelopeData(t, getBody, &fetched)
	if len(fetched.Repos) != 1 || fetched.Repos[0].ID != repo.ID {
		t.Fatalf("unexpected architect detail repos: %#v", fetched.Repos)
	}

	updateStatus, updateBody := requestJSON(t, handler, http.MethodPatch, "/api/architects/"+architect.ID, map[string]any{
		"title":    "Renamed",
		"group_id": nil,
	})
	if updateStatus != http.StatusOK {
		t.Fatalf("expected architect patch status %d, got %d: %s", http.StatusOK, updateStatus, string(updateBody))
	}

	var updated domain.Architect
	decodeEnvelopeData(t, updateBody, &updated)
	if updated.Title != "Renamed" || updated.Group != nil {
		t.Fatalf("unexpected updated architect: %#v", updated)
	}

	repoUpdateStatus, repoUpdateBody := requestJSON(t, handler, http.MethodPatch, "/api/architects/"+architect.ID+"/repos/"+repo.ID, map[string]any{"path": nil})
	if repoUpdateStatus != http.StatusBadRequest {
		t.Fatalf("expected repo patch validation status %d, got %d: %s", http.StatusBadRequest, repoUpdateStatus, string(repoUpdateBody))
	}

	errBody := decodeEnvelopeError(t, repoUpdateBody)
	if errBody.Code != string(domain.ErrCodeValidation) {
		t.Fatalf("expected repo path validation code %q, got %q", domain.ErrCodeValidation, errBody.Code)
	}

	repoListStatus, repoListBody := request(t, handler, http.MethodGet, "/api/architects/"+architect.ID+"/repos", nil)
	if repoListStatus != http.StatusOK {
		t.Fatalf("expected repo list status %d, got %d: %s", http.StatusOK, repoListStatus, string(repoListBody))
	}

	var repoList struct {
		Repos []domain.Repo `json:"repos"`
	}
	decodeEnvelopeData(t, repoListBody, &repoList)
	if len(repoList.Repos) != 1 || repoList.Repos[0].ID != repo.ID {
		t.Fatalf("unexpected repo list payload: %#v", repoList.Repos)
	}

	deleteStatus, deleteBody := request(t, handler, http.MethodDelete, "/api/architects/"+architect.ID, nil)
	if deleteStatus != http.StatusNoContent {
		t.Fatalf("expected architect delete status %d, got %d: %s", http.StatusNoContent, deleteStatus, string(deleteBody))
	}

	notFoundStatus, _ := request(t, handler, http.MethodGet, "/api/architects/"+architect.ID, nil)
	if notFoundStatus != http.StatusNotFound {
		t.Fatalf("expected deleted architect get status %d, got %d", http.StatusNotFound, notFoundStatus)
	}
}

func TestArchitectRegistrationAPIValidationAndConflict(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)
	architectPath := t.TempDir()

	status, body := requestJSON(t, handler, http.MethodPost, "/api/architects", map[string]any{"path": "relative/path"})
	if status != http.StatusBadRequest {
		t.Fatalf("expected validation status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != string(domain.ErrCodeValidation) {
		t.Fatalf("expected validation code %q, got %q", domain.ErrCodeValidation, errBody.Code)
	}

	status, _ = requestJSON(t, handler, http.MethodPost, "/api/architects", map[string]any{"path": architectPath})
	if status != http.StatusCreated {
		t.Fatalf("expected first architect create status %d, got %d", http.StatusCreated, status)
	}

	var created domain.Architect
	_, createdBody := requestJSON(t, handler, http.MethodPost, "/api/architects", map[string]any{"path": t.TempDir()})
	decodeEnvelopeData(t, createdBody, &created)

	status, body = requestJSON(t, handler, http.MethodPost, "/api/architects/"+created.ID+"/repos", map[string]any{"key": "daemon"})
	if status != http.StatusBadRequest {
		t.Fatalf("expected repo validation status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
	errBody = decodeEnvelopeError(t, body)
	if errBody.Code != string(domain.ErrCodeValidation) {
		t.Fatalf("expected repo validation code %q, got %q", domain.ErrCodeValidation, errBody.Code)
	}

	status, body = requestJSON(t, handler, http.MethodPost, "/api/architects", map[string]any{"path": architectPath})
	if status != http.StatusConflict {
		t.Fatalf("expected conflict status %d, got %d: %s", http.StatusConflict, status, string(body))
	}
	errBody = decodeEnvelopeError(t, body)
	if errBody.Code != string(domain.ErrCodeConflict) {
		t.Fatalf("expected conflict code %q, got %q", domain.ErrCodeConflict, errBody.Code)
	}
}
