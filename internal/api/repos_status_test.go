package api

import (
	"net/http"
	"testing"
)

func TestReposStatusEndpoint(t *testing.T) {
	t.Parallel()

	handler := newGitDiffTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/daemon/status", nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload statusResponse
	decodeEnvelopeData(t, body, &payload)
	if payload.Repo != "daemon" || payload.RepoPath == "" {
		t.Fatalf("unexpected status payload: %#v", payload)
	}
	byPath := map[string][2]string{}
	for _, e := range payload.Entries {
		byPath[e.Path] = [2]string{e.Index, e.Worktree}
	}
	// The shared fixture stages an edit to file.txt and leaves untracked.txt.
	if cols, ok := byPath["file.txt"]; !ok || cols[0] != "M" {
		t.Fatalf("expected staged M for file.txt, got %#v", payload.Entries)
	}
	if cols, ok := byPath["untracked.txt"]; !ok || cols != [2]string{"?", "?"} {
		t.Fatalf("expected ?? for untracked.txt, got %#v", payload.Entries)
	}
}

func TestReposStatusEndpoint_RepoNotFound(t *testing.T) {
	t.Parallel()

	handler := newGitDiffTestHandler(t)

	status, _ := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/missing/status", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}
