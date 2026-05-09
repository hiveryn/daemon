package api

import (
	"net/http"
	"testing"
)

func TestArchitectsReadOnlyAPI(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/architects", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected architect list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		Architects []architectResponse `json:"architects"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.Architects) != 2 {
		t.Fatalf("expected 2 architects, got %d", len(listed.Architects))
	}
	if listed.Architects[0].Key != "hiveryn" {
		t.Fatalf("expected sorted architect keys, got %#v", listed.Architects)
	}
	if len(listed.Architects[0].Repos) != 2 {
		t.Fatalf("expected 2 repos on architect list payload, got %#v", listed.Architects[0])
	}
	if listed.Architects[0].Repos[0].Key != "daemon" || listed.Architects[0].Repos[1].Key != "desktop" {
		t.Fatalf("expected sorted repos on architect list payload, got %#v", listed.Architects[0].Repos)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected architect get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var architect architectResponse
	decodeEnvelopeData(t, getBody, &architect)
	if architect.Key != "hiveryn" || architect.Group != "personal" {
		t.Fatalf("unexpected architect payload: %#v", architect)
	}
	if len(architect.Repos) != 2 {
		t.Fatalf("expected 2 repos on architect detail, got %#v", architect.Repos)
	}
}

func TestArchitectGroupsReadOnlyAPI(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/architect-groups", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected group list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		ArchitectGroups []architectGroupResponse `json:"architect_groups"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.ArchitectGroups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(listed.ArchitectGroups))
	}
	if listed.ArchitectGroups[0].Name != "personal" {
		t.Fatalf("unexpected group payload: %#v", listed.ArchitectGroups[0])
	}
	if len(listed.ArchitectGroups[0].Architects) != 2 {
		t.Fatalf("expected 2 architects in group payload, got %#v", listed.ArchitectGroups[0].Architects)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/architect-groups/personal", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected group get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var group architectGroupResponse
	decodeEnvelopeData(t, getBody, &group)
	if group.Name != "personal" || len(group.Architects) != 2 {
		t.Fatalf("unexpected group detail payload: %#v", group)
	}
}

func TestReposReadOnlyAPI(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected repo list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		Repos []repoResponse `json:"repos"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(listed.Repos))
	}
	if listed.Repos[0].Key != "daemon" {
		t.Fatalf("expected sorted repos, got %#v", listed.Repos)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/desktop", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected repo get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var repo repoResponse
	decodeEnvelopeData(t, getBody, &repo)
	if repo.Key != "desktop" || repo.Path == "" {
		t.Fatalf("unexpected repo payload: %#v", repo)
	}
}

func TestArchitectMutationEndpointsRemoved(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	cases := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/architect-groups"},
		{method: http.MethodPatch, path: "/api/architect-groups/personal"},
		{method: http.MethodDelete, path: "/api/architect-groups/personal"},
		{method: http.MethodPost, path: "/api/architects"},
		{method: http.MethodPatch, path: "/api/architects/hiveryn"},
		{method: http.MethodDelete, path: "/api/architects/hiveryn"},
		{method: http.MethodPost, path: "/api/architects/hiveryn/repos"},
		{method: http.MethodPatch, path: "/api/architects/hiveryn/repos/daemon"},
		{method: http.MethodDelete, path: "/api/architects/hiveryn/repos/daemon"},
	}

	for _, tc := range cases {
		status, _ := requestJSON(t, handler, tc.method, tc.path, map[string]any{"ignored": true})
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("expected %s %s to return %d, got %d", tc.method, tc.path, http.StatusMethodNotAllowed, status)
		}
	}
}

func TestArchitectAndRepoNotFound(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	cases := []string{
		"/api/architects/missing",
		"/api/architect-groups/missing",
		"/api/architects/missing/repos",
		"/api/architects/hiveryn/repos/missing",
	}

	for _, path := range cases {
		status, body := request(t, handler, http.MethodGet, path, nil)
		if status != http.StatusNotFound {
			t.Fatalf("expected %s to return %d, got %d: %s", path, http.StatusNotFound, status, string(body))
		}
	}
}
