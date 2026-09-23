// Package workspacefs inspects an architect workspace: it validates the
// file-based project artifacts, discovers workflows, and renders the artifact
// schemas.
//
// Everything here is read-only and deterministic. Nothing in this package
// mutates the workspace, and nothing it reports prevents an architect from
// starting: a missing or malformed file is described precisely enough to be
// repaired, never treated as fatal. The checks are structural only — they say
// whether a document has the required shape, never whether its contents are
// true or current.
package workspacefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ConfigFileName          = "hiveryn.yaml"
	ProjectOverviewFileName = "PROJECT_OVERVIEW.md"
	ProjectStateFileName    = "PROJECT_STATE.md"
	RoadmapCurrentFileName  = "ROADMAP_CURRENT.md"
	ArchitectSystemFileName = "ARCHITECT_SYSTEM.md"

	WorkflowsDirName = "workflows"

	markdownExt = ".md"
)

// joinWorkspace joins a forward-slash workspace-relative path onto the
// workspace root. Report paths are always written with forward slashes so they
// are stable across platforms and directly comparable by clients.
func joinWorkspace(workspace, rel string) string {
	return filepath.Join(workspace, filepath.FromSlash(rel))
}

// errOutsideWorkspace reports that a path escapes the workspace once symlinks
// are resolved.
type errOutsideWorkspace struct {
	Path     string
	Resolved string
}

func (e *errOutsideWorkspace) Error() string {
	return fmt.Sprintf("%s resolves to %s, which is outside the architect workspace", e.Path, e.Resolved)
}

// canonicalInsideWorkspace resolves abs through any symlinks and confirms the
// result still lies inside the workspace.
//
// This is the path boundary for every workspace artifact. A selected workflow
// is recorded and later read by its canonical path, so a symlink pointing out
// of the workspace would silently widen what a worker session loads. Such a
// path is reported as an error and excluded rather than followed.
//
// A path that does not exist yet has nothing to resolve, so it is returned
// cleaned; the caller reports the missing file separately.
func canonicalInsideWorkspace(workspace, abs string) (string, error) {
	workspaceReal, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		if os.IsNotExist(err) {
			// The workspace itself is missing; the caller reports that. Fall
			// back to the cleaned path so no bogus boundary error is raised.
			return filepath.Clean(abs), nil
		}
		return "", fmt.Errorf("resolve architect workspace %q: %w", workspace, err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Clean(abs), nil
		}
		return "", fmt.Errorf("resolve %q: %w", abs, err)
	}

	if resolved != workspaceReal && !strings.HasPrefix(resolved, workspaceReal+string(os.PathSeparator)) {
		return "", &errOutsideWorkspace{Path: abs, Resolved: resolved}
	}
	return resolved, nil
}
