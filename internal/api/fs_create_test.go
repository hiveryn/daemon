package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func fsWriteURL(path string, create bool) string {
	values := url.Values{"path": {path}}
	if create {
		values.Set("create", "true")
	}
	return "/api/fs/file?" + values.Encode()
}

func TestFsWriteCreateNewFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "notes", "new.md")

	handler := newFsTestHandler(t)
	status, body := requestJSON(t, handler, http.MethodPut, fsWriteURL(target, true), fsWriteRequest{Content: "hello\n"})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected created content: %q", string(data))
	}
}

func TestFsWriteCreateConflictsWhenExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "existing.txt")
	writeFsTestFile(t, target, "already here")

	handler := newFsTestHandler(t)
	status, body := requestJSON(t, handler, http.MethodPut, fsWriteURL(target, true), fsWriteRequest{Content: "clobber"})
	if status != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", status, string(body))
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "already here" {
		t.Fatalf("existing file was modified: %q", string(data))
	}
}

func TestFsWriteWithoutCreateStillRequiresExisting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "missing.txt")

	handler := newFsTestHandler(t)
	status, body := requestJSON(t, handler, http.MethodPut, fsWriteURL(target, false), fsWriteRequest{Content: "x"})
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", status, string(body))
	}
}
