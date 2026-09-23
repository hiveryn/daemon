package sessionruntime

import (
	"context"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// Intent lifecycle events. These get their own top-level Type rather than
// another value on the already-overloaded "status" type (which carries
// ended/raw agentruntime statuses all at once).
const (
	sessionEventTypeIntent    = "intent"
	sessionEventStatusReqd    = "required"
	sessionEventStatusResolvd = "resolved"
)

// intentOrigin derives an intent's origin from the session record. Origin is
// never taken from the agent: the architect key comes from the stored session,
// so an agent cannot address an architect it wasn't spawned for.
func intentOrigin(session domain.Session) domain.IntentOrigin {
	origin := domain.IntentOrigin{
		ArchitectKey: session.ArchitectKey,
		SessionID:    session.ID,
		SessionType:  session.SessionType,
	}
	if session.SessionType == domain.SessionTypeTicket {
		origin.TicketID = session.ContextID
	}
	return origin
}

// publishIntentRequired emits the durable event that drives the desktop popup.
// It carries everything a generic popup needs — id, type, summary, payload,
// origin, countdown, and the policy that will fire if nobody answers — so the
// desktop needs no per-tool knowledge to render it.
func (s *Service) publishIntentRequired(ctx context.Context, in domain.Intent) error {
	raw := map[string]any{
		"intent_id":    in.ID,
		"intent_type":  string(in.Type),
		"summary":      in.Summary,
		"origin":       in.Origin,
		"wait_seconds": in.WaitSeconds,
		"policy":       string(in.Policy),
	}
	if len(in.Payload) > 0 {
		raw["payload"] = in.Payload
	}
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: in.Origin.SessionID,
		Type:      sessionEventTypeIntent,
		Status:    sessionEventStatusReqd,
		Tool:      string(in.Type),
		Message:   in.Summary,
		Raw:       raw,
		At:        in.CreatedAt,
	})
}

// publishIntentResolved is the durable counterpart to publishIntentRequired.
// The pending intent lives only in memory, so without a resolution event a
// reconnecting desktop would replay the required event into a stale,
// unactionable popup. Pairing is by raw.intent_id — never by session id, since
// a session can have several intents open at once.
func (s *Service) publishIntentResolved(ctx context.Context, in domain.Intent, res intentResult) error {
	raw := map[string]any{
		"intent_id":   in.ID,
		"intent_type": string(in.Type),
		"outcome":     string(res.Outcome),
	}
	if res.Reason != "" {
		raw["reason"] = res.Reason
	}
	if res.Result != nil {
		raw["result"] = res.Result
	}
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: in.Origin.SessionID,
		Type:      sessionEventTypeIntent,
		Status:    sessionEventStatusResolvd,
		Tool:      string(in.Type),
		Message:   in.Summary,
		Raw:       raw,
		At:        time.Now().UTC(),
	})
}

// unresolvedIntents replays a session's durable log and returns the intents
// that were never resolved, in the order they were raised.
//
// Walks oldest-first, adding on required and removing on resolved, keyed by
// intent id. A session end clears everything: the session is over, so no popup
// can be actionable. This replaces the old single-approval-per-session scan,
// which was structurally unable to represent N open intents.
//
// sessionID is passed in rather than read back out of the events: the caller
// listed these events for a known session, so that is the authoritative value.
func unresolvedIntents(sessionID string, events []domain.SessionEvent) []domain.Intent {
	order := []string{}
	open := map[string]domain.Intent{}

	for _, event := range events {
		if event.Type == "status" && event.Status == "ended" {
			order = order[:0]
			open = map[string]domain.Intent{}
			continue
		}
		if event.Type != sessionEventTypeIntent {
			continue
		}
		id, _ := event.Raw["intent_id"].(string)
		if id == "" {
			continue
		}
		switch event.Status {
		case sessionEventStatusReqd:
			if _, seen := open[id]; !seen {
				order = append(order, id)
			}
			open[id] = intentFromEventRaw(sessionID, id, event)
		case sessionEventStatusResolvd:
			if _, seen := open[id]; seen {
				delete(open, id)
				for i, existing := range order {
					if existing == id {
						order = append(order[:i], order[i+1:]...)
						break
					}
				}
			}
		}
	}

	out := make([]domain.Intent, 0, len(order))
	for _, id := range order {
		out = append(out, open[id])
	}
	return out
}

// intentFromEventRaw rebuilds just enough of an Intent from a replayed event to
// emit its resolution. Fields absent from the log stay zero; the reconciler only
// needs id, type, summary, and origin.
func intentFromEventRaw(sessionID, id string, event domain.SessionEvent) domain.Intent {
	in := domain.Intent{
		ID:      id,
		Summary: event.Message,
		Origin:  domain.IntentOrigin{SessionID: sessionID},
	}
	if t, ok := event.Raw["intent_type"].(string); ok {
		in.Type = domain.IntentType(t)
	}
	if origin, ok := event.Raw["origin"].(map[string]any); ok {
		if v, ok := origin["architect_key"].(string); ok {
			in.Origin.ArchitectKey = v
		}
		if v, ok := origin["session_type"].(string); ok {
			in.Origin.SessionType = domain.SessionType(v)
		}
		if v, ok := origin["ticket_id"].(string); ok {
			in.Origin.TicketID = v
		}
	}
	return in
}

// ReconcileIntents runs once at daemon startup. The intent store is in-memory
// and therefore empty after a restart, so any intent still unresolved in a
// session's durable log is by definition orphaned and would replay into a stale
// popup on the next desktop reconnect. Resolving each one converges the log to
// "no popup".
//
// The outcome is `error`, not a dedicated daemon_restart value: nothing is
// waiting on it (the agent's process died with the daemon), so widening the
// agent-facing outcome catalog with a value no agent can ever observe would be
// worse than reusing error with a reason.
func (s *Service) ReconcileIntents(ctx context.Context) error {
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		events, err := s.repo.ListSessionEvents(ctx, session.ID)
		if err != nil {
			return err
		}
		for _, orphan := range unresolvedIntents(session.ID, events) {
			if err := s.publishIntentResolved(ctx, orphan, intentResult{
				Outcome: domain.IntentOutcomeError,
				Reason:  "daemon restarted before this intent resolved",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
