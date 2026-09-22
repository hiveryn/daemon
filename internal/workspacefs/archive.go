package workspacefs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// archiveNamePattern matches ROADMAP-YYYY-MM-DD.md and its same-day collision
// forms ROADMAP-YYYY-MM-DD-NN.md.
var archiveNamePattern = regexp.MustCompile(`^ROADMAP-(\d{4}-\d{2}-\d{2})(?:-(\d{2}))?\.md$`)

// ArchiveNameFormat is the canonical archive filename, reported in errors and
// in the artifact schema.
const ArchiveNameFormat = "ROADMAP-YYYY-MM-DD.md (same-day collisions: ROADMAP-YYYY-MM-DD-02.md, then -03, and so on)"

// parsedArchiveName is a validated archive filename.
type parsedArchiveName struct {
	Date time.Time
	// Sequence is 1 for the unsuffixed name and N for a -NN suffix.
	Sequence int
}

// parseArchiveName validates an archive filename and extracts its date. The
// suffix starts at 02, because the first archive of a day carries no suffix —
// a -01 file would be a second spelling of the same slot, so it is rejected.
func parseArchiveName(name string) (parsedArchiveName, error) {
	match := archiveNamePattern.FindStringSubmatch(name)
	if match == nil {
		return parsedArchiveName{}, fmt.Errorf("%q does not match %s", name, ArchiveNameFormat)
	}

	date, err := time.Parse("2006-01-02", match[1])
	if err != nil {
		return parsedArchiveName{}, fmt.Errorf("%q has an invalid calendar date %q", name, match[1])
	}

	sequence := 1
	if match[2] != "" {
		sequence, err = strconv.Atoi(match[2])
		if err != nil {
			return parsedArchiveName{}, fmt.Errorf("%q has an unreadable sequence suffix %q", name, match[2])
		}
		if sequence < 2 {
			return parsedArchiveName{}, fmt.Errorf("%q uses sequence suffix %q; the first archive of a day carries no suffix and collisions start at -02", name, match[2])
		}
	}

	return parsedArchiveName{Date: date.UTC(), Sequence: sequence}, nil
}

// NextArchiveName returns the filename a roadmap archived on day should take,
// given what already exists in dir. It never returns a name that is already
// present, so archiving cannot overwrite history.
//
// Nothing in this package writes the file; this exists so the naming rule has
// exactly one implementation, shared by the validator and any later caller
// that performs the archive.
func NextArchiveName(dir string, day time.Time) (string, error) {
	stamp := day.UTC().Format("2006-01-02")
	candidates := []string{fmt.Sprintf("ROADMAP-%s.md", stamp)}
	for sequence := 2; sequence <= 99; sequence++ {
		candidates = append(candidates, fmt.Sprintf("ROADMAP-%s-%02d.md", stamp, sequence))
	}

	for _, candidate := range candidates {
		_, err := os.Lstat(filepath.Join(dir, candidate))
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect archive candidate %q: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("every archive name for %s through -99 is taken in %s", stamp, dir)
}

// validateArchiveFile validates one archived roadmap: its filename, its
// structure, and that its archivedAt date agrees with its filename date.
func validateArchiveFile(dir, name, relDir string) domain.WorkspaceEntry {
	def := definitions[domain.ArtifactRoadmapArchive]
	rel := relDir + "/" + name
	diags := newDiagnostics(rel)

	entry := domain.WorkspaceEntry{
		Kind: domain.ArtifactRoadmapArchive,
		Path: rel,
	}

	parsedName, nameErr := parseArchiveName(name)
	if nameErr != nil {
		diags.errorf(domain.DiagArchiveNameInvalid, 0, "%v", nameErr)
	}

	result := validateMarkdownArtifact(def, filepath.Join(dir, name), true, diags)
	entry.ModifiedAt = result.ModifiedAt
	entry.DocumentUpdatedAt = result.DocumentUpdatedAt

	// The date cross-check needs both halves; when either failed its own
	// diagnostic already says so and a mismatch would be noise.
	if nameErr == nil && result.DocumentUpdatedAt != nil {
		archivedDay := result.DocumentUpdatedAt.UTC().Format("2006-01-02")
		if archivedDay != parsedName.Date.Format("2006-01-02") {
			diags.errorf(domain.DiagArchiveDateMismatch, 0,
				"archivedAt is %s but the filename says %s; rename the file or correct archivedAt so they agree",
				archivedDay, parsedName.Date.Format("2006-01-02"))
		}
	}

	entry.Diagnostics = diags.items
	entry.Valid = !diags.hasErrors()
	return entry
}
