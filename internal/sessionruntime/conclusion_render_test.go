package sessionruntime

import (
	"errors"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestRenderArchitectConclusionBodyFullOrder(t *testing.T) {
	t.Parallel()

	body, err := renderArchitectConclusionBody(domain.ConcludeSessionParams{
		Summary:        "Shipped the config tools.",
		Narrative:      "Consolidated the MCP surface.",
		TicketsTouched: "- T-1 — created: config tool\n- T-2 — deleted",
		Decisions:      "- Split conclude into three tools",
		ConfigChanges:  "- Added repo key foo",
		UserPriorities: "- Ship the schema work",
		OpenQuestions:  "- Should moveTicketToDone also be structured?",
		NextSteps:      "- Wire the desktop\n- Update docs",
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	want := strings.Join([]string{
		"## Summary\nShipped the config tools.",
		"## Narrative\nConsolidated the MCP surface.",
		"## Tickets touched\n- T-1 — created: config tool\n- T-2 — deleted",
		"## Decisions\n- Split conclude into three tools",
		"## Config changes\n- Added repo key foo",
		"## User priorities\n- Ship the schema work",
		"## Open questions\n- Should moveTicketToDone also be structured?",
		"## Next steps\n- Wire the desktop\n- Update docs",
	}, "\n\n")
	if body != want {
		t.Fatalf("body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestRenderArchitectConclusionBodyOptionalOmittedAndNoneConvention(t *testing.T) {
	t.Parallel()

	body, err := renderArchitectConclusionBody(domain.ConcludeSessionParams{
		Summary:   "Quick session.",
		Narrative: "Nothing much.",
		NextSteps: "None",
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	// Every optional section (including the now-optional open_questions) is
	// omitted when its Markdown is blank.
	for _, heading := range []string{"## Tickets touched", "## Decisions", "## Config changes", "## User priorities", "## Open questions"} {
		if strings.Contains(body, heading) {
			t.Fatalf("expected empty optional section %q to be omitted, got:\n%s", heading, body)
		}
	}
	if !strings.Contains(body, "## Next steps\nNone") {
		t.Fatalf("expected verbatim None for next steps, got:\n%s", body)
	}
}

func TestRenderArchitectConclusionBodyRequiredFields(t *testing.T) {
	t.Parallel()

	cases := map[string]domain.ConcludeSessionParams{
		"summary":    {Narrative: "x", NextSteps: "None"},
		"narrative":  {Summary: "x", NextSteps: "None"},
		"next_steps": {Summary: "x", Narrative: "x"},
	}
	for field, params := range cases {
		field, params := field, params
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			_, err := renderArchitectConclusionBody(params)
			assertValidationField(t, err, field)
		})
	}
}

func TestRenderArchitectConclusionBodyRejectsBlankRequiredSection(t *testing.T) {
	t.Parallel()

	// next_steps is a required section: a whitespace-only string trims to empty
	// and must be rejected.
	_, err := renderArchitectConclusionBody(domain.ConcludeSessionParams{
		Summary:   "x",
		Narrative: "x",
		NextSteps: "   ",
	})
	assertValidationField(t, err, "next_steps")
}

func TestRenderTicketConclusionBody(t *testing.T) {
	t.Parallel()

	body, err := renderTicketConclusionBody(domain.ConcludeSessionParams{
		Summary:        "Added the endpoint.",
		Implementation: "Wired the handler.",
		Deviations:     "- Skipped the cache",
		Verification:   "go test ./... passed",
		FollowUps:      "- T-9\n- T-10",
		OpenQuestions:  "- Should we cache?",
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	want := strings.Join([]string{
		"## Summary\nAdded the endpoint.",
		"## Implementation\nWired the handler.",
		"## Deviations\n- Skipped the cache",
		"## Verification\ngo test ./... passed",
		"## Follow-ups\n- T-9\n- T-10",
		"## Open questions\n- Should we cache?",
	}, "\n\n")
	if body != want {
		t.Fatalf("body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestRenderTicketConclusionBodyOmitsOptionalFollowUpsAndOpenQuestions(t *testing.T) {
	t.Parallel()

	body, err := renderTicketConclusionBody(domain.ConcludeSessionParams{
		Summary:        "Added the endpoint.",
		Implementation: "Wired the handler.",
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	if strings.Contains(body, "## Follow-ups") || strings.Contains(body, "## Open questions") {
		t.Fatalf("expected empty follow-ups and open questions to be omitted, got:\n%s", body)
	}
}

func TestRenderTicketConclusionBodyImplementationRequiredUnlessRejected(t *testing.T) {
	t.Parallel()

	// Not rejected: implementation is required.
	_, err := renderTicketConclusionBody(domain.ConcludeSessionParams{
		Summary: "x",
	})
	assertValidationField(t, err, "implementation")

	// Rejected: implementation may be omitted and no Implementation section renders.
	body, err := renderTicketConclusionBody(domain.ConcludeSessionParams{
		Summary:  "No work produced.",
		Rejected: true,
	})
	if err != nil {
		t.Fatalf("rejected render failed: %v", err)
	}
	if strings.Contains(body, "## Implementation") {
		t.Fatalf("expected no Implementation section when rejected, got:\n%s", body)
	}
}

func TestRenderFreeformConclusionBody(t *testing.T) {
	t.Parallel()

	body, err := renderFreeformConclusionBody(domain.ConcludeSessionParams{
		Summary:         "Investigated the flake.",
		Findings:        "It's a race in the scheduler.",
		Recommendations: "- Add a mutex",
		OpenQuestions:   "None",
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	want := strings.Join([]string{
		"## Summary\nInvestigated the flake.",
		"## Findings\nIt's a race in the scheduler.",
		"## Recommendations\n- Add a mutex",
		"## Open questions\nNone",
	}, "\n\n")
	if body != want {
		t.Fatalf("body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestRenderFreeformConclusionBodyRequiredFields(t *testing.T) {
	t.Parallel()

	cases := map[string]domain.ConcludeSessionParams{
		"summary":         {Findings: "x", Recommendations: "None", OpenQuestions: "None"},
		"findings":        {Summary: "x", Recommendations: "None", OpenQuestions: "None"},
		"recommendations": {Summary: "x", Findings: "x", OpenQuestions: "None"},
		"open_questions":  {Summary: "x", Findings: "x", Recommendations: "None"},
	}
	for field, params := range cases {
		field, params := field, params
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			_, err := renderFreeformConclusionBody(params)
			assertValidationField(t, err, field)
		})
	}
}

func assertValidationField(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected validation error for field %q, got nil", field)
	}
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *domain.ValidationError, got %T: %v", err, err)
	}
	if verr.Field != field {
		t.Fatalf("field = %q, want %q", verr.Field, field)
	}
}
