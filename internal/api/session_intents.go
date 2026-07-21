package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

// Intent endpoints come in two shapes:
//
//   - Agent-facing, per tool (conclude-session, create-work-ticket). These
//     BLOCK: the request is held open until the user answers or the tool's
//     policy fires. Each has a typed body and tool-specific pre-validation, so
//     they are separate routes rather than one generic /intents/{type}.
//   - Desktop-facing, generic (approve, deny), addressed by intent id — the
//     desktop must not learn a new route per tool.
//
// The desktop's own direct routes (POST /api/sessions/{id}/conclude and
// POST /api/architects/{key}/tickets) stay unapproved; approval gates the
// AGENT, not the user.

// intentResolutionBody is the envelope every agent-facing intent route returns.
// The outcome is what lets an agent tell "the user said no" (do not retry) from
// "it broke" (maybe retry) without parsing prose.
type intentResolutionBody struct {
	IntentID string `json:"intent_id"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	Result   any    `json:"result,omitempty"`
}

func writeIntentResolution[R any](w http.ResponseWriter, r *http.Request, res domain.IntentResolution[R], result any) {
	body := intentResolutionBody{
		IntentID: res.IntentID,
		Outcome:  string(res.Outcome),
		Reason:   res.Reason,
	}
	if res.Outcome.Approved() {
		body.Result = result
	}
	writeJSON(w, r, http.StatusOK, body)
}

// concludeSessionIntent is the agent's conclude path. It blocks.
func (h *sessionsHandler) concludeSessionIntent(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	// Structured conclusion input. The daemon renders these into the canonical
	// conclusion.md body (see sessionruntime.render*ConclusionBody); Body is not
	// accepted from the wire. The [fm] metadata fields (commits/outcome/
	// rejection_reason) still flow into frontmatter as before.
	var input struct {
		Commits         []domain.CommitRef `json:"commits,omitempty"`
		Outcome         string             `json:"outcome,omitempty"`
		RejectionReason string             `json:"rejection_reason,omitempty"`
		Summary         string             `json:"summary,omitempty"`
		Narrative       string             `json:"narrative,omitempty"`
		Implementation  string             `json:"implementation,omitempty"`
		Findings        string             `json:"findings,omitempty"`
		Verification    string             `json:"verification,omitempty"`
		TicketsTouched  string             `json:"tickets_touched,omitempty"`
		Decisions       string             `json:"decisions,omitempty"`
		ConfigChanges   string             `json:"config_changes,omitempty"`
		UserPriorities  string             `json:"user_priorities,omitempty"`
		Deviations      string             `json:"deviations,omitempty"`
		FollowUps       string             `json:"follow_ups,omitempty"`
		Recommendations string             `json:"recommendations,omitempty"`
		OpenQuestions   string             `json:"open_questions,omitempty"`
		NextSteps       string             `json:"next_steps,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	res, err := h.sessions.RequestConclusion(r.Context(), r.PathValue("id"), domain.ConcludeSessionParams{
		Commits:         input.Commits,
		Outcome:         domain.TicketOutcome(input.Outcome),
		RejectionReason: input.RejectionReason,
		Summary:         input.Summary,
		Narrative:       input.Narrative,
		Implementation:  input.Implementation,
		Findings:        input.Findings,
		Verification:    input.Verification,
		TicketsTouched:  input.TicketsTouched,
		Decisions:       input.Decisions,
		ConfigChanges:   input.ConfigChanges,
		UserPriorities:  input.UserPriorities,
		Deviations:      input.Deviations,
		FollowUps:       input.FollowUps,
		Recommendations: input.Recommendations,
		OpenQuestions:   input.OpenQuestions,
		NextSteps:       input.NextSteps,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeIntentResolution(w, r, res, map[string]any{
		"session_id": res.Result.SessionID,
		"ticket_id":  res.Result.TicketID,
	})
}

// createWorkTicketIntent is the agent's ticket-create path. It blocks, and the
// ticket is written only if the intent resolves approved. Session-scoped rather
// than architect-scoped on purpose: the architect key is read from the stored
// session, so an agent cannot create tickets in an architect it wasn't spawned
// for.
func (h *sessionsHandler) createWorkTicketIntent(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	var input struct {
		Title           string   `json:"title"`
		Repo            string   `json:"repo"`
		AdditionalRepos []string `json:"additional_repos"`
		Body            string   `json:"body,omitempty"`
		References      []string `json:"references,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	res, err := h.sessions.RequestCreateWorkTicket(r.Context(), r.PathValue("id"), domain.CreateTicketParams{
		Title:           input.Title,
		Repo:            input.Repo,
		AdditionalRepos: input.AdditionalRepos,
		Body:            input.Body,
		References:      input.References,
		// Now is deliberately not set here: it is stamped at write time inside
		// the intent's Exec, so it never enters the dedup hash.
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeIntentResolution(w, r, res, res.Result)
}

// approveIntent resolves an intent as approved and runs its side effect.
func (h *sessionsHandler) approveIntent(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	intent, err := h.sessions.ApproveIntent(r.Context(), r.PathValue("id"), r.PathValue("intentID"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, intent)
}

// denyIntent resolves an intent as denied. The side effect never runs.
func (h *sessionsHandler) denyIntent(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	var input struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	if err := h.sessions.DenyIntent(r.Context(), r.PathValue("id"), r.PathValue("intentID"), input.Reason); err != nil {
		writeDomainError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
