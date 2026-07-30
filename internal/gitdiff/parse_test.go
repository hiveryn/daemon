package gitdiff

import "testing"

func TestParseDiffGitHeader_QuotedPathWithSpace(t *testing.T) {
	oldPath, newPath, err := parseDiffGitHeader(`diff --git "file with space.txt" "file with space.txt"`)
	if err != nil {
		t.Fatal(err)
	}
	if oldPath != "file with space.txt" || newPath != "file with space.txt" {
		t.Fatalf("unexpected paths: %q %q", oldPath, newPath)
	}
}

func TestParseDiffGitHeader_QuotedPathWithEscapedQuote(t *testing.T) {
	oldPath, newPath, err := parseDiffGitHeader(`diff --git "file \"quoted\".txt" "file \"quoted\".txt"`)
	if err != nil {
		t.Fatal(err)
	}
	if oldPath != `file "quoted".txt` || newPath != `file "quoted".txt` {
		t.Fatalf("unexpected paths: %q %q", oldPath, newPath)
	}
}

func TestParseDiffGitHeader_UnprefixedLine(t *testing.T) {
	_, _, err := parseDiffGitHeader("not a diff header")
	if err == nil {
		t.Fatal("expected error for missing 'diff --git ' prefix")
	}
}

func TestParseDiffGitHeader_UnquotedRepeatedPathWithSpaces(t *testing.T) {
	const path = "docs/medibank-overseas-workers-standard-hospital-and-medical (1).pdf"

	oldPath, newPath, err := parseDiffGitHeader("diff --git " + path + " " + path)
	if err != nil {
		t.Fatal(err)
	}
	if oldPath != path || newPath != path {
		t.Fatalf("unexpected paths: %q %q", oldPath, newPath)
	}
}

func TestParseDiffBlock_PureDeletionFallsBackToOldPath(t *testing.T) {
	block := "diff --git gone.txt gone.txt\n" +
		"deleted file mode 100644\n" +
		"index abc123..0000000\n" +
		"--- gone.txt\n" +
		"+++ /dev/null\n" +
		"@@ -1 +0,0 @@\n" +
		"-gone\n"

	file, err := parseDiffBlock(block)
	if err != nil {
		t.Fatal(err)
	}
	if file.Path != "gone.txt" || file.OldPath != "" || file.Status != "deleted" {
		t.Fatalf("unexpected parsed file: %#v", file)
	}
}

func TestParseDiffBlock_SelfRenameCollapsesOldPath(t *testing.T) {
	block := "diff --git same.txt same.txt\n" +
		"index abc123..def456 100644\n" +
		"--- same.txt\n" +
		"+++ same.txt\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"

	file, err := parseDiffBlock(block)
	if err != nil {
		t.Fatal(err)
	}
	if file.Path != "same.txt" || file.OldPath != "" {
		t.Fatalf("expected OldPath cleared for self-rename, got %#v", file)
	}
}

func TestParseDiffBlock_RenameWithSpacesFallsBackToRenameLines(t *testing.T) {
	block := "diff --git docs/old name.pdf docs/new name.pdf\n" +
		"similarity index 100%\n" +
		"rename from docs/old name.pdf\n" +
		"rename to docs/new name.pdf\n"

	file, err := parseDiffBlock(block)
	if err != nil {
		t.Fatal(err)
	}
	if file.Status != "renamed" || file.OldPath != "docs/old name.pdf" || file.Path != "docs/new name.pdf" {
		t.Fatalf("unexpected parsed file: %#v", file)
	}
}

func TestCountAddedDeleted_SkipsHeaderLines(t *testing.T) {
	block := "diff --git file.txt file.txt\n" +
		"index abc123..def456 100644\n" +
		"--- file.txt\n" +
		"+++ file.txt\n" +
		"@@ -1,2 +1,2 @@\n" +
		"-old line\n" +
		"+new line\n" +
		" context line\n"

	added, deleted := countAddedDeleted(block)
	if added != 1 || deleted != 1 {
		t.Fatalf("expected 1 addition and 1 deletion, got added=%d deleted=%d", added, deleted)
	}
}

func TestCountAddedDeleted_BinaryBlockYieldsZero(t *testing.T) {
	block := "diff --git image.bin image.bin\n" +
		"index abc123..def456 100644\n" +
		"Binary files image.bin and image.bin differ\n"

	added, deleted := countAddedDeleted(block)
	if added != 0 || deleted != 0 {
		t.Fatalf("expected 0/0 for binary block, got added=%d deleted=%d", added, deleted)
	}
}
