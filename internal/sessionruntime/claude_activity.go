package sessionruntime

import (
	"context"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/domain"
)

// Claude's idle gate is fed by its ordered hook events. When this daemon holds
// no hook evidence for a session (it restarted while a remote worker kept
// running, or the remote attachment and the reverse tunnel its hooks travel
// through were down), its activity is unknown: a waiter re-arming then proves
// only that a hand-off can reach the session. The native record is the
// evidence that can establish idleness again without waiting for the user's
// next turn: Claude writes a system turn_duration entry when a turn ends, so a
// turn end after the latest input means the agent sits at its prompt. Without
// one (a turn still running, or one that ended abnormally) activity stays
// unknown and input is refused until a hook event says otherwise.

// claudeIdleProbeTimeout bounds an asynchronous native-record idle probe.
var claudeIdleProbeTimeout = 30 * time.Second

// claudeActivityUnknown explains an unknown gate in refusals and conditions.
const claudeActivityUnknown = "Claude's activity is unknown to this daemon (no hook event since it started observing the session, or hooks may have been lost while the remote attachment was down)"

// probeClaudeIdle returns the session's gate, establishing it from the native
// record when no hook evidence is held. The record's verdict is applied only
// if no hook event changed the gate while it was read.
func (s *Service) probeClaudeIdle(ctx context.Context, session domain.Session) (bool, error) {
	idle, known, stamp := s.messageActivity(session.ID)
	if known {
		return idle, nil
	}
	history, err := s.readTerminalRecord(ctx, session)
	if err != nil {
		return false, err
	}
	if !nativeTurnEnded(history) {
		return false, nil
	}
	if s.setMessageIdleUnchanged(session.ID, stamp) {
		s.logger.Info("claude activity established from the native record", "session_id", session.ID, "recorded_through", history.RecordedThrough)
		s.kickMessages()
		return true, nil
	}
	// A hook event decided meanwhile; it is newer than the record read.
	idle, _ = s.messageIdle(session.ID)
	return idle, nil
}

// probeClaudeIdleAsync runs probeClaudeIdle in the background for a Claude
// session whose waiter armed while its activity is unknown (at most one probe
// per session at a time); an established idle kicks the dispatcher.
func (s *Service) probeClaudeIdleAsync(sessionID string) {
	if s.messages == nil || s.repo == nil {
		return
	}
	s.messages.mu.Lock()
	if s.messages.probing[sessionID] {
		s.messages.mu.Unlock()
		return
	}
	s.messages.probing[sessionID] = true
	s.messages.mu.Unlock()
	go func() {
		defer func() {
			s.messages.mu.Lock()
			delete(s.messages.probing, sessionID)
			s.messages.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), claudeIdleProbeTimeout)
		defer cancel()
		session, err := s.repo.GetSession(ctx, sessionID)
		if err != nil {
			return
		}
		run := session.CurrentRun
		if run == nil || run.Status != domain.SessionRunStatusRunning || run.ProfileSnapshot == nil || run.ProfileSnapshot.Agent != string(agentruntime.AgentClaude) || run.NativeID == "" {
			return
		}
		if _, err := s.probeClaudeIdle(ctx, session); err != nil {
			s.logger.Warn("claude activity probe: native record unreadable", "session_id", sessionID, "error", err)
		}
	}()
}

// nativeTurnEnded reports native evidence that the latest turn ended: a turn
// end recorded after the latest user input. An empty record (the kickoff not
// written yet) is no evidence.
func nativeTurnEnded(history agentruntime.History) bool {
	ended := false
	for _, item := range history.Items {
		switch {
		case item.TurnEnd:
			ended = true
		case item.Role == "user":
			ended = false
		}
	}
	return ended
}
