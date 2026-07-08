package api

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/gitdiff"
)

// maxSearchResults caps how many matches a single /api/fs/search call
// returns (and is the default when no limit param is given). It's a var
// (not const) so tests can lower it.
var maxSearchResults = 100

// maxSearchCandidates caps how many candidate files a single search
// collects before stopping with truncated=true, bounding walk time and
// memory on pathological roots. It's a var (not const) so tests can
// lower it.
var maxSearchCandidates = 200_000

type fsSearchMatch struct {
	// Path is relative to the searched root, "/"-separated.
	Path string `json:"path"`
}

type fsSearchResponse struct {
	Root    string          `json:"root"`
	Query   string          `json:"query"`
	Matches []fsSearchMatch `json:"matches"`
	// Total counts all matches found, before the limit cap.
	Total int `json:"total"`
	// Truncated reports that candidate collection hit maxSearchCandidates,
	// so matches may be incomplete beyond the limit cap.
	Truncated bool `json:"truncated,omitempty"`
}

func (h *fsHandler) search(w http.ResponseWriter, r *http.Request) {
	root, ok := parseAbsPathParam(w, r)
	if !ok {
		return
	}
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "missing required query parameter: q", map[string]string{"field": "q"})
		return
	}
	limit := maxSearchResults
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxSearchResults {
			writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "limit must be an integer between 1 and "+strconv.Itoa(maxSearchResults), map[string]string{"field": "limit"})
			return
		}
		limit = parsed
	}

	info, err := os.Stat(root)
	if err != nil {
		writeFsOSError(w, r, root, err)
		return
	}
	if !info.IsDir() {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "path is not a directory: "+root, map[string]string{"field": "path"})
		return
	}

	candidates, truncated, err := collectSearchCandidates(r.Context(), root, maxSearchCandidates)
	if err != nil {
		writeFsOSError(w, r, root, err)
		return
	}

	matches, total := rankMatches(candidates, query, limit)
	writeJSON(w, r, http.StatusOK, fsSearchResponse{
		Root:      root,
		Query:     query,
		Matches:   matches,
		Total:     total,
		Truncated: truncated,
	})
}

// collectSearchCandidates gathers the file paths (relative to root) a search
// matches against. Git-aware: a root inside a work tree is listed with one
// `git ls-files` subprocess (tracked + untracked-unignored — the fast path
// and the reason each query needs no persistent index); any other root is
// walked, treating each nested directory containing a .git entry as a repo
// root listed the same way (so a workspace root spanning several repos still
// respects each repo's gitignore) and skipping .git directories themselves.
func collectSearchCandidates(ctx context.Context, root string, budget int) ([]string, bool, error) {
	if gitdiff.IsInsideWorkTree(ctx, root) {
		files, err := gitdiff.ListFiles(ctx, root)
		if err != nil {
			return nil, false, err
		}
		if len(files) > budget {
			return files[:budget], true, nil
		}
		return files, false, nil
	}

	var out []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if len(out) >= budget {
			truncated = true
			return fs.SkipAll
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			if path == root {
				return nil
			}
			// A .git entry (dir, or file for worktrees/submodules) marks a
			// nested repo root: list it via git and skip descending.
			if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
				files, listErr := gitdiff.ListFiles(ctx, path)
				if listErr != nil {
					return listErr
				}
				prefix := filepath.ToSlash(rel) + "/"
				for _, f := range files {
					if len(out) >= budget {
						truncated = true
						return fs.SkipAll
					}
					out = append(out, prefix+f)
				}
				return fs.SkipDir
			}
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	return out, truncated, nil
}

// rankMatches filters candidates against query (case-insensitive) and
// returns the best `limit` matches plus the total match count. Rank order:
// basename substring < basename subsequence < path substring < path
// subsequence; ties break on shorter path, then lexicographically.
func rankMatches(candidates []string, query string, limit int) ([]fsSearchMatch, int) {
	type scored struct {
		path  string
		score int
	}
	q := strings.ToLower(query)
	var hits []scored
	for _, p := range candidates {
		pl := strings.ToLower(p)
		base := pl[strings.LastIndexByte(pl, '/')+1:]
		var score int
		switch {
		case strings.Contains(base, q):
			score = 0
		case isSubsequence(q, base):
			score = 1
		case strings.Contains(pl, q):
			score = 2
		case isSubsequence(q, pl):
			score = 3
		default:
			continue
		}
		hits = append(hits, scored{path: p, score: score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score < hits[j].score
		}
		if len(hits[i].path) != len(hits[j].path) {
			return len(hits[i].path) < len(hits[j].path)
		}
		return hits[i].path < hits[j].path
	})
	total := len(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	matches := make([]fsSearchMatch, len(hits))
	for i, h := range hits {
		matches[i] = fsSearchMatch{Path: h.path}
	}
	return matches, total
}

func isSubsequence(needle, hay string) bool {
	i := 0
	for j := 0; j < len(hay) && i < len(needle); j++ {
		if hay[j] == needle[i] {
			i++
		}
	}
	return i == len(needle)
}
