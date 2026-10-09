package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hiveryn/daemon/internal/config"
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

// defaultDiffTimeout bounds one diff computation. A remote repository's diff is
// several SSH round trips, routinely longer than an ordinary API call; the bound
// keeps a stalled connection from holding the request open indefinitely. The
// desktop waits longer than this, so the daemon's own error is what it shows.
const defaultDiffTimeout = 60 * time.Second

func (h *reposHandler) diffBound() time.Duration {
	if h.diffTimeout > 0 {
		return h.diffTimeout
	}
	return defaultDiffTimeout
}

// diffContext bounds a diff and routes its Git commands to the repository's
// machine. The diff is read-only, so it stays tied to the requester too: a
// client that stops waiting cancels it.
func (h *reposHandler) diffContext(parent context.Context, cfg config.Config, machine string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, h.diffBound())
	return gitdiff.WithSSH(ctx, cfg.Machines[machine].SSH), cancel
}

// diffError names an exceeded bound and the machine, keeping the original
// cause (SSH or Git stderr) intact; domain errors pass through unchanged.
func (h *reposHandler) diffError(ctx context.Context, repoKey, machine string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		where := ""
		if machine != "" {
			where = " on machine " + machine
		}
		return fmt.Errorf("git diff of repo %s%s did not complete within %s: %w", repoKey, where, h.diffBound(), err)
	}
	return err
}

type diffResponse struct {
	Repo     string          `json:"repo"`
	RepoPath string          `json:"repo_path"`
	Files    []gitdiff.File  `json:"files"`
	Summary  gitdiff.Summary `json:"summary"`
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

	ctx, cancel := h.diffContext(r.Context(), cfg, repo.Machine)
	defer cancel()
	result, err := gitdiff.LoadWorkingTreeDiff(ctx, repo.Path)
	if err != nil {
		err = h.diffError(ctx, repoKey, repo.Machine, err)
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

	ctx, cancel := h.diffContext(r.Context(), cfg, repo.Machine)
	defer cancel()
	result, err := gitdiff.LoadCommitDiff(ctx, repo.Path, sha)
	if err != nil {
		err = h.diffError(ctx, repoKey, repo.Machine, err)
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
