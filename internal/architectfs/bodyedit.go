package architectfs

import "strings"

type bodyEditMatch struct {
	start int
	end   int
}

func findBodyEditMatches(content, oldString string) []bodyEditMatch {
	for _, matcher := range []func(string, string) []bodyEditMatch{
		findExactBodyEditMatches,
		findNormalizedBodyEditMatches,
		findAnchoredBodyEditMatches,
	} {
		if matches := matcher(content, oldString); len(matches) > 0 {
			return matches
		}
	}
	return nil
}

func findExactBodyEditMatches(content, oldString string) []bodyEditMatch {
	var matches []bodyEditMatch
	for offset := 0; ; {
		idx := strings.Index(content[offset:], oldString)
		if idx == -1 {
			return matches
		}
		start := offset + idx
		matches = append(matches, bodyEditMatch{start: start, end: start + len(oldString)})
		offset = start + len(oldString)
	}
}

func findNormalizedBodyEditMatches(content, oldString string) []bodyEditMatch {
	contentLines, contentStarts := splitLinesWithOffsets(content)
	searchLines := normalizeSearchLines(oldString)
	if len(searchLines) == 0 || len(searchLines) > len(contentLines) {
		return nil
	}

	var matches []bodyEditMatch
	for i := 0; i <= len(contentLines)-len(searchLines); i++ {
		matched := true
		for j := range searchLines {
			if normalizeBodyEditLine(contentLines[i+j]) != normalizeBodyEditLine(searchLines[j]) {
				matched = false
				break
			}
		}
		if matched {
			start := contentStarts[i]
			lastLine := i + len(searchLines) - 1
			end := contentStarts[lastLine] + len(contentLines[lastLine])
			matches = append(matches, bodyEditMatch{start: start, end: end})
		}
	}

	return matches
}

func findAnchoredBodyEditMatches(content, oldString string) []bodyEditMatch {
	contentLines, contentStarts := splitLinesWithOffsets(content)
	searchLines := normalizeSearchLines(oldString)
	if len(searchLines) < 3 {
		return nil
	}

	firstIdx, lastIdx, ok := firstAndLastNonEmptyLine(searchLines)
	if !ok || firstIdx == lastIdx {
		return nil
	}

	searchSequence := collectNormalizedNonEmptyLines(searchLines[firstIdx : lastIdx+1])
	if len(searchSequence) < 2 {
		return nil
	}

	firstAnchor := normalizeBodyEditLine(searchLines[firstIdx])
	lastAnchor := normalizeBodyEditLine(searchLines[lastIdx])

	var matches []bodyEditMatch
	for startLine := 0; startLine < len(contentLines); startLine++ {
		if normalizeBodyEditLine(contentLines[startLine]) != firstAnchor {
			continue
		}
		for endLine := startLine + 1; endLine < len(contentLines); endLine++ {
			if normalizeBodyEditLine(contentLines[endLine]) != lastAnchor {
				continue
			}
			candidateSequence := collectNormalizedNonEmptyLines(contentLines[startLine : endLine+1])
			if !equalStrings(candidateSequence, searchSequence) {
				continue
			}

			start := contentStarts[startLine]
			end := contentStarts[endLine] + len(contentLines[endLine])
			matches = append(matches, bodyEditMatch{start: start, end: end})
			break
		}
	}

	return matches
}

func applyBodyEditMatches(content string, matches []bodyEditMatch, newString string, replaceAll bool) string {
	if !replaceAll {
		match := matches[0]
		return content[:match.start] + newString + content[match.end:]
	}

	var b strings.Builder
	last := 0
	for _, match := range matches {
		b.WriteString(content[last:match.start])
		b.WriteString(newString)
		last = match.end
	}
	b.WriteString(content[last:])
	return b.String()
}

func splitLinesWithOffsets(content string) ([]string, []int) {
	lines := strings.Split(content, "\n")
	starts := make([]int, len(lines))
	offset := 0
	for i, line := range lines {
		starts[i] = offset
		offset += len(line)
		if i < len(lines)-1 {
			offset++
		}
	}
	return lines, starts
}

func normalizeSearchLines(search string) []string {
	lines := strings.Split(search, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func firstAndLastNonEmptyLine(lines []string) (int, int, bool) {
	first := -1
	last := -1
	for i, line := range lines {
		if normalizeBodyEditLine(line) == "" {
			continue
		}
		if first == -1 {
			first = i
		}
		last = i
	}
	if first == -1 || last == -1 {
		return 0, 0, false
	}
	return first, last, true
}

func collectNormalizedNonEmptyLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		normalized := normalizeBodyEditLine(line)
		if normalized == "" {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

func normalizeBodyEditLine(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
