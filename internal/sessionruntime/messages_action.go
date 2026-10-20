package sessionruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/hiveryn/daemon/internal/domain"
)

// Action-ended notices: the second agent message producer. An execution an
// agent requested (executeAction) announces its end to that agent — the
// project's architect, whichever of its sessions runs (a pending notice
// outlives an architect-session replacement), or the exact worker session
// that asked (gone session: failed, never redirected). A manual launch has no
// agent caller and nobody to tell. Pending-state ends (denied, could not
// start, abandoned, restart before approval) are reported by the executeAction
// call itself and are not executions: no notice.
//
// Exactly one notice per execution, whatever ends it. An execution leaves
// running once (PublishActionConclusion and FinishActionRun are compare-and-set
// on running), and every path that can win that write — the agent's approved
// conclusion, the user's cancel, a launch or resume failure, the restart
// reconciliation — begins the same row (dedupe action_ended:<execution id>)
// before its write and settles it after. Settling re-renders the body from
// the execution record, so the row says what actually happened even when the
// path that began it lost the race; a path that began the row and lost simply
// leaves it to the winner. A crash between the write and the settle leaves a
// pending_commit row that reconciliation settles the same way. Denied
// conclusions and artifact preparation never reach the record and never
// notify.

// actionEndedDedupeKey is the identity of an execution's final notice.
func actionEndedDedupeKey(executionID string) string {
	return fmt.Sprintf("%s:%s", domain.AgentMessageActionEnded, executionID)
}

// actionEndedRecipient is who asked for the execution, or "" for a manual
// launch.
func actionEndedRecipient(run domain.ActionRun) string {
	switch {
	case run.ArchitectKey == "" || run.Trigger == domain.ActionRunTriggerManual:
		return ""
	case run.Trigger == domain.ActionRunTriggerWorker && run.RequesterSessionID != "":
		return domain.AgentMessageRecipientSession + run.RequesterSessionID
	default:
		return domain.AgentMessageRecipientArchitect + run.ArchitectKey
	}
}

// actionEndedText renders the notice for an execution in its final state (or,
// at begin time, in the state the caller is about to write). It names what
// ended and how, points at getActionResult, and never claims a package is on
// the caller's machine: the copy is made on the caller's first read, and only
// artifacts.state says when it is usable.
func actionEndedText(run domain.ActionRun, pkg *domain.ActionArtifactRecord) (string, map[string]any) {
	body := fmt.Sprintf("Action %s execution %s has ended. Outcome: %s", run.Action, run.ID, run.Status)
	if run.Status == domain.ActionRunFailed && run.Summary == "" && run.Error != "" {
		body += " (" + run.Error + ")"
	}
	what := "record"
	if run.Summary != "" {
		what = "summary"
	}
	body += ". Use getActionResult with this execution id to read its " + what
	if pkg == nil {
		body += ". No artifact package was published."
	} else {
		files := "files"
		if pkg.Files == 1 {
			files = "file"
		}
		body += fmt.Sprintf("; its artifact package (%d %s) is copied to your machine on that first read, and output_dir is set once artifacts.state is ready.", pkg.Files, files)
	}
	payload := map[string]any{
		"execution_id":         run.ID,
		"action":               run.Action,
		"outcome":              string(run.Status),
		"error":                run.Error,
		"package":              pkg != nil,
		"requester_session_id": run.RequesterSessionID,
	}
	return body, payload
}

// beginActionEndedMessage records the pending_commit notice for an execution
// about to leave running, or nothing for a manual launch. An existing row —
// begun by another path, or left by an earlier attempt — is returned as it
// is: the settle after the write renders the final text.
func (s *Service) beginActionEndedMessage(ctx context.Context, run domain.ActionRun, pkg *domain.ActionArtifactRecord) (domain.AgentMessage, error) {
	recipient := actionEndedRecipient(run)
	if recipient == "" {
		return domain.AgentMessage{}, nil
	}
	body, payload := actionEndedText(run, pkg)
	return s.BeginAgentMessage(ctx, AgentMessageParams{
		Kind:         domain.AgentMessageActionEnded,
		Recipient:    recipient,
		ArchitectKey: run.ArchitectKey,
		DedupeKey:    actionEndedDedupeKey(run.ID),
		Body:         body,
		Payload:      payload,
	})
}

// settleActionEndedMessage makes the notice deliverable with its text taken
// from the execution record, right after the record was written. A settle
// that cannot run (the record cannot be read back) is logged and left to
// reconciliation; the execution is already final.
func (s *Service) settleActionEndedMessage(ctx context.Context, notice domain.AgentMessage, executionID string) {
	if notice.ID == "" {
		s.kickMessages()
		return
	}
	if s.actions != nil {
		if obligations, ok := s.actions.runs.(domain.ActionNotificationRepository); ok {
			if err := obligations.ClearActionNotification(ctx, executionID); err != nil {
				s.logger.Error("clear action notification obligation", "execution_id", executionID, "error", err)
			}
		}
	}
	if err := s.settleAgentMessage(ctx, notice.ID); err != nil {
		s.logger.Error("settle action ended notice; reconciliation will settle it", "message_id", notice.ID, "execution_id", executionID, "error", err)
	}
}

// checkActionEndedCommit settles a pending_commit notice against the
// execution record: completed or failed → committed with the text rendered
// from the record and its published package; gone, or ended without ever
// running → failed; still running → kept with its condition.
func (s *Service) checkActionEndedCommit(ctx context.Context, m domain.AgentMessage) messageCommitVerdict {
	executionID, _ := m.Payload["execution_id"].(string)
	if executionID == "" {
		return messageCommitVerdict{failed: "malformed action_ended payload: execution id missing"}
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
	switch run.Status {
	case domain.ActionRunCompleted, domain.ActionRunFailed:
		if run.StartedAt == nil {
			return messageCommitVerdict{failed: "the request never ran (" + string(run.Status) + "); the executeAction call reported it"}
		}
		pkg, err := s.publishedPackage(ctx, run)
		if err != nil {
			return messageCommitVerdict{condition: "package of execution " + executionID + " cannot be read: " + err.Error()}
		}
		body, payload := actionEndedText(run, pkg)
		return messageCommitVerdict{committed: true, body: body, payload: payload}
	case domain.ActionRunDenied:
		return messageCommitVerdict{failed: "the request was denied; it never ran"}
	default:
		return messageCommitVerdict{condition: fmt.Sprintf("the execution is still %s", run.Status)}
	}
}

// publishedPackage is the execution's published artifact package, if any.
func (s *Service) publishedPackage(ctx context.Context, run domain.ActionRun) (*domain.ActionArtifactRecord, error) {
	pkg, _, err := s.artifactRecords(ctx, run.ID, run.Machine)
	if err != nil {
		return nil, err
	}
	if pkg == nil || pkg.State == domain.ActionArtifactsPrepared {
		return nil, nil
	}
	return pkg, nil
}

// repairActionNotifications transfers durable terminal obligations into the
// outbox. Begin is idempotent; clear only after it succeeds. The final text is
// always rendered from the winning execution state, not the failed producer.
func (s *Service) repairActionNotifications(ctx context.Context) error {
	if s.actions == nil {
		return nil
	}
	obligations, ok := s.actions.runs.(domain.ActionNotificationRepository)
	if !ok {
		return nil
	}
	runs, err := obligations.PendingActionNotifications(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		notice, err := s.beginActionEndedMessage(ctx, run, nil)
		if err != nil {
			return fmt.Errorf("repair action notice %s: %w", run.ID, err)
		}
		if notice.ID == "" {
			return fmt.Errorf("action notice %s has no recipient", run.ID)
		}
		if err := obligations.ClearActionNotification(ctx, run.ID); err != nil {
			return err
		}
		s.settleActionEndedMessage(ctx, notice, run.ID)
	}
	return nil
}
