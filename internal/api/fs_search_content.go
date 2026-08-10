package api

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/gitdiff"
)

// maxContentSearchResults caps how many matched lines a single
// /api/fs/search-content call returns (and is the default when no limit
// param is given). It's a var (not const) so tests can lower it.
var maxContentSearchResults = 200

// maxContentScanFileBytes caps how large a file the non-git fallback scanner
// will read. Files above it are skipped, matching git grep's practical
// behavior of not producing useful hits in huge blobs.
var maxContentScanFileBytes int64 = 1 * 1024 * 1024

type fsContentSearchResponse struct {
	Root    string                 `json:"root"`
	Query   string                 `json:"query"`
	Matches []gitdiff.ContentMatch `json:"matches"`
	// Truncated reports that the match cap (or the fallback's candidate
	// budget) was hit, so more matches may exist.
	Truncated bool `json:"truncated,omitempty"`
}

// searchContent is the content (grep) counterpart of the filename search:
// case-insensitive fixed-string matching over file contents. A root inside a
// git work tree is searched with one `git grep --untracked` subprocess
// (gitignored files never match); any other root falls back to scanning the
// same git-aware candidate set the filename search walks.
func (h *fsHandler) searchContent(w http.ResponseWriter, r *http.Request) {
	root, ok := parseAbsPathParam(w, r)
	if !ok {
		return
	}
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "missing required query parameter: q", map[string]string{"field": "q"})
		return
	}
	limit := maxContentSearchResults
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxContentSearchResults {
			writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "limit must be an integer between 1 and "+strconv.Itoa(maxContentSearchResults), map[string]string{"field": "limit"})
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

	var matches []gitdiff.ContentMatch
	var truncated bool
	if gitdiff.IsInsideWorkTree(r.Context(), root) {
		matches, truncated, err = gitdiff.GrepContent(r.Context(), root, query, limit)
	} else {
		matches, truncated, err = scanContentFallback(r, root, query, limit)
	}
	if err != nil {
		writeFsOSError(w, r, root, err)
		return
	}

	writeJSON(w, r, http.StatusOK, fsContentSearchResponse{
		Root:      root,
		Query:     query,
		Matches:   matches,
		Truncated: truncated,
	})
}

// scanContentFallback greps the candidate files of a non-git root in-process.
// Candidates come from the same walk the filename search uses, so nested
// repos still respect their gitignores.
func scanContentFallback(r *http.Request, root, query string, limit int) ([]gitdiff.ContentMatch, bool, error) {
	candidates, walkTruncated, err := collectSearchCandidates(r.Context(), root, maxSearchCandidates)
	if err != nil {
		return nil, false, err
	}

	queryLower := strings.ToLower(query)
	var matches []gitdiff.ContentMatch
	truncated := walkTruncated
	for _, rel := range candidates {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxContentScanFileBytes {
			// Vanished/special/huge files are skipped, not fatal — the walk
			// races live filesystems by nature.
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if bytes.IndexByte(data, 0) != -1 {
			continue // binary
		}
		fileMatches, overflow := scanFileContent(rel, data, queryLower, limit-len(matches))
		matches = append(matches, fileMatches...)
		if overflow {
			truncated = true
			break
		}
	}
	return matches, truncated, nil
}

// scanFileContent collects up to budget matched lines; overflow reports that
// at least one further match existed beyond the budget.
func scanFileContent(rel string, data []byte, queryLower string, budget int) ([]gitdiff.ContentMatch, bool) {
	var out []gitdiff.ContentMatch
	lineNo := 0
	for len(data) > 0 {
		lineNo++
		var line []byte
		if idx := bytes.IndexByte(data, '\n'); idx != -1 {
			line = data[:idx]
			data = data[idx+1:]
		} else {
			line = data
			data = nil
		}
		if strings.Contains(strings.ToLower(string(line)), queryLower) {
			if len(out) >= budget {
				return out, true
			}
			text := strings.TrimSuffix(string(line), "\r")
			if len(text) > 500 {
				text = text[:500]
			}
			out = append(out, gitdiff.ContentMatch{Path: rel, Line: lineNo, Text: text})
		}
	}
	return out, false
}
