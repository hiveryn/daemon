package sessionruntime

import (
	"context"

	"github.com/hiveryn/agentruntime"

	"github.com/hiveryn/daemon/internal/domain"
)

// noAttentionCoverage is the coverage of a provider whose adapter detects no
// attention at all.
const noAttentionCoverage = "This provider reports no attention signals; prompts are not detected."

// startAttention registers the attention monitor of an Action session's new
// main terminal, or returns nil for other session types. The caller has
// already closed the previous terminal's monitor (cancelReceiverBridge).
func (s *Service) startAttention(session domain.Session, agentKind agentruntime.AgentKind, size terminalSize) *attentionMonitor {
	if session.SessionType != domain.SessionTypeAction {
		return nil
	}
	detector, _ := s.adapters[agentKind].(agentruntime.AttentionDetector)
	coverage := noAttentionCoverage
	if detector != nil {
		coverage = detector.AttentionCoverage()
	}
	executionID := session.ContextID
	monitor := newAttentionMonitor(detector, coverage, size, func() { s.publishActionAttention(executionID) })
	s.bridgeMu.Lock()
	if s.attention == nil {
		s.attention = map[string]*attentionMonitor{}
	}
	s.attention[session.ID] = monitor
	s.bridgeMu.Unlock()
	return monitor
}

func (s *Service) attentionMonitor(sessionID string) *attentionMonitor {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	return s.attention[sessionID]
}

// closeAttention stops a replaced or ended terminal's monitor. Clearing a
// reported attention is a change a waiter must see; the monitor is already
// unregistered, so the announcement reads the execution as having none.
func (s *Service) closeAttention(monitor *attentionMonitor) {
	if monitor != nil && monitor.Close() {
		monitor.onChange()
	}
}

// actionAttention is the attention of the agent of a running execution.
// Without a live main terminal nothing can be inspected.
func (s *Service) actionAttention(sessionID string) domain.ActionAgentAttention {
	if monitor := s.attentionMonitor(sessionID); monitor != nil {
		return monitor.Current()
	}
	return domain.ActionAgentAttention{State: domain.ActionAttentionUnavailable}
}

// withAttention attaches the live attention to a running execution.
func (s *Service) withAttention(run domain.ActionRun) domain.ActionRun {
	if run.Status == domain.ActionRunRunning && run.SessionID != "" {
		attention := s.actionAttention(run.SessionID)
		run.Attention = &attention
	}
	return run
}

// publishActionAttention announces on the actions stream that a running
// execution's agent attention changed, so the Actions window refetches and
// waitForActionResult rereads.
func (s *Service) publishActionAttention(executionID string) {
	rt, err := s.actionRuntime()
	if err != nil {
		return
	}
	run, err := rt.runs.GetActionRun(context.Background(), executionID)
	if err != nil {
		s.logger.Error("read action execution to announce attention", "execution_id", executionID, "error", err)
		return
	}
	if run.Status != domain.ActionRunRunning {
		return
	}
	attention := s.actionAttention(run.SessionID)
	s.logger.Info("action agent attention changed", "action", run.Action, "execution_id", run.ID, "session_id", run.SessionID, "state", attention.State, "reason", attention.Reason, "source", attention.Source)
	s.publishActionEvent(run, run.Status)
}
