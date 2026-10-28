package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// Conversation continuity for existing terminal-owned sessions.
//
// The laptop keeps its native TUI; the daemon already owns that session's PTY,
// native identity, hook state and the provider's own message route. A second
// client (the conversation API) reads the provider's durable record and sends
// input through that route into the SAME native conversation:
//
//   - Claude: the asyncRewake waiter armed by the session's Stop/SessionStart
//     hooks, only while idle. Claude records the input as a task_notification
//     turn, never as typed terminal input; that native provenance is kept.
//   - Codex: the durable thread queue (clientUserMessageId = submission id),
//     a genuine native user message (turn trigger "queue") that the TUI runs as
//     its own turn once idle.
//   - OpenCode: prompt_async on the TUI's embedded server with a daemon-chosen
//     message id, only while idle.
//
// None of these touch the TUI's composer, so a laptop draft is preserved. A
// send hands over synchronously (submit.go) under the session's hand-off slot,
// shared with the outbox. The reservation (provider reference, hand-off time,
// receiving terminal) is durable before any hand-off. The dispatcher only
// reconciles afterwards: completion is explicit native turn-end evidence for
// the turn containing that input; an input that cannot be found after the
// record passed the hand-off and the agent went idle becomes uncertain and is
// never sent again automatically.

// terminalHistoryMinInterval throttles native record reads per session
// (a variable so tests can read on every pass).
var terminalHistoryMinInterval = time.Second

const (
	// terminalLostAfter: idle this long after a hand-off without the input in
	// the record makes the submission uncertain.
	terminalLostAfter = 10 * time.Second
	// terminalHandedCap bounds a hand-off that is never confirmed at all.
	terminalHandedCap = 15 * time.Minute
	// terminalRecheck is how soon the dispatcher looks again while terminal
	// submissions are open (native records are written just after hook events).
	terminalRecheck = 2 * time.Second
	// terminalInputHold bounds how long laptop keystrokes to the agent's
	// terminal are buffered around a hand-off; the hold ends earlier when the
	// agent reports it started working.
	terminalInputHold = 3 * time.Second
	// terminalHoldSettle lets a laptop submission already on its way surface as
	// a hook event before the idle state is checked again under the hold.
	terminalHoldSettle = 300 * time.Millisecond
	// claudeInputHeader prefixes the hook text Claude receives, so the record can
	// be correlated: Claude's transcript carries no caller identity of its own.
	claudeInputHeader = "Hiveryn conversation input "
)

type terminalHistoryState struct {
	mu       sync.Mutex
	syncedAt time.Time
	history  agentruntime.History
	err      error
	put      map[string]domain.ConversationItem
}

// terminalRoute describes the provider route of a local terminal session.
type terminalRoute struct {
	kind  agentruntime.AgentKind
	name  string
	limit string
}

func terminalRouteFor(kind agentruntime.AgentKind) (terminalRoute, bool) {
	switch kind {
	case agentruntime.AgentClaude:
		return terminalRoute{kind, "claude_async_rewake", "Sent through Claude's asyncRewake hook only while the agent is idle at its prompt; Claude records it as a task notification (native provenance), not as typed terminal input. Refused while the agent is busy."}, true
	case agentruntime.AgentCodex:
		return terminalRoute{kind, "codex_thread_queue", "Queued in Codex's durable thread queue and run as its own native user turn once the thread is idle. After an Esc-interrupted turn Codex holds the queue until the next laptop turn completes."}, true
	case agentruntime.AgentOpenCode:
		return terminalRoute{kind, "opencode_prompt_async", "Posted to the TUI's local session server only while the agent is idle; laptop input submitted during that turn can merge into it (native OpenCode behaviour), which is reported rather than hidden."}, true
	}
	return terminalRoute{}, false
}

// conversationUnsupported names why a terminal session has no conversation
// route at all ("" when it has one): an agent without a reader or input route,
// or a remote location the route cannot reach.
func (s *Service) conversationUnsupported(session domain.Session, kind agentruntime.AgentKind) string {
	_, ok := terminalRouteFor(kind)
	_, reads := s.adapters[kind].(agentruntime.HistoryReader)
	if !ok || !reads {
		return fmt.Sprintf("%s terminal conversations are not supported", kind)
	}
	return s.remoteConversationUnsupported(session, kind)
}

// terminalConversationUnsupported reports a session whose agent/location has
// no conversation route at all (as opposed to one unavailable right now).
func (s *Service) terminalConversationUnsupported(session domain.Session) bool {
	run := session.CurrentRun
	if run == nil || run.ProfileSnapshot == nil {
		return false
	}
	kind, err := parseAgentKind(run.ProfileSnapshot.Agent)
	return err == nil && s.conversationUnsupported(session, kind) != ""
}

// remoteRouteLimit is the extra caveat of a route reached over SSH.
func remoteRouteLimit(kind agentruntime.AgentKind, machine string) string {
	limit := " Remote worker on " + config.MachineLabel(machine) + ": history is read over SSH (one command per read on a reused connection, typically about one network round trip or two) and an unreachable machine is an explicit error, never empty history."
	switch kind {
	case agentruntime.AgentClaude:
		limit += " Input reaches Claude's waiter through the worker's reverse tunnel, so a send is refused (SEND_UNAVAILABLE) while the worker's SSH attachment is down or before the worker armed a waiter (workers launched before remote conversation support never do)."
	case agentruntime.AgentCodex:
		limit += " Input is queued by running `codex app-server` on that machine over SSH; a connection lost after the request was sent is DELIVERY_UNKNOWN and reconciled from the queue and rollout."
	}
	return limit
}

// terminalCapabilities describes history/send for a terminal-owned session.
func (s *Service) terminalCapabilities(session domain.Session) domain.ConversationCapabilities {
	c := domain.ConversationCapabilities{ExecutionLocation: "local"}
	if session.Machine != "" {
		c.ExecutionLocation, c.Machine = "remote", session.Machine
	}
	run := session.CurrentRun
	if run == nil || run.ProfileSnapshot == nil {
		c.Limitation = "No run: the native conversation is unknown"
		return c
	}
	kind, err := parseAgentKind(run.ProfileSnapshot.Agent)
	if err != nil {
		c.Limitation = "Unknown agent: " + err.Error()
		return c
	}
	if reason := s.conversationUnsupported(session, kind); reason != "" {
		c.Limitation = reason
		return c
	}
	route, _ := terminalRouteFor(kind)
	if session.Machine != "" {
		route.limit += remoteRouteLimit(kind, session.Machine)
	}
	if run.NativeID == "" {
		c.Limitation = "The native session id is not known yet (no hook event received)"
		return c
	}
	c.History = true
	c.Limitation = route.limit
	if run.Status != domain.SessionRunStatusRunning {
		c.Limitation = "Session is not running; history is read-only"
		return c
	}
	if kind == agentruntime.AgentOpenCode {
		if _, ok := s.messageEndpoint(session.ID); !ok {
			c.Limitation = "The TUI's session server is unknown to this daemon process (launched before it started); history is read-only until the session is relaunched"
			return c
		}
	}
	c.Send = true
	c.InputRoute = route.name
	return c
}

type inputHolder interface {
	HoldInput(sessionID, terminalID string, d time.Duration) func()
}

// holdTerminalInput buffers laptop input to the session's main terminal for
// at most terminalInputHold; the returned release (also called when the agent
// reports working) flushes it in order.
func (s *Service) holdTerminalInput(session domain.Session) func() {
	holder, ok := s.terminal.(inputHolder)
	terminalID := s.mainTerminalID(session.ID)
	if !ok || terminalID == "" {
		return func() {}
	}
	release := holder.HoldInput(session.ID, terminalID, terminalInputHold)
	if s.messages == nil {
		return release
	}
	s.messages.mu.Lock()
	if s.messages.holds == nil {
		s.messages.holds = map[string]func(){}
	}
	s.messages.holds[session.ID] = release
	s.messages.mu.Unlock()
	return release
}

// releaseTerminalInput ends a session's input hold once the agent is working.
func (s *Service) releaseTerminalInput(sessionID string) {
	if s.messages == nil {
		return
	}
	s.messages.mu.Lock()
	release := s.messages.holds[sessionID]
	delete(s.messages.holds, sessionID)
	s.messages.mu.Unlock()
	if release != nil {
		release()
	}
}

// terminalNow is the dispatcher's clock (tests control it), or wall time
// when message delivery is not configured.
func (s *Service) terminalNow() time.Time {
	if s.messages == nil {
		return time.Now()
	}
	return s.messages.now()
}

func (s *Service) terminalConversationRepo() (domain.TerminalConversationRepository, error) {
	r, ok := s.repo.(domain.TerminalConversationRepository)
	if !ok {
		return nil, fmt.Errorf("terminal conversation persistence is not configured")
	}
	return r, nil
}

// readTerminalRecord reads the current run's native record.
func (s *Service) readTerminalRecord(ctx context.Context, session domain.Session) (agentruntime.History, error) {
	run := session.CurrentRun
	if run == nil || run.ProfileSnapshot == nil || run.NativeID == "" {
		return agentruntime.History{}, &domain.ConflictError{Resource: "conversation", Message: "the native session of this terminal session is not known yet"}
	}
	kind, err := parseAgentKind(run.ProfileSnapshot.Agent)
	if err != nil {
		return agentruntime.History{}, err
	}
	adapter := s.adapters[kind]
	reader, ok := adapter.(agentruntime.HistoryReader)
	if !ok {
		return agentruntime.History{}, &domain.ConflictError{Resource: "conversation", Message: fmt.Sprintf("%s terminal history is unsupported", kind)}
	}
	if session.Machine != "" {
		// Never the laptop's provider home: the record lives on the worker's machine.
		return s.readRemoteRecord(ctx, session, kind, adapter, reader)
	}
	env := cloneStringMap(run.ProfileSnapshot.Env)
	req := agentruntime.HistoryRequest{NativeSessionID: run.NativeID, ConfigRoot: adapter.ConfigRoot(env), Env: env}
	if kind == agentruntime.AgentClaude {
		req.TranscriptHandle, err = adapter.LocateTranscript(ctx, agentruntime.LocateRequest{NativeSessionID: run.NativeID, ConfigRoot: req.ConfigRoot, Workdir: run.Workdir})
		if err != nil {
			return agentruntime.History{}, fmt.Errorf("locate native record: %w", err)
		}
	}
	return reader.ReadHistory(ctx, req)
}

func (s *Service) terminalHistoryState(id string) *terminalHistoryState {
	s.terminalHistoryMu.Lock()
	defer s.terminalHistoryMu.Unlock()
	if s.terminalHistory == nil {
		s.terminalHistory = map[string]*terminalHistoryState{}
	}
	st := s.terminalHistory[id]
	if st == nil {
		st = &terminalHistoryState{put: map[string]domain.ConversationItem{}}
		s.terminalHistory[id] = st
	}
	return st
}

func (s *Service) forgetTerminalHistory(id string) {
	s.terminalHistoryMu.Lock()
	delete(s.terminalHistory, id)
	s.terminalHistoryMu.Unlock()
}

// syncTerminalHistory reads the native record (throttled) and persists its
// normalized items into the conversation version feed. force bypasses the
// throttle (final sync at session end).
func (s *Service) syncTerminalHistory(ctx context.Context, session domain.Session, force bool) (agentruntime.History, error) {
	st := s.terminalHistoryState(session.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !force && time.Since(st.syncedAt) < terminalHistoryMinInterval {
		return st.history, st.err
	}
	history, err := s.readTerminalRecord(ctx, session)
	st.syncedAt = time.Now()
	st.err = err
	if err != nil {
		return history, err
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return history, err
	}
	subs, err := s.terminalSubmissionIndex(ctx, session.ID)
	if err != nil {
		st.err = err
		return history, err
	}
	// The whole record goes to the store in native order whenever anything
	// changed, so an item first seen late is placed among its native
	// neighbours instead of after everything already shown.
	items := s.classifyTerminalHistory(session, history, subs)
	changed := false
	for _, item := range items {
		if previous, ok := st.put[item.ID]; !ok || previous != item {
			changed = true
			break
		}
	}
	if changed {
		if err := repo.PutConversationItems(ctx, session.ID, items); err != nil {
			st.err = err
			return history, err
		}
		for _, item := range items {
			st.put[item.ID] = item
		}
	}
	st.history = history
	return history, nil
}

// terminalSubmissionIndex maps every conversation-API submission of the
// session by id and provider reference.
func (s *Service) terminalSubmissionIndex(ctx context.Context, id string) (map[string]domain.Submission, error) {
	repo, err := s.conversationRepo()
	if err != nil {
		return nil, err
	}
	trepo, err := s.terminalConversationRepo()
	if err != nil {
		return nil, err
	}
	index := map[string]domain.Submission{}
	for after := int64(0); ; {
		page, err := repo.ListSubmissions(ctx, id, domain.SequenceQuery{AfterSequence: after, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, sub := range page.Submissions {
			index[sub.ID] = sub
		}
		if !page.HasMore {
			break
		}
		after = page.NextSequence
	}
	refs, err := trepo.TerminalProviderRefs(ctx, id)
	if err != nil {
		return nil, err
	}
	for ref, subID := range refs {
		if sub, ok := index[subID]; ok {
			index["ref:"+ref] = sub
		}
	}
	return index, nil
}

// terminalInput identifies what a native user input is.
type terminalInput struct {
	via        string // terminal, api, notification, kickoff
	submission string
	text       string
}

func classifyTerminalInput(session domain.Session, item agentruntime.HistoryItem, subs map[string]domain.Submission) terminalInput {
	text := item.Text
	// Claude prefixes hook text with its own label ("Stop hook blocking error
	// from command ..."); the correlation header follows it.
	if at := strings.Index(text, claudeInputHeader); at >= 0 && at < 200 {
		rest := text[at+len(claudeInputHeader):]
		id, _, _ := strings.Cut(rest, " ")
		if sub, ok := subs[id]; ok {
			return terminalInput{via: "api", submission: sub.ID, text: sub.Text}
		}
	}
	if item.Ref != "" {
		if sub, ok := subs[item.Ref]; ok {
			return terminalInput{via: "api", submission: sub.ID, text: sub.Text}
		}
		if sub, ok := subs["ref:"+item.Ref]; ok {
			return terminalInput{via: "api", submission: sub.ID, text: sub.Text}
		}
	}
	// The launch message: the stored kickoff, followed for workers by the
	// injected Overview/workflows. The record's own text is kept verbatim.
	if prompt := strings.TrimSpace(session.Prompt); prompt != "" && strings.HasPrefix(strings.TrimSpace(text), prompt) {
		return terminalInput{via: "kickoff", text: text}
	}
	// Outbox notifications (messageText); Claude prefixes hook text with its
	// own label, which is not part of the message.
	if at := strings.Index(text, "Hiveryn message "); at >= 0 && at < 200 {
		return terminalInput{via: "notification", text: strings.TrimSpace(text[at:])}
	}
	return terminalInput{via: "terminal", text: text}
}

// classifyTerminalHistory turns native items into conversation items. The
// launch message is shown as recorded with kickoff provenance, and the items
// of its turn carry origin kickoff; API submissions show the text the client
// submitted, without correlation framing, and name their submission; items of
// a submission's turn name it too.
func (s *Service) classifyTerminalHistory(session domain.Session, history agentruntime.History, subs map[string]domain.Submission) []domain.ConversationItem {
	turnSubmission := map[string]string{}
	kickoffTurn := map[string]bool{}
	out := make([]domain.ConversationItem, 0, len(history.Items))
	for _, item := range history.Items {
		ci := domain.ConversationItem{ID: item.ID, NativeTurnID: item.TurnID, Role: item.Role, Origin: "user", Text: item.Text, Tool: item.Tool, Status: item.Status, At: item.At}
		if item.Role == "user" {
			input := classifyTerminalInput(session, item, subs)
			ci.Via, ci.NativeOrigin, ci.Text = input.via, item.NativeOrigin, input.text
			switch input.via {
			case "notification", "kickoff":
				ci.Origin = input.via
			}
			if input.via == "kickoff" && item.TurnID != "" {
				kickoffTurn[item.TurnID] = true
			}
			if input.submission != "" && item.TurnID != "" {
				turnSubmission[item.TurnID] = input.submission
			}
			ci.SubmissionID = input.submission
		} else {
			ci.SubmissionID = turnSubmission[item.TurnID]
			if kickoffTurn[item.TurnID] {
				ci.Origin = "kickoff"
			}
		}
		out = append(out, ci)
	}
	return out
}

// dispatchTerminalSubmissions settles accepted and unknown input from native
// evidence (delivery itself is synchronous: submit.go). inflight is shared
// with the outbox: a notification waits while this session's input is still
// open. It reports whether any terminal submission is still open (the caller
// re-checks soon).
func (s *Service) dispatchTerminalSubmissions(ctx context.Context, sessions []domain.Session, inflight map[string]bool) bool {
	trepo, err := s.terminalConversationRepo()
	if err != nil {
		return false
	}
	open := false
	for i := range sessions {
		session := sessions[i]
		if session.InteractionMode == domain.InteractionChat {
			continue
		}
		subs, err := trepo.OpenTerminalSubmissions(ctx, session.ID)
		if err != nil {
			s.logger.Error("list terminal submissions", "session_id", session.ID, "error", err)
			continue
		}
		if len(subs) == 0 {
			continue
		}
		open = true
		s.progressTerminalSession(ctx, session, subs, inflight)
	}
	return open
}

func (s *Service) progressTerminalSession(ctx context.Context, session domain.Session, subs []domain.TerminalSubmission, inflight map[string]bool) {
	repo, err := s.conversationRepo()
	if err != nil {
		return
	}
	var history agentruntime.History
	var historyErr error
	if session.CurrentRun != nil && session.CurrentRun.NativeID != "" {
		// Throttled: hook events kick the dispatcher many times per turn.
		history, historyErr = s.syncTerminalHistory(ctx, session, false)
	}
	for _, sub := range subs {
		if sub.Status == domain.SubmissionDelivering {
			// Its synchronous attempt owns it (and the hand-off slot).
			inflight[session.ID] = true
			continue
		}
		before := sub.Submission
		s.confirmTerminalSubmission(ctx, session, &sub, history, historyErr)
		if terminalSubmissionOpen(sub.Status) {
			inflight[session.ID] = true
		}
		if !sameSubmission(sub.Submission, before) {
			if err := repo.UpdateSubmission(ctx, sub.Submission); err != nil {
				s.logger.Error("update terminal submission", "submission_id", sub.ID, "error", err)
			}
		}
	}
}

func terminalSubmissionOpen(status string) bool {
	switch status {
	case domain.SubmissionDelivering, domain.SubmissionUnknown, domain.SubmissionAccepted, domain.SubmissionRunning:
		return true
	}
	return false
}

func sameSubmission(a, b domain.Submission) bool {
	return a.Status == b.Status && a.Error == b.Error && a.ErrorCode == b.ErrorCode && a.NativeTurnID == b.NativeTurnID &&
		(a.AcceptedAt == nil) == (b.AcceptedAt == nil)
}

// acceptTerminal records established provider acceptance.
func (s *Service) acceptTerminal(sub *domain.TerminalSubmission, status, evidence, detail string) {
	if sub.AcceptedAt == nil {
		now := s.terminalNow().UTC()
		sub.AcceptedAt, sub.Acceptance = &now, evidence
	}
	sub.Status, sub.ErrorCode, sub.Error = status, "", detail
}

// confirmTerminalSubmission settles accepted/unknown/running input from
// native evidence. Unknown input found in the record (or Codex's queue) was
// accepted after all; input never found is settled uncertain and never resent.
func (s *Service) confirmTerminalSubmission(ctx context.Context, session domain.Session, sub *domain.TerminalSubmission, history agentruntime.History, historyErr error) {
	turn := ""
	if historyErr == nil {
		turn = terminalInputTurn(session, history, sub)
	}
	if turn != "" {
		sub.NativeTurnID = turn
		s.acceptTerminal(sub, domain.SubmissionRunning, "native_record", "Running in the native conversation")
		laterEnded := false
		seen := false
		for _, item := range history.Items {
			if item.TurnID == turn {
				seen = true
				if item.TurnEnd {
					switch item.Status {
					case "completed":
						sub.Status, sub.Error = domain.SubmissionCompleted, ""
					case "interrupted":
						sub.Status, sub.Error = domain.SubmissionFailed, "The native turn was interrupted"
					default:
						sub.Status, sub.Error = domain.SubmissionFailed, "The native turn failed: "+item.Text
					}
					return
				}
				continue
			}
			if seen && item.TurnEnd {
				laterEnded = true
			}
		}
		if laterEnded {
			sub.Status = domain.SubmissionUncertain
			sub.Error = "The input appeared in the native conversation, but its turn has no completion record while a later turn completed (the provider merged later input into it); visible replies are in history"
			return
		}
		// A provider process that died mid-turn never completes it; the resumed
		// process starts from the next input.
		if s.processReplaced(session, sub) {
			sub.Status = domain.SubmissionUncertain
			sub.Error = "The provider process was restarted during this turn and its completion was never recorded; visible output is in history. Not resent"
		}
		return
	}
	now := s.terminalNow()
	idle, idleAt := s.messageIdle(session.ID)
	running := session.CurrentRun != nil && session.CurrentRun.Status == domain.SessionRunStatusRunning
	// What could not be found: the input (acceptance unknown) or its turn.
	what := "The input"
	if sub.AcceptedAt == nil {
		what = "The input, whose acceptance was never established,"
	}
	switch {
	case historyErr != nil:
		if now.Sub(sub.HandedAt) > terminalHandedCap {
			sub.Status = domain.SubmissionUncertain
			sub.Error = "The native record could not be read for " + terminalHandedCap.String() + " after the hand-off: " + historyErr.Error() + "; not resent"
		}
	case s.terminalInputQueued(ctx, session, sub):
		s.acceptTerminal(sub, domain.SubmissionAccepted, "codex_thread_queue", "Queued natively; it runs as the agent's next turn once idle")
	case !running:
		sub.Status = domain.SubmissionUncertain
		sub.Error = what + " did not appear in the native record before the session stopped; not resent"
	case s.processReplaced(session, sub) && history.RecordedThrough.After(sub.HandedAt) && idle && idleAt.After(sub.HandedAt.Add(terminalLostAfter)):
		sub.Status = domain.SubmissionUncertain
		sub.Error = "The provider process was restarted after the hand-off and the input is not in the native record; not resent"
	case history.RecordedThrough.After(sub.HandedAt) && idle && idleAt.After(sub.HandedAt.Add(terminalLostAfter)):
		sub.Status = domain.SubmissionUncertain
		sub.Error = what + " was not found in the native record after the agent went idle again; not resent"
	case now.Sub(sub.HandedAt) > terminalHandedCap:
		sub.Status = domain.SubmissionUncertain
		sub.Error = what + " was not found in the native record " + terminalHandedCap.String() + " after the hand-off; not resent"
	}
}

// terminalInputTurn returns the native turn holding the submission's input.
func terminalInputTurn(session domain.Session, history agentruntime.History, sub *domain.TerminalSubmission) string {
	subs := map[string]domain.Submission{sub.ID: sub.Submission}
	if sub.ProviderRef != "" {
		subs["ref:"+sub.ProviderRef] = sub.Submission
	}
	for _, item := range history.Items {
		if item.Role == "user" && classifyTerminalInput(session, item, subs).submission == sub.ID {
			return item.TurnID
		}
	}
	return ""
}

// processReplaced reports that the provider process which received the input
// is gone: the session's live main terminal (held in memory, not in SQLite)
// is no longer the one handed to — an unexpected exit resumed, or the daemon
// restarted and restored the session.
//
// A remote worker's main terminal is only its SSH attachment: a reconnect
// replaces it while the provider keeps running in its tmux pane, and Hiveryn
// never restarts a remote provider, so it is never replaced.
func (s *Service) processReplaced(session domain.Session, sub *domain.TerminalSubmission) bool {
	if session.Machine != "" {
		return false
	}
	current := s.mainTerminalID(session.ID)
	return sub.HandedTerminal != "" && current != "" && current != sub.HandedTerminal
}

// terminalInputQueued asks Codex whether its durable queue still holds the input.
func (s *Service) terminalInputQueued(ctx context.Context, session domain.Session, sub *domain.TerminalSubmission) bool {
	run := session.CurrentRun
	if run == nil || run.ProfileSnapshot == nil || run.ProfileSnapshot.Agent != string(agentruntime.AgentCodex) {
		return false
	}
	deliverer, adapter, ok := s.messageDeliverer(agentruntime.AgentCodex)
	if !ok {
		return false
	}
	env := cloneStringMap(run.ProfileSnapshot.Env)
	confirmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if session.Machine != "" {
		// The queue lives on the worker's machine; the record was already read
		// from there. Each check starts an app-server over SSH, so it is spaced.
		inspector, ok := deliverer.(agentruntime.QueueInspector)
		if !ok {
			return false
		}
		return s.remoteMessageQueued(confirmCtx, session, sub.ID, func(ctx context.Context) (bool, error) {
			return inspector.MessageQueued(ctx, agentruntime.ConfirmRequest{Env: env, NativeSessionID: run.NativeID, MessageID: sub.ID, Exec: remoteProviderCommand(session.SSH)})
		})
	}
	if root := adapter.ConfigRoot(env); root != "" {
		env["CODEX_HOME"] = root
	}
	receipt, err := deliverer.ConfirmMessage(confirmCtx, agentruntime.ConfirmRequest{Env: env, NativeSessionID: run.NativeID, MessageID: sub.ID})
	return err == nil && receipt.Queued
}

// terminalRecordPoll is how often the native record is read while a hand-off
// waits for its acceptance evidence there.
var terminalRecordPoll = 250 * time.Millisecond

// deliverTerminalInput hands one input to a terminal session's provider route
// while holding the session's hand-off slot, and records the outcome: accepted
// (with its evidence), rejected (nothing reached the provider) or unknown.
func (s *Service) deliverTerminalInput(ctx context.Context, session domain.Session, submission domain.Submission) {
	sub := domain.TerminalSubmission{Submission: submission}
	s.handOffTerminalInput(ctx, session, &sub)
	if sub.Status == domain.SubmissionDelivering {
		sub.Status, sub.ErrorCode, sub.Error = domain.SubmissionUnknown, domain.DeliveryUnknown, "The hand-off ended without an outcome; acceptance is reconciled from the native record and the input is never resent"
	}
	s.persistSubmission(sub.Submission)
	s.logger.Info("conversation input delivery", "session_id", session.ID, "submission_id", sub.ID, "status", sub.Status, "code", sub.ErrorCode, "acceptance", sub.Acceptance)
}

func (s *Service) handOffTerminalInput(ctx context.Context, session domain.Session, sub *domain.TerminalSubmission) {
	reject := func(code, reason string) {
		sub.Status, sub.ErrorCode, sub.Error = domain.SubmissionRejected, code, "Not sent: "+reason
	}
	unknown := func(reason string) {
		sub.Status, sub.ErrorCode, sub.Error = domain.SubmissionUnknown, domain.DeliveryUnknown, reason+"; acceptance is reconciled from the native record and the input is never resent"
	}
	run := *session.CurrentRun
	kind, err := parseAgentKind(run.ProfileSnapshot.Agent)
	if err != nil {
		reject(domain.DeliverySendUnsupported, err.Error())
		return
	}
	trepo, err := s.terminalConversationRepo()
	if err != nil {
		reject(domain.DeliveryRejected, err.Error())
		return
	}
	deliverer, _, ok := s.messageDeliverer(kind)
	if !ok || s.messages == nil {
		reject(domain.DeliverySendUnsupported, fmt.Sprintf("%s cannot receive input", kind))
		return
	}
	// Claude and OpenCode take input only at idle; earlier input that has not
	// reached the record yet still owns that idle slot. Codex queues natively.
	if kind != agentruntime.AgentCodex {
		if pending := s.terminalInputPending(ctx, session.ID, sub.ID); pending != "" {
			reject(domain.DeliverySessionBusy, pending)
			return
		}
	}
	reserve := func(ref string) bool {
		if err := ctx.Err(); err != nil {
			reject(domain.DeliveryRejected, "the delivery bound expired before the hand-off")
			return false
		}
		now := s.terminalNow().UTC()
		terminalID := s.mainTerminalID(session.ID)
		reserved, err := trepo.ReserveTerminalSubmission(ctx, session.ID, sub.ID, ref, terminalID, now)
		if err != nil || !reserved {
			reject(domain.DeliveryRejected, fmt.Sprintf("the hand-off reservation could not be recorded (reserved=%v, error=%v)", reserved, err))
			return false
		}
		sub.ProviderRef, sub.HandedAt, sub.HandedTerminal = ref, now, terminalID
		return true
	}
	req := agentruntime.DeliverRequest{Env: cloneStringMap(run.ProfileSnapshot.Env), NativeSessionID: run.NativeID, MessageID: sub.ID, Text: sub.Text}
	idle, _ := s.messageIdle(session.ID)
	switch kind {
	case agentruntime.AgentClaude:
		// A remote Claude's waiter reaches the daemon only through the
		// attachment's reverse tunnel; while it is down the activity state is
		// stale and no waiter can take input.
		if state := s.remoteConnectionState(session); state != "" && state != "connected" {
			reject(domain.DeliverySendUnavailable, "the remote worker's SSH attachment is "+state+"; Claude's input waiter is reachable only through it")
			return
		}
		// A waiter re-arming after a restart or reconnect is no idle evidence:
		// without hook evidence the native record must show the turn ended.
		if _, known, _ := s.messageActivity(session.ID); !known {
			probed, err := s.probeClaudeIdle(ctx, session)
			if err != nil {
				reject(domain.DeliverySendUnavailable, claudeActivityUnknown+" and its native record could not be read: "+err.Error())
				return
			}
			if !probed {
				reject(domain.DeliverySessionBusy, claudeActivityUnknown+" and its native record shows no completed turn after the latest input; treated as working until it stops at its prompt")
				return
			}
			idle = true
		}
		if !idle {
			reject(domain.DeliverySessionBusy, "the agent is working; Claude takes input only at its idle prompt")
			return
		}
		if _, armed := s.messages.mailbox.Armed(session.ID); !armed {
			reject(domain.DeliverySendUnavailable, "Claude has no armed input waiter (one arms when the agent stops at its prompt)")
			return
		}
		release := s.holdTerminalInput(session)
		if !sleepContext(ctx, terminalHoldSettle) {
			release()
			reject(domain.DeliveryRejected, "the delivery bound expired before the hand-off")
			return
		}
		if idle, _ = s.messageIdle(session.ID); !idle {
			release()
			reject(domain.DeliverySessionBusy, "laptop input started a turn at the same moment")
			return
		}
		if !reserve(sub.ID) {
			release()
			return
		}
		text := claudeInputHeader + sub.ID + " — a message from the user sent from the Hiveryn conversation client (not typed in this terminal). Reply to it as you would to the user here:\n\n" + sub.Text
		if _, delivered := s.messages.mailbox.Deliver(session.ID, ingest.MailboxMessage{ID: sub.ID, Text: text}); !delivered {
			release()
			reject(domain.DeliverySendUnavailable, "Claude's input waiter left before the hand-off")
			return
		}
		s.setMessageIdle(session.ID, false)
		// The waiter taking it is not Claude consuming it: wait for the input
		// in Claude's own record.
		if turn := s.awaitTerminalRecord(ctx, session, sub); turn != "" {
			sub.NativeTurnID = turn
			s.acceptTerminal(sub, domain.SubmissionRunning, "claude_native_record", "Running in the native conversation")
			return
		}
		unknown("Handed to Claude's input waiter, but it did not appear in Claude's record within " + submitDeliveryBound.String())
	case agentruntime.AgentCodex:
		deliver := func(ctx context.Context) error {
			_, err := deliverer.DeliverMessage(ctx, req)
			return err
		}
		if session.Machine != "" {
			// The queue lives on the worker's machine: run app-server there
			// over the conversation's SSH master, which is established before
			// the reservation, so an unreachable machine is a known refusal.
			if err := s.withRemoteConversation(ctx, session, func(context.Context) error { return nil }); err != nil {
				reject(domain.DeliveryRejected, err.Error())
				return
			}
			req.Exec = remoteProviderCommand(session.SSH)
			local := deliver
			deliver = func(ctx context.Context) error {
				return s.withRemoteConversation(ctx, session, local)
			}
		}
		if !reserve(sub.ID) {
			return
		}
		if err := deliver(ctx); err != nil {
			if agentruntime.IsNotDelivered(err) {
				reject(domain.DeliveryRejected, "Codex did not take the input: "+err.Error())
				return
			}
			unknown("Codex queue hand-off acceptance unknown: " + err.Error())
			return
		}
		s.acceptTerminal(sub, domain.SubmissionAccepted, "codex_thread_queue", "Queued in Codex's durable thread queue; it runs as the agent's next turn once idle")
	case agentruntime.AgentOpenCode:
		endpoint, ok := s.messageEndpoint(session.ID)
		if !ok {
			reject(domain.DeliverySendUnavailable, "the TUI's session server is unknown to this daemon process")
			return
		}
		req.ServerPort, req.ServerPassword = endpoint.port, endpoint.password
		// The TUI's own server is authoritative for idle; the hook-fed gate can
		// lag (or be unset after a daemon restart).
		probe := func() (bool, error) {
			prober, ok := deliverer.(agentruntime.IdleProber)
			if !ok {
				idle, _ := s.messageIdle(session.ID)
				return idle, nil
			}
			return prober.ProbeIdle(ctx, req)
		}
		if idle, err = probe(); err != nil {
			reject(domain.DeliveryRejected, "the session server's run state is unreadable: "+err.Error())
			return
		}
		if !idle {
			reject(domain.DeliverySessionBusy, "the agent is working; OpenCode takes input only while idle")
			return
		}
		release := s.holdTerminalInput(session)
		handed := false
		defer func() {
			if !handed {
				release()
			}
		}()
		if !sleepContext(ctx, terminalHoldSettle) {
			reject(domain.DeliveryRejected, "the delivery bound expired before the hand-off")
			return
		}
		if idle, err = probe(); err != nil || !idle {
			reject(domain.DeliverySessionBusy, "laptop input started a turn at the same moment")
			return
		}
		preparer, ok := deliverer.(agentruntime.MessagePreparer)
		if !ok {
			reject(domain.DeliverySendUnsupported, "the OpenCode adapter cannot choose a message identity")
			return
		}
		ref, err := preparer.PrepareMessage(ctx, req)
		if err != nil {
			reject(domain.DeliveryRejected, "could not prepare the OpenCode message: "+err.Error())
			return
		}
		if !reserve(ref) {
			return
		}
		req.ProviderRef = ref
		if _, err := deliverer.DeliverMessage(ctx, req); err != nil {
			if agentruntime.IsNotDelivered(err) {
				reject(domain.DeliveryRejected, "OpenCode did not take the input: "+err.Error())
				return
			}
			handed = true // it may be running: the hold ends on its own
			unknown("OpenCode hand-off acceptance unknown: " + err.Error())
			return
		}
		handed = true
		s.setMessageIdle(session.ID, false)
		s.acceptTerminal(sub, domain.SubmissionAccepted, "opencode_prompt_async", "Accepted by the TUI's session server")
	default:
		reject(domain.DeliverySendUnsupported, fmt.Sprintf("%s terminal input is unsupported", kind))
	}
}

// terminalInputPending names earlier input to the session that has not
// reached its native record yet, or "".
func (s *Service) terminalInputPending(ctx context.Context, sessionID, except string) string {
	if trepo, err := s.terminalConversationRepo(); err == nil {
		subs, err := trepo.OpenTerminalSubmissions(ctx, sessionID)
		if err != nil {
			return "the session's open input is unreadable: " + err.Error()
		}
		for _, other := range subs {
			if other.ID != except && other.NativeTurnID == "" {
				return "earlier input " + other.ID + " (" + other.Status + ") has not reached the native conversation yet"
			}
		}
	}
	open, err := s.messages.repo.ListOpenAgentMessages(ctx)
	if err != nil {
		return "the session's open notifications are unreadable: " + err.Error()
	}
	for _, m := range open {
		if m.State == domain.AgentMessageHanded && m.TargetSessionID == sessionID {
			return "a Hiveryn notification handed to this session has not reached its record yet"
		}
	}
	return ""
}

// awaitTerminalRecord polls the native record until the input appears (its
// native turn id) or the attempt's bound ends ("").
func (s *Service) awaitTerminalRecord(ctx context.Context, session domain.Session, sub *domain.TerminalSubmission) string {
	for {
		if history, err := s.syncTerminalHistory(ctx, session, true); err == nil {
			if turn := terminalInputTurn(session, history, sub); turn != "" {
				return turn
			}
		}
		if !sleepContext(ctx, terminalRecordPoll) {
			return ""
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// settleTerminalConversation runs a final native sync at session end and
// settles open submissions: input whose acceptance or completion was not
// observed becomes uncertain; an attempt never handed over is rejected.
// Errors are logged; teardown proceeds.
func (s *Service) settleTerminalConversation(ctx context.Context, session domain.Session) {
	if session.InteractionMode == domain.InteractionChat {
		return
	}
	defer s.forgetTerminalHistory(session.ID)
	defer s.closeRemoteConversation(session.ID)
	// A synchronous hand-off in progress finishes (bounded) before settling.
	if err := s.waitHandoff(ctx, session.ID); err != nil {
		s.logger.Warn("wait for conversation hand-off before settling", "session_id", session.ID, "error", err)
	}
	trepo, err := s.terminalConversationRepo()
	if err != nil {
		return
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return
	}
	subs, err := trepo.OpenTerminalSubmissions(ctx, session.ID)
	if err != nil {
		s.logger.Error("settle terminal conversation", "session_id", session.ID, "error", err)
		return
	}
	syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	history, historyErr := s.syncTerminalHistory(syncCtx, session, true)
	cancel()
	if historyErr != nil && !errors.Is(historyErr, context.DeadlineExceeded) {
		s.logger.Warn("final terminal history sync", "session_id", session.ID, "error", historyErr)
	}
	for _, sub := range subs {
		switch {
		case sub.Status == domain.SubmissionDelivering && sub.HandedAt.IsZero():
			sub.Status, sub.ErrorCode, sub.Error = domain.SubmissionRejected, domain.DeliverySessionEnding, "Not sent: the session ended before the hand-off"
		default:
			s.confirmTerminalSubmission(ctx, session, &sub, history, historyErr)
			if terminalSubmissionOpen(sub.Status) {
				sub.Status, sub.Error = domain.SubmissionUncertain, "The session ended before native completion of this input was observed"
				if sub.AcceptedAt == nil {
					sub.Error = "The session ended before acceptance of this input was established; not resent"
				}
			}
		}
		if err := repo.UpdateSubmission(ctx, sub.Submission); err != nil {
			s.logger.Error("settle terminal submission", "submission_id", sub.ID, "error", err)
		}
	}
}
