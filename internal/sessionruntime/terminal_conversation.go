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
// None of these touch the TUI's composer, so a laptop draft is preserved. The
// dispatcher (messages_delivery.go) runs these with the outbox under one
// in-flight-per-session rule. A dispatch reservation (status started, provider
// reference, hand-off time) is durable before any hand-off. Completion is only
// explicit native turn-end evidence for the turn containing that input; an
// input that cannot be found after the record passed the hand-off and the agent
// went idle becomes uncertain and is never sent again automatically.

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
		return terminalRoute{kind, "claude_async_rewake", "Sent through Claude's asyncRewake hook only while the agent is idle at its prompt; Claude records it as a task notification (native provenance), not as typed terminal input. Waits while the agent is busy."}, true
	case agentruntime.AgentCodex:
		return terminalRoute{kind, "codex_thread_queue", "Queued in Codex's durable thread queue and run as its own native user turn once the thread is idle. After an Esc-interrupted turn Codex holds the queue until the next laptop turn completes."}, true
	case agentruntime.AgentOpenCode:
		return terminalRoute{kind, "opencode_prompt_async", "Posted to the TUI's local session server only while the agent is idle; laptop input submitted during that turn can merge into it (native OpenCode behaviour), which is reported rather than hidden."}, true
	}
	return terminalRoute{}, false
}

// terminalCapabilities describes history/send for a terminal-owned session.
func (s *Service) terminalCapabilities(session domain.Session) domain.ConversationCapabilities {
	c := domain.ConversationCapabilities{ExecutionLocation: "local"}
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
	route, ok := terminalRouteFor(kind)
	_, reads := s.adapters[kind].(agentruntime.HistoryReader)
	if !ok || !reads {
		c.Limitation = fmt.Sprintf("%s terminal conversations are not supported", kind)
		return c
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
	for _, item := range s.classifyTerminalHistory(session, history, subs) {
		if previous, ok := st.put[item.ID]; ok && previous == item {
			continue
		}
		if err := repo.PutConversationItem(ctx, session.ID, item); err != nil {
			st.err = err
			return history, err
		}
		st.put[item.ID] = item
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
	// Outbox notifications (messageText); Claude prefixes hook text with its
	// own label, which is not part of the message.
	if at := strings.Index(text, "Hiveryn message "); at >= 0 && at < 200 {
		return terminalInput{via: "notification", text: strings.TrimSpace(text[at:])}
	}
	if prompt := strings.TrimSpace(session.Prompt); prompt != "" && strings.HasPrefix(strings.TrimSpace(text), prompt) {
		return terminalInput{via: "kickoff"}
	}
	return terminalInput{via: "terminal", text: text}
}

// classifyTerminalHistory turns native items into conversation items. The
// launch prompt (role/kickoff instructions and injected context) is hidden;
// API submissions show the text the client submitted, without correlation
// framing, and name their submission; items of a submission's turn name it too.
func (s *Service) classifyTerminalHistory(session domain.Session, history agentruntime.History, subs map[string]domain.Submission) []domain.ConversationItem {
	turnSubmission := map[string]string{}
	out := make([]domain.ConversationItem, 0, len(history.Items))
	for _, item := range history.Items {
		ci := domain.ConversationItem{ID: item.ID, NativeTurnID: item.TurnID, Role: item.Role, Origin: "user", Text: item.Text, Tool: item.Tool, Status: item.Status, At: item.At}
		if item.Role == "user" {
			input := classifyTerminalInput(session, item, subs)
			if input.via == "kickoff" {
				continue
			}
			ci.Via, ci.NativeOrigin, ci.Text = input.via, item.NativeOrigin, input.text
			if input.via == "notification" {
				ci.Origin = "notification"
			}
			if input.submission != "" && item.TurnID != "" {
				turnSubmission[item.TurnID] = input.submission
			}
			ci.SubmissionID = input.submission
		} else {
			ci.SubmissionID = turnSubmission[item.TurnID]
		}
		out = append(out, ci)
	}
	return out
}

// submitTerminal admits a conversation-API input for a terminal session.
func (s *Service) submitTerminal(ctx context.Context, session domain.Session, req domain.SubmissionRequest) (domain.Submission, error) {
	caps := s.terminalCapabilities(session)
	if !caps.Send {
		return domain.Submission{}, &domain.ConflictError{Resource: "session", Message: "send is unavailable: " + caps.Limitation}
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return domain.Submission{}, err
	}
	sub, err := repo.EnqueueSubmission(ctx, session.ID, req, "user")
	if err != nil {
		return sub, err
	}
	s.kickMessages()
	return sub, nil
}

// dispatchTerminalSubmissions confirms started submissions and hands over the
// next queued one per running local terminal session. inflight is shared with
// the outbox: one native hand-off per session at a time. It reports whether
// any terminal submission is still open (the caller re-checks soon).
func (s *Service) dispatchTerminalSubmissions(ctx context.Context, sessions []domain.Session, inflight map[string]bool) bool {
	trepo, err := s.terminalConversationRepo()
	if err != nil {
		return false
	}
	open := false
	for i := range sessions {
		session := sessions[i]
		if session.InteractionMode == domain.InteractionChat || session.Machine != "" {
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
	running := session.CurrentRun != nil && session.CurrentRun.Status == domain.SessionRunStatusRunning
	var history agentruntime.History
	var historyErr error
	if session.CurrentRun != nil && session.CurrentRun.NativeID != "" {
		// Throttled: hook events kick the dispatcher many times per turn.
		history, historyErr = s.syncTerminalHistory(ctx, session, false)
	}
	for _, sub := range subs {
		if sub.Status != "started" {
			continue
		}
		before := sub.Submission
		s.confirmTerminalSubmission(ctx, session, &sub, history, historyErr)
		if sub.Status == "started" {
			inflight[session.ID] = true
		}
		if sub.Submission != before {
			if err := repo.UpdateSubmission(ctx, sub.Submission); err != nil {
				s.logger.Error("update terminal submission", "submission_id", sub.ID, "error", err)
			}
		}
	}
	for _, sub := range subs {
		if sub.Status != "queued" {
			continue
		}
		before := sub.Error
		switch {
		case !running:
			sub.Error = "Waiting: the session is not running"
		case inflight[session.ID]:
			sub.Error = "Waiting for the previous input to this session to appear in its native record"
		default:
			s.handOffTerminalSubmission(ctx, session, &sub)
			if sub.Status == "started" {
				inflight[session.ID] = true
			}
		}
		if sub.Status != "queued" || sub.Error != before {
			if err := repo.UpdateSubmission(ctx, sub.Submission); err != nil {
				s.logger.Error("record terminal submission wait", "submission_id", sub.ID, "error", err)
			}
		}
		// Only the oldest queued input is considered: admission order is kept.
		break
	}
}

// confirmTerminalSubmission settles a started submission from native evidence.
func (s *Service) confirmTerminalSubmission(ctx context.Context, session domain.Session, sub *domain.TerminalSubmission, history agentruntime.History, historyErr error) {
	turn := ""
	if historyErr == nil {
		subs := map[string]domain.Submission{sub.ID: sub.Submission}
		if sub.ProviderRef != "" {
			subs["ref:"+sub.ProviderRef] = sub.Submission
		}
		for _, item := range history.Items {
			if item.Role == "user" && classifyTerminalInput(session, item, subs).submission == sub.ID {
				turn = item.TurnID
				break
			}
		}
	}
	if turn != "" {
		sub.NativeTurnID = turn
		laterEnded := false
		seen := false
		for _, item := range history.Items {
			if item.TurnID == turn {
				seen = true
				if item.TurnEnd {
					switch item.Status {
					case "completed":
						sub.Status, sub.Error = "completed", ""
					case "interrupted":
						sub.Status, sub.Error = "failed", "The native turn was interrupted"
					default:
						sub.Status, sub.Error = "failed", "The native turn failed: "+item.Text
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
			sub.Status = "uncertain"
			sub.Error = "The input appeared in the native conversation, but its turn has no completion record while a later turn completed (the provider merged later input into it); visible replies are in history"
			return
		}
		// A provider process that died mid-turn never completes it; the resumed
		// process starts from the next input.
		if s.processReplaced(session.ID, sub) {
			sub.Status = "uncertain"
			sub.Error = "The provider process was restarted during this turn and its completion was never recorded; visible output is in history. Not resent"
			return
		}
		sub.Error = "Running in the native conversation"
		return
	}
	now := s.terminalNow()
	idle, idleAt := s.messageIdle(session.ID)
	running := session.CurrentRun != nil && session.CurrentRun.Status == domain.SessionRunStatusRunning
	switch {
	case historyErr != nil:
		sub.Error = "Handed over; the native record is not readable yet: " + historyErr.Error()
		if now.Sub(sub.HandedAt) > terminalHandedCap {
			sub.Status = "uncertain"
			sub.Error = "The native record could not be read for " + terminalHandedCap.String() + " after the hand-off: " + historyErr.Error() + "; not resent"
		}
	case s.terminalInputQueued(ctx, session, sub):
		sub.Error = "Queued natively; it runs as the agent's next turn once idle"
	case !running:
		sub.Status = "uncertain"
		sub.Error = "The session stopped before the input appeared in its native record; not resent"
	case s.processReplaced(session.ID, sub) && history.RecordedThrough.After(sub.HandedAt) && idle && idleAt.After(sub.HandedAt.Add(terminalLostAfter)):
		sub.Status = "uncertain"
		sub.Error = "The provider process was restarted after the hand-off and the input is not in the native record; not resent"
	case history.RecordedThrough.After(sub.HandedAt) && idle && idleAt.After(sub.HandedAt.Add(terminalLostAfter)):
		sub.Status = "uncertain"
		sub.Error = "Not found in the native record after the agent went idle again; not resent"
	case now.Sub(sub.HandedAt) > terminalHandedCap:
		sub.Status = "uncertain"
		sub.Error = "Not found in the native record " + terminalHandedCap.String() + " after the hand-off; not resent"
	default:
		sub.Error = "Handed over; awaiting the native record"
	}
}

// processReplaced reports that the provider process which received the input
// is gone: the session's live main terminal (held in memory, not in SQLite)
// is no longer the one handed to — an unexpected exit resumed, or the daemon
// restarted and restored the session.
func (s *Service) processReplaced(sessionID string, sub *domain.TerminalSubmission) bool {
	current := s.mainTerminalID(sessionID)
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
	if root := adapter.ConfigRoot(env); root != "" {
		env["CODEX_HOME"] = root
	}
	confirmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	receipt, err := deliverer.ConfirmMessage(confirmCtx, agentruntime.ConfirmRequest{Env: env, NativeSessionID: run.NativeID, MessageID: sub.ID})
	return err == nil && receipt.Queued
}

// handOffTerminalSubmission reserves and hands over one queued submission.
func (s *Service) handOffTerminalSubmission(ctx context.Context, session domain.Session, sub *domain.TerminalSubmission) {
	run := *session.CurrentRun
	if run.ProfileSnapshot == nil || run.NativeID == "" {
		sub.Error = "Waiting: the native session id is not known yet"
		return
	}
	kind, err := parseAgentKind(run.ProfileSnapshot.Agent)
	if err != nil {
		sub.Error = "Waiting: " + err.Error()
		return
	}
	trepo, err := s.terminalConversationRepo()
	if err != nil {
		sub.Error = err.Error()
		return
	}
	deliverer, _, ok := s.messageDeliverer(kind)
	if !ok {
		sub.Error = fmt.Sprintf("Waiting: %s cannot receive input", kind)
		return
	}
	idle, _ := s.messageIdle(session.ID)
	reserve := func(ref string) bool {
		now := s.terminalNow().UTC()
		terminalID := s.mainTerminalID(session.ID)
		reserved, err := trepo.ReserveTerminalSubmission(ctx, session.ID, sub.ID, ref, terminalID, now)
		if err != nil {
			sub.Error = "Waiting: could not record the dispatch reservation: " + err.Error()
			return false
		}
		if !reserved {
			return false
		}
		sub.Status, sub.ProviderRef, sub.HandedAt, sub.HandedTerminal, sub.Error = "started", ref, now, terminalID, ""
		return true
	}
	switch kind {
	case agentruntime.AgentClaude:
		if _, armed := s.messages.mailbox.Armed(session.ID); !armed {
			sub.Error = "Waiting: Claude has no armed input waiter yet; its next turn end arms one"
			return
		}
		if !idle {
			sub.Error = "Waiting: the agent is busy; sent at its next idle point"
			return
		}
		release := s.holdTerminalInput(session)
		time.Sleep(terminalHoldSettle)
		if idle, _ = s.messageIdle(session.ID); !idle {
			release()
			sub.Error = "Waiting: laptop input started a turn; sent at the next idle point"
			return
		}
		if !reserve(sub.ID) {
			release()
			return
		}
		text := claudeInputHeader + sub.ID + " — a message from the user sent from the Hiveryn conversation client (not typed in this terminal). Reply to it as you would to the user here:\n\n" + sub.Text
		if _, delivered := s.messages.mailbox.Deliver(session.ID, ingest.MailboxMessage{ID: sub.ID, Text: text}); !delivered {
			release()
			// The waiter left before taking it: nothing reached the provider.
			if err := trepo.ReleaseTerminalSubmission(ctx, session.ID, sub.ID, "Waiting: the waiter left before the hand-off"); err != nil {
				s.logger.Error("release terminal submission", "submission_id", sub.ID, "error", err)
				return
			}
			sub.Status, sub.Error = "queued", "Waiting: the waiter left before the hand-off"
			return
		}
		s.setMessageIdle(session.ID, false)
		sub.Error = "Handed to Claude's asyncRewake waiter; awaiting the native record"
	case agentruntime.AgentCodex:
		if !reserve(sub.ID) {
			return
		}
		deliverCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := deliverer.DeliverMessage(deliverCtx, agentruntime.DeliverRequest{Env: cloneStringMap(run.ProfileSnapshot.Env), NativeSessionID: run.NativeID, MessageID: sub.ID, Text: sub.Text})
		cancel()
		if err != nil {
			sub.Error = "Hand-off acceptance unknown: " + err.Error() + "; the native queue and record are checked before anything else happens"
			return
		}
		sub.Error = "Queued natively; it runs as the agent's next turn once idle"
	case agentruntime.AgentOpenCode:
		endpoint, ok := s.messageEndpoint(session.ID)
		if !ok {
			sub.Error = "Waiting: the TUI's session server is unknown to this daemon process"
			return
		}
		req := agentruntime.DeliverRequest{Env: cloneStringMap(run.ProfileSnapshot.Env), NativeSessionID: run.NativeID, MessageID: sub.ID, Text: sub.Text, ServerPort: endpoint.port, ServerPassword: endpoint.password}
		// The TUI's own server is authoritative for idle; the hook-fed gate can
		// lag (or be unset after a daemon restart).
		if prober, ok := deliverer.(agentruntime.IdleProber); ok {
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			idle, err = prober.ProbeIdle(probeCtx, req)
			cancel()
			if err != nil {
				sub.Error = "Waiting: the session server's run state is unreadable: " + err.Error()
				return
			}
		}
		if !idle {
			sub.Error = "Waiting: the agent is busy; sent at its next idle point"
			return
		}
		release := s.holdTerminalInput(session)
		time.Sleep(terminalHoldSettle)
		if prober, ok := deliverer.(agentruntime.IdleProber); ok {
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			idle, err = prober.ProbeIdle(probeCtx, req)
			cancel()
			if err != nil || !idle {
				release()
				sub.Error = "Waiting: laptop input started a turn; sent at the next idle point"
				return
			}
		}
		handed := false
		defer func() {
			if !handed {
				release()
			}
		}()
		preparer, ok := deliverer.(agentruntime.MessagePreparer)
		if !ok {
			sub.Error = "Waiting: the OpenCode adapter cannot choose a message identity"
			return
		}
		prepareCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		ref, err := preparer.PrepareMessage(prepareCtx, req)
		cancel()
		if err != nil {
			sub.Error = "Waiting: " + err.Error()
			return
		}
		if !reserve(ref) {
			return
		}
		req.ProviderRef = ref
		deliverCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = deliverer.DeliverMessage(deliverCtx, req)
		cancel()
		if err != nil {
			sub.Error = "Hand-off acceptance unknown: " + err.Error() + "; the native record is checked before anything else happens"
			return
		}
		handed = true
		s.setMessageIdle(session.ID, false)
		sub.Error = "Accepted by the session server; awaiting the native record"
	default:
		sub.Error = fmt.Sprintf("Waiting: %s terminal input is unsupported", kind)
	}
}

// settleTerminalConversation runs a final native sync at session end and
// settles open submissions: never-handed input failed, handed input whose
// completion was not observed uncertain. Errors are logged; teardown proceeds.
func (s *Service) settleTerminalConversation(ctx context.Context, session domain.Session) {
	if session.InteractionMode == domain.InteractionChat || session.Machine != "" {
		return
	}
	defer s.forgetTerminalHistory(session.ID)
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
		if sub.Status == "started" {
			s.confirmTerminalSubmission(ctx, session, &sub, history, historyErr)
			if sub.Status == "started" {
				sub.Status, sub.Error = "uncertain", "The session ended before native completion of this input was observed"
			}
		} else {
			sub.Status, sub.Error = "failed", "Session ended before the input was handed over"
		}
		if err := repo.UpdateSubmission(ctx, sub.Submission); err != nil {
			s.logger.Error("settle terminal submission", "submission_id", sub.ID, "error", err)
		}
	}
}
