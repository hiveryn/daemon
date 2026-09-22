package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// workspaceHandler serves the read-only architect workspace surface: the
// structural check, the artifact schemas, and workflow discovery.
//
// Every route here is a GET and mutates nothing. A workspace with missing or
// invalid files still answers 200 with a report describing the problems —
// the request succeeded, and what it found is the payload. A non-200 here
// means the request itself could not be served (unknown architect, unreadable
// filesystem), never that the workspace is broken.
type workspaceHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
	workspaces   domain.WorkspaceService
}

func (h *workspaceHandler) check(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := resolveArchitectWorkspace(w, r, h.config, h.configSource)
	if !ok {
		return
	}

	report, err := h.workspaces.CheckWorkspace(r.Context(), key, workspace)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, report)
}

// workerPreflight answers whether a ticket session could be launched into this
// workspace right now. It is the launch's own project-context validation, not
// the aggregate workspace verdict: a workspace the check calls invalid is still
// launchable when only architect-only artifacts or unselected workflows are
// broken. Selected-workflow validity comes from the workflow listing and is
// revalidated authoritatively at launch.
func (h *workspaceHandler) workerPreflight(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := resolveArchitectWorkspace(w, r, h.config, h.configSource)
	if !ok {
		return
	}

	preflight, err := h.workspaces.PreflightWorker(r.Context(), key, workspace)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, preflight)
}

func (h *workspaceHandler) listWorkflows(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := resolveArchitectWorkspace(w, r, h.config, h.configSource)
	if !ok {
		return
	}

	list, err := h.workspaces.ListWorkflows(r.Context(), key, workspace, parseRepoScope(r.URL.Query().Get("repos")))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, list)
}

// describeArtifact serves one artifact schema. It is architect-scoped by route
// for symmetry with the rest of the surface, but the schemas themselves are
// global: they describe the artifact format, not this workspace's contents.
func (h *workspaceHandler) describeArtifact(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := resolveArchitectWorkspace(w, r, h.config, h.configSource); !ok {
		return
	}

	kind := domain.ArtifactKind(strings.TrimSpace(r.PathValue("kind")))
	if !kind.Valid() {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation),
			"unknown artifact kind "+string(kind)+"; must be one of: "+strings.Join(artifactKindNames(), ", "), map[string]string{
				"field": "kind",
			})
		return
	}

	schema, err := h.workspaces.DescribeArtifact(r.Context(), kind)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, schema)
}

// parseRepoScope splits the comma-separated `repos` query parameter into the
// writable repo scope suggestions are matched against. An absent or empty
// parameter is an empty scope, which suggests nothing.
func parseRepoScope(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	repos := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			repos = append(repos, trimmed)
		}
	}
	return repos
}

func artifactKindNames() []string {
	kinds := domain.ArtifactKinds()
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	return names
}

// resolveArchitectWorkspace resolves the workspace path for the {key} path
// param from fresh config, writing a 404 and returning ok=false when the
// architect is unknown.
func resolveArchitectWorkspace(w http.ResponseWriter, r *http.Request, cfg config.Config, source config.Source) (key, workspace string, ok bool) {
	current, err := currentConfig(cfg, source)
	if err != nil {
		writeDomainError(w, r, err)
		return "", "", false
	}
	key = r.PathValue("key")
	architect, exists := current.Architects[key]
	if !exists {
		writeArchitectNotFound(w, r, key)
		return "", "", false
	}
	return key, architect.Path, true
}
