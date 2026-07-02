package gitdiff

import (
	"context"
	"os"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
)

const (
	sectionKindStaged   = "staged"
	sectionKindUnstaged = "unstaged"
)

// LoadWorkingTreeDiff computes the current staged + unstaged + untracked
// diff for repoPath. It never mutates the working tree or index.
func LoadWorkingTreeDiff(ctx context.Context, repoPath string) (WorkingTreeDiff, error) {
	if err := checkRepoPath(repoPath); err != nil {
		return WorkingTreeDiff{}, err
	}

	staged, err := readTrackedDiff(ctx, repoPath, sectionKindStaged, true)
	if err != nil {
		return WorkingTreeDiff{}, err
	}
	unstaged, err := readTrackedDiff(ctx, repoPath, sectionKindUnstaged, false)
	if err != nil {
		return WorkingTreeDiff{}, err
	}
	untracked, err := readUntrackedDiff(ctx, repoPath)
	if err != nil {
		return WorkingTreeDiff{}, err
	}

	files := mergeSectionFiles(staged, unstaged, untracked)
	return WorkingTreeDiff{
		RepoPath: repoPath,
		Files:    files,
		Summary:  summarizeWorkingTreeFiles(files),
	}, nil
}

func checkRepoPath(repoPath string) error {
	info, err := os.Stat(repoPath)
	if err != nil || !info.IsDir() {
		return &domain.NotFoundError{Resource: "repo_path", ID: repoPath}
	}
	return nil
}

// sectionFile is a parsedFile tagged with which working-tree section
// (staged/unstaged) it was read from, prior to merging by path.
type sectionFile struct {
	parsedFile
	Kind string
}

func readTrackedDiff(ctx context.Context, repoPath, kind string, staged bool) ([]sectionFile, error) {
	args := []string{"diff"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, "--no-ext-diff", "--no-color", "--find-renames", "--no-prefix")
	raw, err := runGit(ctx, repoPath, args...)
	if err != nil {
		return nil, err
	}
	parsed, err := parseDiffOutput(raw)
	if err != nil {
		return nil, err
	}
	files := make([]sectionFile, 0, len(parsed))
	for _, f := range parsed {
		files = append(files, sectionFile{parsedFile: f, Kind: kind})
	}
	return files, nil
}

func readUntrackedDiff(ctx context.Context, repoPath string) ([]sectionFile, error) {
	paths, err := listUntrackedFiles(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	files := make([]sectionFile, 0, len(paths))
	for _, path := range paths {
		raw, err := runGitAllowChanges(ctx, repoPath, "diff", "--no-index", "--no-ext-diff", "--no-color", "--no-prefix", "--", "/dev/null", path)
		if err != nil {
			return nil, err
		}
		parsed, err := parseDiffOutput(raw)
		if err != nil {
			return nil, err
		}
		for _, f := range parsed {
			f.Status = "untracked"
			files = append(files, sectionFile{parsedFile: f, Kind: sectionKindUnstaged})
		}
	}
	return files, nil
}

func listUntrackedFiles(ctx context.Context, repoPath string) ([]string, error) {
	output, err := runGit(ctx, repoPath, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	if output == "" {
		return []string{}, nil
	}
	parts := strings.Split(output, "\x00")
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		paths = append(paths, part)
	}
	return paths, nil
}

// mergeSectionFiles merges staged/unstaged/untracked file groups by path,
// so a file touched both staged and unstaged ends up as one File with two
// Sections. Non-"modified" statuses (new/renamed/deleted/untracked) win
// over "modified" when merging.
func mergeSectionFiles(groups ...[]sectionFile) []File {
	type sectionAccum struct {
		Kind     string
		Raw      string
		IsBinary bool
	}
	type mergedFile struct {
		Path     string
		OldPath  string
		Status   string
		Sections []sectionAccum
	}

	order := make([]string, 0)
	byPath := map[string]*mergedFile{}

	for _, group := range groups {
		for _, next := range group {
			mf, ok := byPath[next.Path]
			if !ok {
				mf = &mergedFile{Path: next.Path, OldPath: next.OldPath, Status: next.Status}
				byPath[next.Path] = mf
				order = append(order, next.Path)
			} else {
				mf.Status = mergeStatus(mf.Status, next.Status)
				if mf.OldPath == "" {
					mf.OldPath = next.OldPath
				}
			}
			mf.Sections = append(mf.Sections, sectionAccum{Kind: next.Kind, Raw: next.Raw, IsBinary: next.IsBinary})
		}
	}

	files := make([]File, 0, len(order))
	for _, path := range order {
		mf := byPath[path]
		file := File{Path: mf.Path, OldPath: mf.OldPath, Status: mf.Status}

		sections := make([]Section, 0, len(mf.Sections))
		rawParts := make([]string, 0, len(mf.Sections))
		for _, s := range mf.Sections {
			added, deleted := countAddedDeleted(s.Raw)
			text, size, truncated := capRawDiff(s.Raw)
			sections = append(sections, Section{
				Kind:           s.Kind,
				RawUnifiedDiff: text,
				IsBinary:       s.IsBinary,
				Additions:      added,
				Deletions:      deleted,
				RawDiffBytes:   size,
				Truncated:      truncated,
			})
			file.Additions += added
			file.Deletions += deleted
			if s.IsBinary {
				file.IsBinary = true
			}
			rawParts = append(rawParts, s.Raw)
		}
		file.Sections = sections

		joined := joinRawDiff(rawParts)
		text, size, truncated := capRawDiff(joined)
		file.RawUnifiedDiff = text
		file.RawDiffBytes = size
		file.Truncated = truncated

		files = append(files, file)
	}
	return files
}

func mergeStatus(current, next string) string {
	if current == "" || current == "modified" {
		return next
	}
	if next == "" || next == "modified" {
		return current
	}
	return current
}

func joinRawDiff(parts []string) string {
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString(part)
	}
	return b.String()
}

func summarizeWorkingTreeFiles(files []File) Summary {
	summary := Summary{Files: len(files)}
	for _, file := range files {
		summary.Additions += file.Additions
		summary.Deletions += file.Deletions
		hasStaged := false
		hasUnstaged := false
		for _, s := range file.Sections {
			switch s.Kind {
			case sectionKindStaged:
				hasStaged = true
			case sectionKindUnstaged:
				hasUnstaged = true
			}
		}
		if hasStaged {
			summary.StagedFiles++
		}
		if hasUnstaged {
			summary.UnstagedFiles++
		}
	}
	return summary
}
