package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

type chatOwner struct {
	cancel      context.CancelFunc
	cmd         *exec.Cmd
	exited      chan struct{}
	wake        chan struct{}
	ready       chan struct{}
	conn        agentruntime.Conversation
	diagnostics *chatDiagnostics
	once        sync.Once
}

func (s *Service) conversationRepo() (domain.ConversationRepository, error) {
	r, ok := s.repo.(domain.ConversationRepository)
	if !ok {
		return nil, fmt.Errorf("conversation persistence is not configured")
	}
	return r, nil
}
func (s *Service) chatCapabilities(session domain.Session) domain.ConversationCapabilities {
	c := domain.ConversationCapabilities{ExecutionLocation: "local", Limitation: "Terminal conversation submission is unsupported"}
	if session.Machine != "" {
		c.ExecutionLocation = "remote"
		c.Limitation = "Remote SSH chat is unsupported"
		return c
	}
	if session.InteractionMode != domain.InteractionChat {
		return s.terminalCapabilities(session)
	}
	c.History = true
	c.Send = session.CurrentRun != nil && session.CurrentRun.Status == domain.SessionRunStatusRunning
	if c.Send {
		c.InputRoute = "chat"
	}
	c.Limitation = "Native permission controls are unavailable; a permission request holds the chat"
	return c
}

// enqueueChat admits an internal entry (an outbox notification) into a chat
// owner's queue; it runs after the entries before it. User input never queues:
// it is delivered synchronously (submit.go).
func (s *Service) enqueueChat(ctx context.Context, id string, req domain.SubmissionRequest, origin string) (domain.Submission, error) {
	if err := validateSubmission(req); err != nil {
		return domain.Submission{}, err
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return domain.Submission{}, err
	}
	// Serialize admission with teardown and synchronous sends.
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	prior, lookupErr := repo.GetSubmission(ctx, id, domain.SubmissionIdentity(id, req.RequestID))
	if lookupErr == nil {
		if prior.Text != req.Text || prior.Origin != origin {
			return domain.Submission{}, &domain.ConflictError{Resource: "submission", Field: "request_id", Message: "request_id was already used with different text or origin"}
		}
		return prior, nil
	}
	var missing *domain.NotFoundError
	if !errors.As(lookupErr, &missing) {
		return domain.Submission{}, lookupErr
	}
	if _, ending := s.ending[id]; ending {
		return domain.Submission{}, &domain.ConflictError{Resource: "session", Message: "session is ending"}
	}
	session, err := s.repo.GetSession(ctx, id)
	if err != nil {
		return domain.Submission{}, err
	}
	if session.InteractionMode != domain.InteractionChat {
		return domain.Submission{}, &domain.ConflictError{Resource: "session", Message: "only chat sessions have an internal queue"}
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.Submission{}, &domain.ConflictError{Resource: "session", Message: "session is not running"}
	}
	sub, _, err := repo.InsertSubmission(ctx, id, req, origin, domain.SubmissionQueued, "", "")
	if err != nil {
		return sub, err
	}
	s.wakeChat(id)
	return sub, nil
}

// chatAdmissionRefusal: user input enters a chat only when its owner is ready
// and nothing is open in it — never behind another entry.
func (s *Service) chatAdmissionRefusal(ctx context.Context, session domain.Session) (string, string) {
	s.chatMu.Lock()
	owner := s.chats[session.ID]
	s.chatMu.Unlock()
	ready := false
	if owner != nil {
		select {
		case <-owner.ready:
			ready = true
		default:
		}
	}
	if !ready {
		return domain.DeliverySendUnavailable, "Not sent: the chat owner is not ready to take input"
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return domain.DeliveryRejected, "Not sent: " + err.Error()
	}
	head, err := repo.PendingSubmission(ctx, session.ID)
	if err != nil {
		return domain.DeliveryRejected, "Not sent: the chat state is unreadable: " + err.Error()
	}
	if head != nil {
		reason := "Not sent: the agent is busy with " + head.Origin + " input " + head.ID + " (" + head.Status + ")"
		if head.Status == domain.SubmissionUnknown || head.Status == domain.SubmissionUncertain || head.Status == domain.SubmissionBlocked {
			reason += "; it holds the chat until it is reconciled"
		}
		return domain.DeliverySessionBusy, reason
	}
	return "", ""
}

// deliverChatInput waits for the chat owner to take the input and for the
// provider's acceptance evidence, within the attempt's bound.
func (s *Service) deliverChatInput(ctx context.Context, session domain.Session, sub domain.Submission) {
	repo, err := s.conversationRepo()
	if err != nil {
		return
	}
	signal, stop := s.deliverySignal(sub.ID)
	defer stop()
	s.chatMu.Lock()
	owner := s.chats[session.ID]
	s.chatMu.Unlock()
	var exited <-chan struct{}
	if owner != nil {
		exited = owner.exited
	}
	s.wakeChat(session.ID)
	for {
		current, err := repo.GetSubmission(context.WithoutCancel(ctx), session.ID, sub.ID)
		if err == nil && (current.AcceptedAt != nil || current.Status != domain.SubmissionDelivering) {
			return
		}
		select {
		case <-signal:
			continue
		case <-exited:
		case <-ctx.Done():
		}
		break
	}
	settleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settled, err := repo.SettleDelivery(settleCtx, session.ID, sub.ID, domain.SubmissionRejected, domain.DeliveryRejected, "Not sent: the chat owner did not take the input within the delivery bound")
	if err == nil && !settled {
		_, err = repo.SettleDelivery(settleCtx, session.ID, sub.ID, domain.SubmissionUnknown, domain.DeliveryUnknown, "The chat owner started the turn but provider acceptance was not established within the delivery bound; it is reconciled from the native turn and never resent")
	}
	if err != nil {
		s.logger.Error("settle chat delivery", "submission_id", sub.ID, "error", err)
	}
}

// chatItem is the conversation item of one native event of sub's turn. A user
// item is the provider's echo of the input; via names how that input entered
// (the kickoff, an API submission or an outbox notification).
func chatItem(sub domain.Submission, e agentruntime.ConversationEvent) domain.ConversationItem {
	item := domain.ConversationItem{ID: e.ID, SubmissionID: sub.ID, NativeTurnID: e.TurnID, Role: e.Role, Origin: sub.Origin, Text: e.Text, Tool: e.Tool, Status: e.Status, At: e.At}
	if e.Role == "user" {
		item.Via = sub.Origin
		if sub.Origin == "user" {
			item.Via = "api"
		}
	}
	return item
}
func (s *Service) ReadConversation(ctx context.Context, id string, q domain.ConversationQuery) (domain.ConversationPage, error) {
	if q.AfterRevision < 0 || q.SnapshotRevision < 0 || q.Limit < 0 || q.Limit > 200 {
		return domain.ConversationPage{}, &domain.ValidationError{Field: "pagination", Message: "revisions must be nonnegative and limit at most 200"}
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return domain.ConversationPage{}, err
	}
	session, sessionErr := s.repo.GetSession(ctx, id)
	if sessionErr == nil && session.Machine != "" {
		return domain.ConversationPage{}, &domain.ConflictError{Resource: "conversation", Message: "history of remote SSH sessions is unsupported"}
	}
	if sessionErr == nil && session.InteractionMode != domain.InteractionChat {
		// Read the native record first; an unreadable record is an error, never
		// an empty or silently stale page.
		if !s.terminalCapabilities(session).History {
			return domain.ConversationPage{}, &domain.ConflictError{Resource: "conversation", Message: "history unavailable: " + s.terminalCapabilities(session).Limitation}
		}
		if _, err := s.syncTerminalHistory(ctx, session, false); err != nil {
			return domain.ConversationPage{}, fmt.Errorf("read native conversation record: %w", err)
		}
		s.kickMessages()
	}
	var missing *domain.NotFoundError
	if sessionErr != nil && !errors.As(sessionErr, &missing) {
		return domain.ConversationPage{}, sessionErr
	}
	page, err := repo.ReadConversation(ctx, id, q)
	if err == nil && sessionErr != nil && page.SnapshotRevision == 0 {
		archived, archiveErr := repo.IsArchivedChat(ctx, id)
		if archiveErr != nil {
			return page, archiveErr
		}
		if !archived {
			return page, sessionErr
		}
	}
	return page, err
}
func (s *Service) wakeChat(id string) {
	s.chatMu.Lock()
	owner := s.chats[id]
	s.chatMu.Unlock()
	if owner != nil {
		select {
		case owner.wake <- struct{}{}:
		default:
		}
	}
}
func (s *Service) activateChat(id string) {
	s.chatMu.Lock()
	owner := s.chats[id]
	s.chatMu.Unlock()
	if owner != nil {
		owner.once.Do(func() { close(owner.ready) })
		s.wakeChat(id)
	}
}

func (s *Service) launchChat(ctx context.Context, session domain.Session, run domain.SessionRun, profile config.VariantConfig, kind agentruntime.AgentKind, req agentruntime.StartRequest) error {
	if session.Machine != "" {
		return fmt.Errorf("remote SSH chat is unsupported")
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return err
	}
	archived, err := repo.IsArchivedChat(ctx, session.ID)
	if err != nil {
		return err
	}
	if archived {
		return &domain.ConflictError{Resource: "session", Message: "chat is archived"}
	}
	adapter, ok := s.adapters[kind].(agentruntime.ConversationAdapter)
	if !ok {
		return fmt.Errorf("%s has no structured conversation transport", kind)
	}
	req.ID = session.ID
	req.Agent = kind
	req.DisableNativeQuestions = true
	req.ClaudeAutoMemory = profile.ClaudeAutoMemory
	if err := configureOpenCodeArchitectAgent(session, kind, &req); err != nil {
		return err
	}
	req.MCPServers, err = s.mcpServersForSession(session.SessionType, session.ArchitectKey, session.ID, profile.MCP)
	if err != nil {
		return err
	}
	if kind == agentruntime.AgentOpenCode {
		req.MessageDelivery, err = s.messageDeliveryFor(session.ID, kind)
		if err != nil {
			return err
		}
	}
	spec, err := adapter.PrepareConversation(ctx, req)
	if err != nil {
		return fmt.Errorf("prepare %s chat: %w", kind, err)
	}
	cleanup := func() {
		for _, path := range spec.Launch.CleanupPaths {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.logger.Error("remove chat launch artifact", "error", err)
			}
		}
	}
	// The lock is inherited by the child. A previous owner surviving a daemon
	// crash prevents a second submitter, even if its native ID is already known.
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("hiveryn-chat-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		cleanup()
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, session.ID+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		cleanup()
		return err
	}
	ownershipDeadline := time.Now().Add(3 * time.Second)
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(ownershipDeadline) {
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		_ = lock.Close()
		cleanup()
		return fmt.Errorf("chat owner remains or ownership cannot be established: %w", err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	owner := &chatOwner{cancel: cancel, exited: make(chan struct{}), wake: make(chan struct{}, 1), ready: make(chan struct{})}
	watchRead, watchWrite, err := os.Pipe()
	if err != nil {
		cancel()
		_ = lock.Close()
		cleanup()
		return err
	}
	// A separate inherited read end observes daemon death without PID polling.
	// The small watcher lives in the owned process group; EOF terminates that
	// group on macOS as well as Linux. It also retains the ownership lock until
	// the previous group has stopped, preventing overlapping restart owners.
	argv := append([]string{"-c", `( IFS= read -r _ <&4; kill -TERM -- -$$ ) & exec "$@"`, "hiveryn-chat", spec.Launch.Command}, spec.Launch.Args...)
	cmd := exec.Command("/bin/sh", argv...)
	owner.cmd = cmd
	cmd.Dir = spec.Launch.Workdir
	cmd.Env = mergeProcessEnv(spec.Launch.Env, spec.Launch.Workdir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{lock, watchRead}
	stdout, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = watchRead.Close()
		_ = watchWrite.Close()
		cancel()
		_ = lock.Close()
		cleanup()
		return err
	}
	cmd.Stdout = stdoutWrite
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWrite.Close()
		_ = watchRead.Close()
		_ = watchWrite.Close()
		cancel()
		_ = lock.Close()
		cleanup()
		return err
	}
	stderr, stderrWrite, err := os.Pipe()
	if err != nil {
		cancel()
		_ = watchRead.Close()
		_ = watchWrite.Close()
		_ = stdout.Close()
		_ = stdoutWrite.Close()
		_ = lock.Close()
		cleanup()
		return err
	}
	diagnosticEnv := append([]string(nil), cmd.Env...)
	for _, server := range req.MCPServers {
		for key, value := range server.Env {
			diagnosticEnv = append(diagnosticEnv, key+"="+value)
		}
	}
	diagnostics := newChatDiagnostics(diagnosticEnv)
	owner.diagnostics = diagnostics
	stderrDone := make(chan struct{})
	cmd.Stderr = stderrWrite
	if err = cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutWrite.Close()
		_ = stderr.Close()
		_ = stderrWrite.Close()
		_ = watchRead.Close()
		_ = watchWrite.Close()
		cancel()
		_ = lock.Close()
		cleanup()
		return err
	}
	_ = watchRead.Close()
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	go func() { defer close(stderrDone); _, _ = io.Copy(diagnostics, stderr); _ = stderr.Close() }()
	s.chatMu.Lock()
	if s.chats == nil {
		s.chats = map[string]*chatOwner{}
	}
	s.chats[session.ID] = owner
	s.chatMu.Unlock()
	go func() {
		err := cmd.Wait()
		_ = watchWrite.Close()
		cancel()
		_ = lock.Close()
		close(owner.exited)
		cleanup()
		defer s.chatExited(session.ID, owner)
		if err != nil {
			s.logger.Error("chat provider exited", "session_id", session.ID, "error", err)
		}
	}()
	result := make(chan error, 1)
	go func() {
		conn, err := spec.Connect(lifetime, stdout, stdin)
		if err == nil {
			owner.conn = conn
		}
		result <- err
	}()
	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		_ = stdout.Close()
		_ = s.stopChat(context.Background(), session.ID, false)
		select {
		case <-stderrDone:
		case <-time.After(100 * time.Millisecond):
		}
		return fmt.Errorf("connect %s chat: %w; provider diagnostics: %s", kind, err, diagnostics.String())
	}
	if err = s.repo.UpdateRunNativeID(ctx, run.ID, owner.conn.NativeID()); err != nil {
		_ = stdout.Close()
		_ = s.stopChat(context.Background(), session.ID, false)
		return err
	}
	if req.Resume {
		if err = s.reconcileChat(ctx, session.ID, owner.conn); err != nil {
			s.logger.Error("reconcile native chat receipt", "session_id", session.ID, "error", err)
		}
		if err = repo.SettleSubmissions(ctx, session.ID, domain.SubmissionUncertain, "Provider owner restarted; prior in-flight delivery/completion is unconfirmed. Not replayed."); err != nil {
			_ = stdout.Close()
			_ = s.stopChat(context.Background(), session.ID, false)
			return err
		}
	} else {
		// Queue the unchanged launch prompt before exposing the live session. Its
		// native echo is the kickoff shown in the conversation.
		if _, _, err = repo.InsertSubmission(ctx, session.ID, domain.SubmissionRequest{RequestID: "kickoff:" + run.ID, Text: req.Prompt}, "kickoff", domain.SubmissionQueued, "", ""); err != nil {
			_ = stdout.Close()
			_ = s.stopChat(context.Background(), session.ID, false)
			return err
		}
	}
	go func() { <-lifetime.Done(); _ = stdout.Close() }()
	go s.runChat(lifetime, session, run, owner)
	if req.Resume {
		s.activateChat(session.ID)
	}
	return nil
}
func (s *Service) runChat(ctx context.Context, session domain.Session, run domain.SessionRun, owner *chatOwner) {
	repo, err := s.conversationRepo()
	if err != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-owner.ready:
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-owner.wake:
		}
		for ctx.Err() == nil {
			sub, err := repo.ReserveSubmission(ctx, session.ID)
			if err != nil {
				s.logger.Error("reserve chat submission", "error", err)
				return
			}
			if sub == nil {
				break
			}
			if err = s.setChatStatus(ctx, run, domain.AgentStatusActive); err != nil {
				s.logger.Error("set chat status", "error", err)
				return
			}
			complete := false
			err = owner.conn.Turn(ctx, agentruntime.ConversationInput{ID: sub.ID, Text: sub.Text, Origin: sub.Origin}, func(e agentruntime.ConversationEvent) error {
				if e.TurnID != "" {
					sub.NativeTurnID = e.TurnID
				}
				if e.Acceptance != "" && sub.AcceptedAt == nil {
					now := time.Now().UTC()
					sub.AcceptedAt, sub.Acceptance = &now, e.Acceptance
					sub.Status, sub.ErrorCode, sub.Error = domain.SubmissionRunning, "", ""
				}
				if e.Status == "blocked" {
					sub.Status = "blocked"
					sub.Error = e.Text
					if err := s.setChatStatus(ctx, run, domain.AgentStatusWaiting); err != nil {
						return err
					}
				}
				if e.Complete {
					complete = true
					sub.Status = e.Status
					sub.Error = ""
					if e.Status == "failed" {
						sub.Error = e.Text
					}
				}
				// The native user echo is the recorded input, the kickoff's
				// included; its submission's origin names how it entered.
				if e.ID != "" {
					if err := repo.PutConversationItems(ctx, session.ID, []domain.ConversationItem{chatItem(*sub, e)}); err != nil {
						return err
					}
				}
				if err := repo.UpdateSubmission(ctx, *sub); err != nil {
					return err
				}
				s.signalDelivery(sub.ID)
				return nil
			})
			if err != nil || !complete {
				// Without acceptance evidence the input itself is unconfirmed.
				sub.Status = domain.SubmissionUncertain
				sub.Error = "Native completion is unconfirmed; not replayed"
				if sub.AcceptedAt == nil {
					sub.Status, sub.ErrorCode = domain.SubmissionUnknown, domain.DeliveryUnknown
					sub.Error = "Provider acceptance is unconfirmed; not replayed"
				}
				if err != nil {
					sub.Error += ": " + err.Error()
				}
				if owner.diagnostics != nil && owner.diagnostics.String() != "" {
					sub.Error += "; provider diagnostics: " + owner.diagnostics.String()
				}
				saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				saveErr := repo.UpdateSubmission(saveCtx, *sub)
				if ctx.Err() == nil {
					if statusErr := s.setChatStatus(saveCtx, run, domain.AgentStatusWaiting); statusErr != nil {
						s.logger.Error("set chat waiting status", "error", statusErr)
					}
				}
				cancel()
				if saveErr != nil {
					s.logger.Error("persist uncertain chat submission", "error", saveErr)
				}
				s.signalDelivery(sub.ID)
				if ctx.Err() == nil {
					s.logger.Error("chat turn interrupted", "session_id", session.ID, "error", err)
				}
				return
			}
			if err = s.setChatStatus(ctx, run, domain.AgentStatusIdle); err != nil {
				s.logger.Error("set chat status", "error", err)
				return
			}
		}
	}
}
func (s *Service) stopChat(ctx context.Context, id string, settle bool) error {
	s.chatMu.Lock()
	owner := s.chats[id]
	s.chatMu.Unlock()
	// Terminal sessions are settled from their native record when they end
	// (settleTerminalConversation); only the chat queue is failed wholesale.
	if settle && s.isChatSession(ctx, id) {
		repo, err := s.conversationRepo()
		if err == nil {
			if err = repo.SettleSubmissions(ctx, id, "failed", "Session ended before submission completed"); err != nil {
				return err
			}
		}
	}
	s.chatMu.Lock()
	if s.chats[id] == owner {
		delete(s.chats, id)
	}
	s.chatMu.Unlock()
	if owner == nil {
		return nil
	}
	owner.cancel()
	select {
	case <-owner.exited:
		return nil
	default:
	}
	if err := syscall.Kill(-owner.cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case <-owner.exited:
		return nil
	case <-time.After(3 * time.Second):
	}
	if err := syscall.Kill(-owner.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case <-owner.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) chatExited(id string, owner *chatOwner) {
	s.chatMu.Lock()
	current := s.chats[id]
	s.chatMu.Unlock()
	if current != owner || !s.beginLaunch(id) {
		return
	}
	defer s.endLaunch(id)
	s.chatMu.Lock()
	if s.chats[id] != owner {
		s.chatMu.Unlock()
		return
	}
	delete(s.chats, id)
	s.chatMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), defaultRunLaunchTimeout)
	defer cancel()
	session, err := s.repo.GetSession(ctx, id)
	if err != nil || session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return
	}
	if err = s.restoreSession(ctx, session, *session.CurrentRun); err != nil {
		s.logger.Error("restore chat owner", "session_id", id, "error", err)
		s.failChatRestore(ctx, id, err)
		s.markRunFailed(session.CurrentRun.ID, domain.SessionRunFailureRestoreFailed, "chat_owner_exit")
		if session.SessionType == domain.SessionTypeTicket {
			if err = s.moveTicketToBacklog(ctx, session); err != nil {
				s.logger.Error("move failed chat ticket to backlog", "error", err)
			}
		}
	}
}

func (s *Service) deliverChatNotification(ctx context.Context, m *domain.AgentMessage, target messageTarget) {
	m.TargetSessionID = target.session.ID
	m.TargetAgent = string(target.agent)
	m.TargetNativeID = target.run.NativeID
	m.ProviderRef = "chat:" + domain.SubmissionIdentity(target.session.ID, "notification:"+m.ID)
	if !s.reserveMessageAttempt(ctx, m) {
		return
	}
	sub, err := s.enqueueChat(ctx, target.session.ID, domain.SubmissionRequest{RequestID: "notification:" + m.ID, Text: messageText(*m)}, "notification")
	if err != nil {
		m.LastError = err.Error()
		m.Condition = "chat notification admission failed: " + err.Error()
		return
	}
	s.markMessageHanded(m, "chat:"+sub.ID, "queued in the chat owner's internal queue under its sole submitter")
}
func (s *Service) confirmChatNotification(ctx context.Context, m *domain.AgentMessage) {
	repo, err := s.conversationRepo()
	if err != nil {
		m.Condition = "chat receipt unavailable: " + err.Error()
		return
	}
	sub, err := repo.GetSubmission(ctx, m.TargetSessionID, strings.TrimPrefix(m.ProviderRef, "chat:"))
	if err != nil {
		var missing *domain.NotFoundError
		if errors.As(err, &missing) { // reservation survived but queue insert did not
			_, admitErr := s.enqueueChat(ctx, m.TargetSessionID, domain.SubmissionRequest{RequestID: "notification:" + m.ID, Text: messageText(*m)}, "notification")
			if admitErr == nil {
				m.Condition = "chat queue admission recovered"
				return
			}
			err = admitErr
		}
		var gone *domain.NotFoundError
		if errors.As(err, &gone) {
			s.repend(m, "chat target ended before queue admission")
			return
		}
		m.Condition = "chat receipt unavailable: " + err.Error()
		return
	}
	switch sub.Status {
	case "completed":
		now := s.messages.now()
		m.State = domain.AgentMessageAcknowledged
		m.AppearedAt = &now
		m.AcknowledgedAt = &now
		m.Condition = ""
	case domain.SubmissionRejected:
		s.repend(m, "chat session ended before notification dispatch")
	case "failed":
		if sub.NativeTurnID == "" && strings.HasPrefix(sub.Error, "Session ended") {
			s.repend(m, "chat session ended before notification dispatch")
			return
		}
		s.markMessageFailed(m, "chat submission failed: "+sub.Error)
	default:
		m.Condition = "chat submission " + sub.Status
		if sub.Error != "" {
			m.Condition += ": " + sub.Error
		}
	}
}

func (s *Service) reconcileChat(ctx context.Context, id string, conn agentruntime.Conversation) error {
	repo, err := s.conversationRepo()
	if err != nil {
		return err
	}
	sub, err := repo.PendingSubmission(ctx, id)
	if err != nil || sub == nil || sub.Status == domain.SubmissionQueued {
		return err
	}
	reconciler, ok := conn.(agentruntime.ConversationReconciler)
	if !ok {
		return nil
	}
	events, err := reconciler.Reconcile(ctx, agentruntime.ConversationInput{ID: sub.ID, Text: sub.Text, Origin: sub.Origin}, sub.NativeTurnID)
	if err != nil {
		return err
	}
	// The recovered events are native order: an item recorded before one
	// already shown is placed before it.
	items := make([]domain.ConversationItem, 0, len(events))
	for _, e := range events {
		items = append(items, chatItem(*sub, e))
	}
	if err = repo.PutConversationItems(ctx, id, items); err != nil {
		return err
	}
	for _, e := range events {
		if e.Complete {
			// Explicit native completion of the turn is also its acceptance.
			if sub.AcceptedAt == nil {
				now := time.Now().UTC()
				sub.AcceptedAt, sub.Acceptance, sub.ErrorCode = &now, "native_record", ""
			}
			sub.Status = e.Status
			sub.Error = e.Text
			if err = repo.UpdateSubmission(ctx, *sub); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) setChatStatus(ctx context.Context, run domain.SessionRun, status string) error {
	if err := s.repo.UpdateRunAgentStatus(ctx, run.ID, status); err != nil {
		return err
	}
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{SessionID: run.SessionID, RunID: run.ID, Type: "agent_status", Status: status, At: time.Now().UTC()})
}

func (s *Service) failChatRestore(ctx context.Context, id string, cause error) {
	repo, err := s.conversationRepo()
	if err != nil {
		s.logger.Error("chat restore disposition", "error", err)
		return
	}
	reason := "Chat owner could not be restored: " + cause.Error()
	if err = repo.SettleSubmissions(ctx, id, domain.SubmissionUncertain, reason+"; prior delivery/completion remains unconfirmed"); err != nil {
		s.logger.Error("settle interrupted chat", "error", err)
	}
	if err = repo.FailQueuedSubmissions(ctx, id, reason); err != nil {
		s.logger.Error("settle queued chat", "error", err)
	}
}

// isChatSession reports a chat-mode session; archived chats count as chat.
// Only chat queues are held by a blocked/uncertain head.
func (s *Service) isChatSession(ctx context.Context, id string) bool {
	session, err := s.repo.GetSession(ctx, id)
	return err != nil || session.InteractionMode == domain.InteractionChat
}

// archiveChat retains a discovery descriptor for the ended session's observed
// history. Terminal sessions are settled from a final native sync first.
func (s *Service) archiveChat(ctx context.Context, session domain.Session, lifecycle string) error {
	if session.Machine != "" {
		return nil
	}
	terminal := session.InteractionMode != domain.InteractionChat
	repo, err := s.conversationRepo()
	if err != nil {
		if terminal { // no conversation persistence: nothing was observed to retain
			return nil
		}
		return err
	}
	if terminal {
		s.settleTerminalConversation(ctx, session)
	}
	chat := domain.ArchivedChat{ID: session.ID, ArchitectKey: session.ArchitectKey, SessionType: session.SessionType, ContextID: session.ContextID, CreatedAt: session.CreatedAt, EndedAt: time.Now().UTC(), Lifecycle: lifecycle, InteractionMode: session.InteractionMode}
	if run := session.CurrentRun; run != nil {
		chat.ProfileName = run.ProfileName
		if run.ProfileSnapshot != nil {
			chat.Agent = run.ProfileSnapshot.Agent
			chat.Model = run.ProfileSnapshot.Model
		}
	}
	return repo.ArchiveChat(ctx, chat)
}
