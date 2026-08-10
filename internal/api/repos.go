package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/gitdiff"
)

func (h *reposHandler) list(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	architectKey := r.PathValue("key")
	repos, ok := listRepos(cfg, architectKey)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect "+architectKey+" not found", map[string]string{
			"resource": "architect",
			"id":       architectKey,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]repoResponse{"repos": repos})
}

func (h *reposHandler) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	architectKey := r.PathValue("key")
	repoKey := r.PathValue("repoKey")
	repo, architectExists, repoExists := getRepo(cfg, architectKey, repoKey)
	if !architectExists {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect "+architectKey+" not found", map[string]string{
			"resource": "architect",
			"id":       architectKey,
		})
		return
	}
	if !repoExists {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "repo "+repoKey+" not found", map[string]string{
			"resource": "repo",
			"id":       repoKey,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, repo)
}

type diffResponse struct {
	Repo     string          `json:"repo"`
	RepoPath string          `json:"repo_path"`
	Files    []gitdiff.File  `json:"files"`
	Summary  gitdiff.Summary `json:"summary"`
}

type statusResponse struct {
	Repo     string                `json:"repo"`
	RepoPath string                `json:"repo_path"`
	Entries  []gitdiff.StatusEntry `json:"entries"`
}

// status is the lightweight sibling of diff: just the changed paths with
// their porcelain status columns, for tree decoration — no diff text.
func (h *reposHandler) status(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	architectKey := r.PathValue("key")
	repoKey := r.PathValue("repoKey")
	repo, architectExists, repoExists := getRepo(cfg, architectKey, repoKey)
	if !architectExists {
		writeDomainError(w, r, &domain.NotFoundError{Resource: "architect", ID: architectKey})
		return
	}
	if !repoExists {
		writeDomainError(w, r, &domain.NotFoundError{Resource: "repo", ID: repoKey})
		return
	}

	repoRoot, err := gitdiff.RepoRoot(r.Context(), repo.Path)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	entries, err := gitdiff.Status(r.Context(), repo.Path)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, statusResponse{
		Repo:     repoKey,
		RepoPath: repoRoot,
		Entries:  entries,
	})
}

type commitDiffResponse struct {
	Repo      string          `json:"repo"`
	RepoPath  string          `json:"repo_path"`
	SHA       string          `json:"sha"`
	ParentSHA string          `json:"parent_sha,omitempty"`
	IsMerge   bool            `json:"is_merge"`
	Files     []gitdiff.File  `json:"files"`
	Summary   gitdiff.Summary `json:"summary"`
}

func (h *reposHandler) diff(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	architectKey := r.PathValue("key")
	repoKey := r.PathValue("repoKey")
	repo, architectExists, repoExists := getRepo(cfg, architectKey, repoKey)
	if !architectExists {
		writeDomainError(w, r, &domain.NotFoundError{Resource: "architect", ID: architectKey})
		return
	}
	if !repoExists {
		writeDomainError(w, r, &domain.NotFoundError{Resource: "repo", ID: repoKey})
		return
	}

	result, err := gitdiff.LoadWorkingTreeDiff(r.Context(), repo.Path)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, diffResponse{
		Repo:     repoKey,
		RepoPath: result.RepoPath,
		Files:    result.Files,
		Summary:  result.Summary,
	})
}

func (h *reposHandler) commitDiff(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	architectKey := r.PathValue("key")
	repoKey := r.PathValue("repoKey")
	sha := r.PathValue("sha")
	repo, architectExists, repoExists := getRepo(cfg, architectKey, repoKey)
	if !architectExists {
		writeDomainError(w, r, &domain.NotFoundError{Resource: "architect", ID: architectKey})
		return
	}
	if !repoExists {
		writeDomainError(w, r, &domain.NotFoundError{Resource: "repo", ID: repoKey})
		return
	}

	result, err := gitdiff.LoadCommitDiff(r.Context(), repo.Path, sha)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, commitDiffResponse{
		Repo:      repoKey,
		RepoPath:  result.RepoPath,
		SHA:       result.SHA,
		ParentSHA: result.ParentSHA,
		IsMerge:   result.IsMerge,
		Files:     result.Files,
		Summary:   result.Summary,
	})
}
