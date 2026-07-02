package gitdiff

// MaxRawDiffBytes caps the raw unified diff text embedded per file/section
// in API responses. Files whose raw diff exceeds this are reported with
// Truncated=true and an empty RawUnifiedDiff; RawDiffBytes still reports
// the true size so the client can defer or skip rendering. It's a var
// (not const) so tests can lower it.
var MaxRawDiffBytes = 2 * 1024 * 1024 // 2 MiB

type Summary struct {
	Files         int `json:"files"`
	StagedFiles   int `json:"staged_files,omitempty"`
	UnstagedFiles int `json:"unstaged_files,omitempty"`
	Additions     int `json:"additions"`
	Deletions     int `json:"deletions"`
}

// Section is a working-tree-only breakdown of a File's diff by staged vs.
// unstaged state. Single-commit diffs never populate File.Sections.
type Section struct {
	Kind           string `json:"kind"` // "staged" | "unstaged"
	RawUnifiedDiff string `json:"raw_unified_diff,omitempty"`
	IsBinary       bool   `json:"is_binary"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	RawDiffBytes   int    `json:"raw_diff_bytes"`
	Truncated      bool   `json:"truncated,omitempty"`
}

// File is shared by both the working-tree and single-commit diff results.
// Path/OldPath/Status/IsBinary/Additions/Deletions/RawDiffBytes/Truncated/
// RawUnifiedDiff are always populated (aggregated across Sections for the
// working-tree diff; direct for a single-commit diff, which never sets
// Sections).
type File struct {
	Path           string    `json:"path"`
	OldPath        string    `json:"old_path,omitempty"`
	Status         string    `json:"status"` // modified|new|deleted|renamed|copied|untracked
	IsBinary       bool      `json:"is_binary"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	RawDiffBytes   int       `json:"raw_diff_bytes"`
	Truncated      bool      `json:"truncated,omitempty"`
	RawUnifiedDiff string    `json:"raw_unified_diff,omitempty"`
	Sections       []Section `json:"sections,omitempty"`
}

type WorkingTreeDiff struct {
	RepoPath string  `json:"repo_path"`
	Files    []File  `json:"files"`
	Summary  Summary `json:"summary"`
}

type CommitDiff struct {
	SHA       string  `json:"sha"`
	ParentSHA string  `json:"parent_sha,omitempty"` // empty for root commits
	IsMerge   bool    `json:"is_merge"`
	RepoPath  string  `json:"repo_path"`
	Files     []File  `json:"files"`
	Summary   Summary `json:"summary"`
}

// capRawDiff enforces MaxRawDiffBytes on raw unified diff text, reporting
// the true size regardless of whether it was truncated away.
func capRawDiff(raw string) (text string, size int, truncated bool) {
	size = len(raw)
	if size > MaxRawDiffBytes {
		return "", size, true
	}
	return raw, size, false
}
