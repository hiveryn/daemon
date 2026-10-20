package sessionruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/hiveryn/daemon/internal/domain"
)

// Action-conclusion notices: the second agent message producer. An Action
// requested by an agent (executeAction) announces its approved conclusion to
// that agent — the project's architect, whichever of its sessions runs (a
// pending notice outlives an architect-session replacement), or the exact
// worker session that asked (gone session: failed, never redirected). A manual
// launch has no agent caller and nobody to tell. The message is begun before
// the conclusion is published and committed right after, so the caller is
// told only once getActionResult serves the final status and its package
// record; a crash in between leaves a pending_commit row that reconciliation
// settles against the execution record. Denied conclusions, artifact
// preparation and daemon-side failures (cancel, interruption, launch failure)
// never reach the producer.

// actionConcludedDedupeKey is the identity of an execution's finalized result.
// An execution finalizes once (PublishActionConclusion is compare-and-set on
// running), so every attempt to apply the same outcome converges on one row;
// a row left behind by an attempt with the other outcome is failed as
// superseded by reconciliation.
func actionConcludedDedupeKey(executionID string, status domain.ActionRunStatus) string {
	return fmt.Sprintf("%s:%s:%s", domain.AgentMessageActionConcluded, executionID, status)
}

// actionConcludedBody tells the caller what ended and how to read it. It
// never claims a package is on the caller's machine: the copy is made on the
// caller's first read, and only artifacts.state says when it is usable.
func actionConcludedBody(run domain.ActionRun, status domain.ActionRunStatus, pkg *domain.ActionArtifactRecord) string {
	body := fmt.Sprintf("Action %s execution %s has concluded. Outcome: %s. Use getActionResult with this execution id to read its summary", run.Action, run.ID, status)
	if pkg == nil {
		return body + ". No artifact package was published."
	}
	files := "files"
	if pkg.Files == 1 {
		files = "file"
	}
	return body + fmt.Sprintf("; its artifact package (%d %s) is copied to your machine on that first read, and output_dir is set once artifacts.state is ready.", pkg.Files, files)
}

// actionConcludedRecipient is who asked for the execution, or "" for a manual
// launch.
func actionConcludedRecipient(run domain.ActionRun) string {
	switch {
	case run.ArchitectKey == "":
		return ""
	case run.Trigger == domain.ActionRunTriggerWorker && run.RequesterSessionID != "":
		return domain.AgentMessageRecipientSession + run.RequesterSessionID
	default:
		return domain.AgentMessageRecipientArchitect + run.ArchitectKey
	}
}

// beginActionConcludedMessage records the pending_commit notice for a
// conclusion about to be published, or nothing for a manual launch. A retry
// that finds the row still pending_commit refreshes its text, so a package
// prepared or dropped between attempts is announced as it is now.
func (s *Service) beginActionConcludedMessage(ctx context.Context, run domain.ActionRun, status domain.ActionRunStatus, pkg *domain.ActionArtifactRecord) (domain.AgentMessage, error) {
	recipient := actionConcludedRecipient(run)
	if recipient == "" {
		return domain.AgentMessage{}, nil
	}
	params := AgentMessageParams{
		Kind:         domain.AgentMessageActionConcluded,
		Recipient:    recipient,
		ArchitectKey: run.ArchitectKey,
		DedupeKey:    actionConcludedDedupeKey(run.ID, status),
		Body:         actionConcludedBody(run, status, pkg),
		Payload: map[string]any{
			"execution_id":         run.ID,
			"action":               run.Action,
			"outcome":              string(status),
			"package":              pkg != nil,
			"requester_session_id": run.RequesterSessionID,
		},
	}
	m, err := s.BeginAgentMessage(ctx, params)
	if err != nil {
		return domain.AgentMessage{}, err
	}
	if m.State == domain.AgentMessagePendingCommit && m.Body != params.Body {
		m.Body = params.Body
		m.Payload = cloneAnyMap(params.Payload)
		m.UpdatedAt = s.messages.now()
		if err := s.messages.repo.UpdateAgentMessage(ctx, m); err != nil {
			return domain.AgentMessage{}, fmt.Errorf("refresh action conclusion notice %s: %w", m.ID, err)
		}
	}
	return m, nil
}

// checkActionConcludedCommit settles a pending_commit notice against the
// execution record: final with this outcome and the agent's summary →
// committed; final any other way (cancelled, interrupted, the other outcome)
// → superseded; gone → failed; still running → kept with its condition.
func (s *Service) checkActionConcludedCommit(ctx context.Context, m domain.AgentMessage) messageCommitVerdict {
	executionID, _ := m.Payload["execution_id"].(string)
	outcome, _ := m.Payload["outcome"].(string)
	status := domain.ActionRunStatus(outcome)
	if executionID == "" || (status != domain.ActionRunCompleted && status != domain.ActionRunFailed) {
		return messageCommitVerdict{failed: "malformed action_concluded payload: execution id or outcome missing"}
	}
	rt, err := s.actionRuntime()
	if err != nil {
		return messageCommitVerdict{condition: "execution " + executionID + " cannot be read: " + err.Error()}
	}
	run, err := rt.runs.GetActionRun(ctx, executionID)
	if err != nil {
		if errors.As(err, new(*domain.NotFoundError)) {
			return messageCommitVerdict{failed: "execution " + executionID + " no longer exists"}
		}
		return messageCommitVerdict{condition: "execution " + executionID + " cannot be read: " + err.Error()}
	}
	switch {
	case run.Status == status && run.Summary != "":
		return messageCommitVerdict{committed: true}
	case run.Status.Terminal():
		reason := fmt.Sprintf("superseded: the execution ended as %s without this conclusion", run.Status)
		if run.Error != "" {
			reason += " (" + run.Error + ")"
		}
		return messageCommitVerdict{failed: reason}
	default:
		return messageCommitVerdict{condition: fmt.Sprintf("conclusion not persisted; the execution is %s", run.Status)}
	}
}
