package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

var (
	// streamHeartbeat keeps intermediaries open and lets the client detect a
	// dead connection (it reconnects after three missed heartbeats).
	streamHeartbeat = 15 * time.Second
	// streamWriteBound bounds one event write: a stalled reader is cut off and
	// reconnects from its cursor instead of holding the handler.
	streamWriteBound = 10 * time.Second
)

// streamPageLimit is the conversation page size of one "conversation" event.
const streamPageLimit = 200

// conversationStream serves GET /api/sessions/{id}/stream: one SSE
// connection carrying a session's conversation revisions, control snapshots
// and status snapshots (docs/conversation-api.md, "Stream"). Every event is
// re-read from its durable source when the subscription signals a change, so
// nothing is buffered per client and nothing is lost to a slow reader.
func (h *sessionsHandler) conversationStream(w http.ResponseWriter, r *http.Request) {
	service := h.conversationService(w, r)
	if service == nil {
		return
	}
	id := r.PathValue("id")
	cursor, err := streamCursor(r)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	watch, err := service.WatchConversation(r.Context(), id)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	defer watch.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &conversationStreamWriter{w: w, rc: http.NewResponseController(w), h: h, service: service, id: id, cursor: cursor, controls: -1}
	// The client may resume quickly after a disconnect; tell it how long to wait.
	if err := s.write("retry: 1000\n\n"); err != nil {
		return
	}

	heartbeat := time.NewTicker(streamHeartbeat)
	defer heartbeat.Stop()
	for {
		ended, err := s.sync(r, watch)
		if err != nil {
			h.logger.Warn("[sse] conversation stream ended", "session_id", id, "error", err)
			return
		}
		if ended {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-watch.Done():
			return
		case <-watch.Changed():
		case <-heartbeat.C:
			if err := s.write(": heartbeat\n\n"); err != nil {
				return
			}
		}
	}
}

// streamCursor is the last conversation revision the client processed: the
// Last-Event-ID header of a reconnect, else after_revision, else 0.
func streamCursor(r *http.Request) (int64, error) {
	raw := r.Header.Get("Last-Event-ID")
	field := "Last-Event-ID"
	if raw == "" {
		raw, field = r.URL.Query().Get("after_revision"), "after_revision"
	}
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, &domain.ValidationError{Field: field, Message: "must be a nonnegative conversation revision"}
	}
	return v, nil
}

type conversationStreamWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	h       *sessionsHandler
	service domain.ConversationService
	id      string
	cursor  int64
	// replayed is set once the first conversation page (possibly empty) was
	// sent, which tells the client its replay is complete.
	replayed bool
	// status and controls are the last snapshots sent; -1 forces the first.
	status   string
	controls int
}

func (s *conversationStreamWriter) write(frame string) error {
	if err := s.rc.SetWriteDeadline(time.Now().Add(streamWriteBound)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *conversationStreamWriter) event(name string, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", name, err)
	}
	frame := "event: " + name + "\n"
	if id != "" {
		frame += "id: " + id + "\n"
	}
	return s.write(frame + "data: " + string(data) + "\n\n")
}

// sync sends whatever changed: status, controls, then every conversation
// revision after the cursor. For an ended session the final revisions precede
// the ended status, after which the stream closes (ended=true).
func (s *conversationStreamWriter) sync(r *http.Request, watch domain.ConversationWatch) (ended bool, err error) {
	ctx := r.Context()
	session, err := s.h.sessions.GetSession(ctx, s.id)
	var missing *domain.NotFoundError
	if err != nil && !errors.As(err, &missing) {
		return false, err
	}
	live := err == nil
	if live {
		if err := s.sendStatus(domain.ConversationStreamStatus{State: domain.StreamStateLive, Session: streamSession(session), HistoryError: streamError(watch.HistoryError())}); err != nil {
			return false, err
		}
		events, err := s.h.sessions.ListSessionEvents(ctx, s.id)
		if err != nil && !errors.As(err, &missing) {
			return false, err
		}
		controls := []domain.SessionEvent{}
		for _, e := range events {
			if e.Type == "intent" || e.Type == "question" {
				controls = append(controls, e)
			}
		}
		// Control events are append-only, so their count identifies the snapshot.
		if len(controls) != s.controls {
			if err := s.event(domain.StreamEventControls, "", domain.ConversationStreamControls{Events: controls}); err != nil {
				return false, err
			}
			s.controls = len(controls)
		}
	}
	if err := s.sendConversation(r); err != nil {
		return false, err
	}
	if !live {
		// Nothing is left to answer or send; the final history was sent above.
		if err := s.sendStatus(domain.ConversationStreamStatus{State: domain.StreamStateEnded}); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (s *conversationStreamWriter) sendStatus(status domain.ConversationStreamStatus) error {
	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status event: %w", err)
	}
	if string(data) == s.status {
		return nil
	}
	if err := s.write("event: " + domain.StreamEventStatus + "\ndata: " + string(data) + "\n\n"); err != nil {
		return err
	}
	s.status = string(data)
	return nil
}

// sendConversation pages every stored revision after the cursor. Each page's
// next_revision is its SSE id: a client resuming from it never misses or
// needs an earlier revision. A cursor beyond this conversation's revisions
// (another daemon database, a corrupted client) is reset to 0.
func (s *conversationStreamWriter) sendConversation(r *http.Request) error {
	for {
		page, err := s.service.ReadConversationFeed(r.Context(), s.id, domain.ConversationQuery{AfterRevision: s.cursor, Limit: streamPageLimit})
		var invalid *domain.ValidationError
		if errors.As(err, &invalid) && s.cursor > 0 {
			if err := s.event(domain.StreamEventReset, "", domain.ConversationStreamReset{Reason: fmt.Sprintf("revision %d is not part of this conversation; replaying from the start", s.cursor)}); err != nil {
				return err
			}
			s.cursor = 0
			continue
		}
		if err != nil {
			return fmt.Errorf("read conversation feed: %w", err)
		}
		if len(page.Items) > 0 || !s.replayed {
			if err := s.event(domain.StreamEventConversation, strconv.FormatInt(page.NextRevision, 10), page); err != nil {
				return err
			}
			s.cursor = page.NextRevision
		}
		if !page.HasMore {
			s.replayed = true
			return nil
		}
	}
}

func streamError(err error) *domain.ConversationStreamError {
	if err == nil {
		return nil
	}
	var conv *domain.ConversationError
	if errors.As(err, &conv) {
		return &domain.ConversationStreamError{Code: conv.Code, Message: conv.Message}
	}
	return &domain.ConversationStreamError{Code: domain.HistoryReadFailed, Message: err.Error()}
}

// streamSession is the session record a status event carries: without the
// kickoff prompt, the role instructions and the variant environment, args and
// MCP configuration (the conversation feed shows the recorded kickoff).
func streamSession(session domain.Session) *domain.Session {
	out := session
	out.Prompt, out.Instructions = "", ""
	if run := session.CurrentRun; run != nil {
		r := *run
		if snap := run.ProfileSnapshot; snap != nil {
			p := *snap
			p.Env, p.Args, p.MCP = nil, nil, nil
			r.ProfileSnapshot = &p
		}
		out.CurrentRun = &r
	}
	return &out
}
