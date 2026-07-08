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
// Every presentational section is a Markdown string authored by the agent and
// emitted verbatim under its heading, so output is deterministic (fixed section
// order) and conclusions diff cleanly. Required fields return a
// *domain.ValidationError naming the offending field; the caller surfaces it to
// the agent before the approval is stored.

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

	sections = appendIfPresent(sections, renderOptionalSection("Tickets touched", p.TicketsTouched))
	sections = appendIfPresent(sections, renderOptionalSection("Decisions", p.Decisions))
	sections = appendIfPresent(sections, renderOptionalSection("Config changes", p.ConfigChanges))
	sections = appendIfPresent(sections, renderOptionalSection("User priorities", p.UserPriorities))
	sections = appendIfPresent(sections, renderOptionalSection("Open questions", p.OpenQuestions))

	nextSteps, err := renderRequiredSection("next_steps", "Next steps", p.NextSteps)
	if err != nil {
		return "", err
	}
	sections = append(sections, nextSteps)

	return strings.Join(sections, "\n\n"), nil
}

// renderTicketConclusionBody renders the ticket conclusion body in canonical
// order: Summary, Implementation, Deviations, Verification, Follow-ups, Open
// questions. Implementation is required unless the ticket was rejected;
// Follow-ups (Markdown prose naming candidate follow-up tickets) and Open
// questions are optional.
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

	sections = appendIfPresent(sections, renderOptionalSection("Deviations", p.Deviations))
	if verification := strings.TrimSpace(p.Verification); verification != "" {
		sections = append(sections, section("Verification", verification))
	}
	sections = appendIfPresent(sections, renderOptionalSection("Follow-ups", p.FollowUps))
	sections = appendIfPresent(sections, renderOptionalSection("Open questions", p.OpenQuestions))

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

	recommendations, err := renderRequiredSection("recommendations", "Recommendations", p.Recommendations)
	if err != nil {
		return "", err
	}
	sections = append(sections, recommendations)

	openQuestions, err := renderRequiredSection("open_questions", "Open questions", p.OpenQuestions)
	if err != nil {
		return "", err
	}
	sections = append(sections, openQuestions)

	return strings.Join(sections, "\n\n"), nil
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

// renderOptionalSection emits the agent-authored Markdown verbatim under its
// heading, or "" (the section is dropped) when the Markdown is blank.
func renderOptionalSection(heading, markdown string) string {
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return ""
	}
	return section(heading, trimmed)
}

// renderRequiredSection is renderOptionalSection for sections that must always
// appear. The agent records "nothing to report" by writing "None" as the
// Markdown body; a blank string is a validation error naming the field.
func renderRequiredSection(field, heading, markdown string) (string, error) {
	content, err := requiredString(field, markdown)
	if err != nil {
		return "", err
	}
	return section(heading, content), nil
}
