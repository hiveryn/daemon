package sessionruntime

import (
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
)

// This file renders the structured conclusion input from the three conclude
// MCP tools into the canonical conclusion.md markdown body. The daemon is the
// single render point: the structured fields are an input contract, never
// persisted as structured data. Frontmatter metadata is built separately by the
// write path (see newArchitectConclusionDocument / newConclusionDocument /
// newSessionConclusionDocument); these functions produce only the body.
//
// Output is deterministic (fixed section order, stable bullet formatting) so
// conclusions diff cleanly. Required fields return a *domain.ValidationError
// naming the offending field; the caller surfaces it to the agent before the
// approval is stored.

// renderConclusionBody dispatches to the per-type renderer for the session's
// structured conclusion input, returning the canonical markdown body.
func (s *Service) renderConclusionBody(sessionType domain.SessionType, params domain.ConcludeSessionParams) (string, error) {
	switch sessionType {
	case domain.SessionTypeArchitect:
		return renderArchitectConclusionBody(params)
	case domain.SessionTypeTicket:
		return renderTicketConclusionBody(params)
	case domain.SessionTypeFreeform:
		return renderFreeformConclusionBody(params)
	default:
		return "", &domain.ValidationError{Field: "session_type", Message: "unknown session type: " + string(sessionType)}
	}
}

// renderArchitectConclusionBody renders the architect conclusion body in
// canonical order: Summary, Narrative, Tickets touched, Decisions, Config
// changes, User priorities, Open questions, Next steps. Everything except
// Summary, Narrative, and Next steps is optional and its section is omitted
// when empty.
func renderArchitectConclusionBody(p domain.ConcludeSessionParams) (string, error) {
	summary, err := requiredString("summary", p.Summary)
	if err != nil {
		return "", err
	}
	narrative, err := requiredString("narrative", p.Narrative)
	if err != nil {
		return "", err
	}

	sections := []string{
		section("Summary", summary),
		section("Narrative", narrative),
	}

	touched, err := renderTicketsTouched(p.TicketsTouched)
	if err != nil {
		return "", err
	}
	sections = appendIfPresent(sections, touched)
	sections = appendIfPresent(sections, renderOptionalList("Decisions", p.Decisions))
	sections = appendIfPresent(sections, renderOptionalList("Config changes", p.ConfigChanges))
	sections = appendIfPresent(sections, renderOptionalList("User priorities", p.UserPriorities))
	sections = appendIfPresent(sections, renderOptionalList("Open questions", p.OpenQuestions))

	nextSteps, err := renderRequiredList("next_steps", "Next steps", p.NextSteps)
	if err != nil {
		return "", err
	}
	sections = append(sections, nextSteps)

	return strings.Join(sections, "\n\n"), nil
}

// renderTicketConclusionBody renders the ticket conclusion body in canonical
// order: Summary, Implementation, Deviations, Verification, Follow-ups, Open
// questions. Implementation is required unless the ticket was rejected;
// Follow-ups (a list of follow-up ticket IDs) and Open questions are optional.
// Follow-up ticket-ID existence is validated separately in the write path,
// where the ticket store is available.
func renderTicketConclusionBody(p domain.ConcludeSessionParams) (string, error) {
	summary, err := requiredString("summary", p.Summary)
	if err != nil {
		return "", err
	}

	sections := []string{section("Summary", summary)}

	if p.Rejected {
		if impl := strings.TrimSpace(p.Implementation); impl != "" {
			sections = append(sections, section("Implementation", impl))
		}
	} else {
		impl, err := requiredString("implementation", p.Implementation)
		if err != nil {
			return "", err
		}
		sections = append(sections, section("Implementation", impl))
	}

	sections = appendIfPresent(sections, renderOptionalList("Deviations", p.Deviations))
	if verification := strings.TrimSpace(p.Verification); verification != "" {
		sections = append(sections, section("Verification", verification))
	}
	sections = appendIfPresent(sections, renderOptionalList("Follow-ups", p.FollowUps))
	sections = appendIfPresent(sections, renderOptionalList("Open questions", p.OpenQuestions))

	return strings.Join(sections, "\n\n"), nil
}

// renderFreeformConclusionBody renders the freeform conclusion body in
// canonical order: Summary, Findings, Recommendations, Open questions.
func renderFreeformConclusionBody(p domain.ConcludeSessionParams) (string, error) {
	summary, err := requiredString("summary", p.Summary)
	if err != nil {
		return "", err
	}
	findings, err := requiredString("findings", p.Findings)
	if err != nil {
		return "", err
	}

	sections := []string{
		section("Summary", summary),
		section("Findings", findings),
	}

	recommendations, err := renderRequiredList("recommendations", "Recommendations", p.Recommendations)
	if err != nil {
		return "", err
	}
	sections = append(sections, recommendations)

	openQuestions, err := renderRequiredList("open_questions", "Open questions", p.OpenQuestions)
	if err != nil {
		return "", err
	}
	sections = append(sections, openQuestions)

	return strings.Join(sections, "\n\n"), nil
}

var validTicketTouchActions = map[string]bool{
	"created": true,
	"updated": true,
	"deleted": true,
}

func section(heading, content string) string {
	return "## " + heading + "\n" + content
}

func appendIfPresent(sections []string, s string) []string {
	if s == "" {
		return sections
	}
	return append(sections, s)
}

func requiredString(field, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", &domain.ValidationError{Field: field, Message: "is required"}
	}
	return trimmed, nil
}

func normalizeList(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func renderOptionalList(heading string, items []string) string {
	norm := normalizeList(items)
	if len(norm) == 0 {
		return ""
	}
	return section(heading, bulletList(norm))
}

func renderRequiredList(field, heading string, items []string) (string, error) {
	norm := normalizeList(items)
	if len(norm) == 0 {
		return "", &domain.ValidationError{Field: field, Message: `is required; pass ["none"] to record explicitly`}
	}
	if len(norm) == 1 && strings.EqualFold(norm[0], "none") {
		return section(heading, "None"), nil
	}
	return section(heading, bulletList(norm)), nil
}

func bulletList(items []string) string {
	var b strings.Builder
	for i, item := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- ")
		b.WriteString(item)
	}
	return b.String()
}

func renderTicketsTouched(items []domain.TicketTouch) (string, error) {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		action := strings.TrimSpace(item.Action)
		note := strings.TrimSpace(item.Note)
		if id == "" && action == "" && note == "" {
			continue
		}
		if id == "" {
			return "", &domain.ValidationError{Field: "tickets_touched", Message: "each entry requires an id"}
		}
		if !validTicketTouchActions[action] {
			return "", &domain.ValidationError{Field: "tickets_touched", Message: "action must be one of: created, updated, deleted"}
		}
		line := "- " + id + " — " + action
		if note != "" {
			line += ": " + note
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return section("Tickets touched", strings.Join(lines, "\n")), nil
}
