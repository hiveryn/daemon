package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hiveryn/daemon/internal/domain"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (h *sessionsHandler) createSession(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	var input domain.CreateSessionRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	session, err := h.sessions.CreateSession(r.Context(), input)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, session)
}

func (h *sessionsHandler) list(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	sessions, err := h.sessions.ListSessions(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string][]domain.Session{
		"sessions": sessions,
	})
}

func (h *sessionsHandler) get(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	session, err := h.sessions.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, session)
}

func (h *sessionsHandler) getTicket(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	session, err := h.sessions.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if session.SessionType != domain.SessionTypeTicket {
		writeDomainError(w, r, &domain.NotFoundError{
			Resource: "ticket",
			ID:       r.PathValue("id"),
		})
		return
	}

	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectPath, ok := getArchitectPath(cfg, session.ArchitectKey)
	if !ok {
		writeArchitectNotFound(w, r, session.ArchitectKey)
		return
	}

	ticket, err := h.tickets.GetTicket(r.Context(), architectPath, session.ContextID)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, ticket)
}

func (h *sessionsHandler) createRun(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	var input domain.CreateSessionRunRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	result, err := h.sessions.CreateRun(r.Context(), r.PathValue("id"), input)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	// The ticket_moved and session_started events for a new run are published by
	// SessionService.CreateRun itself, so the architect-MCP spawn path — which
	// never touches this handler — emits exactly the same events as this one.

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"run":              result.Run,
		"main_terminal_id": result.MainTerminalID,
		"ws_url":           websocketURL(r, "/ws/session/"+r.PathValue("id")+"/terminal/"+result.MainTerminalID),
	})
}

func (h *sessionsHandler) conclude(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	var input struct {
		Body            string             `json:"body"`
		Commits         []domain.CommitRef `json:"commits,omitempty"`
		Outcome         string             `json:"outcome,omitempty"`
		RejectionReason string             `json:"rejection_reason,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	result, err := h.sessions.ConcludeSession(r.Context(), r.PathValue("id"), domain.ConcludeSessionParams{
		Body:            input.Body,
		Commits:         input.Commits,
		Outcome:         domain.TicketOutcome(input.Outcome),
		RejectionReason: input.RejectionReason,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if result.TicketID != "" {
		publishArchitectEvent(h.publishArchitect, result.ArchitectKey, domain.ArchitectEventTicketConcluded, result.TicketID, result.SessionID)
	}
	// Session end is announced regardless of ticket scope (architect and freeform
	// sessions have no ticket) so a window that missed the session-scoped ended
	// event still drops the tab on its next reconcile.
	publishArchitectEvent(h.publishArchitect, result.ArchitectKey, domain.ArchitectEventSessionEnded, result.TicketID, result.SessionID)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"success":    true,
		"session_id": result.SessionID,
		"ticket_id":  result.TicketID,
	})
}

func (h *sessionsHandler) discard(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	result, err := h.sessions.UnspawnTicketSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if result.TicketID != "" {
		publishArchitectEvent(h.publishArchitect, result.ArchitectKey, domain.ArchitectEventTicketMoved, result.TicketID, result.SessionID)
	}
	publishArchitectEvent(h.publishArchitect, result.ArchitectKey, domain.ArchitectEventSessionEnded, result.TicketID, result.SessionID)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"success":    true,
		"session_id": result.SessionID,
		"ticket_id":  result.TicketID,
	})
}

func (h *sessionsHandler) events(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "streaming not supported", nil)
		return
	}

	sessionID := r.PathValue("id")
	backlog, err := h.sessions.ListSessionEvents(r.Context(), sessionID)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	sub, err := h.sessions.SubscribeSessionEvents(r.Context(), sessionID)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for _, event := range backlog {
		if err := writeSSEEvent(w, event); err != nil {
			h.logger.Warn("[sse] backlog write error", "session_id", sessionID, "error", err)
			return
		}
	}
	flusher.Flush()

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-sub.C():
			if !ok {
				return
			}
			if err := writeSSEEvent(w, event); err != nil {
				h.logger.Warn("[sse] live event write error", "session_id", sessionID, "error", err)
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				h.logger.Warn("[sse] keepalive write error", "session_id", sessionID, "error", err)
				return
			}
			flusher.Flush()
		}
	}
}

// parseAttachSize reads the optional cols/rows query params of a terminal
// attach. Returns (0, 0, nil) when absent. Present-but-invalid values are a
// client bug and produce an error so the attach fails loudly.
func parseAttachSize(r *http.Request) (cols, rows uint16, err error) {
	colsStr := r.URL.Query().Get("cols")
	rowsStr := r.URL.Query().Get("rows")
	if colsStr == "" && rowsStr == "" {
		return 0, 0, nil
	}
	c, errC := strconv.ParseUint(colsStr, 10, 16)
	rw, errR := strconv.ParseUint(rowsStr, 10, 16)
	if errC != nil || errR != nil || c == 0 || rw == 0 {
		return 0, 0, fmt.Errorf("invalid cols/rows query params: cols=%q rows=%q", colsStr, rowsStr)
	}
	return uint16(c), uint16(rw), nil
}

func (h *sessionsHandler) wsTerminal(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	sessionID := r.PathValue("id")
	terminalID := r.PathValue("uuid")

	cols, rows, err := parseAttachSize(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}

	attachment, err := h.sessions.AttachTerminal(r.Context(), sessionID, terminalID)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	defer func() { _ = attachment.Close() }()

	// Attach-time size handshake: bring the PTY to the client's grid BEFORE
	// streaming starts. Size sync used to be purely edge-triggered (resize
	// messages on grid changes), so any missed edge — daemon restart restoring
	// PTYs at the 80×24 default, a resize sent while the WS was down, a client
	// that fit while the pane was in a transient layout — left the PTY and the
	// client grid divergent forever, with TUIs drawing frames for the wrong
	// size. Applying the client's size at attach makes every (re)connect a
	// reconciliation point: the SIGWINCH repaint lands right after the replay
	// buffer in the PTY stream, correcting any stale-size frame.
	if cols > 0 && rows > 0 {
		if err := attachment.Resize(cols, rows); err != nil {
			h.logger.Warn("[ws] attach resize error (ignored)", "session_id", sessionID, "terminal_id", terminalID, "cols", cols, "rows", rows, "error", err)
		} else {
			h.logger.Info("[ws] attach resize", "session_id", sessionID, "terminal_id", terminalID, "cols", cols, "rows", rows)
		}
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	errCh := make(chan error, 2)
	go func() {
		for chunk := range attachment.Output() {
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()

	h.logger.Info("[ws] attached", "session_id", sessionID, "terminal_id", terminalID)
	go func() {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				h.logger.Info("[ws] read goroutine exit", "session_id", sessionID, "terminal_id", terminalID, "error", err)
				errCh <- err
				return
			}
			if messageType != websocket.TextMessage {
				continue
			}

			var resize struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if err := json.Unmarshal(payload, &resize); err == nil && resize.Type == "resize" {
				h.logger.Info("[ws] resize", "session_id", sessionID, "terminal_id", terminalID, "cols", resize.Cols, "rows", resize.Rows)
				if resize.Cols > 0 && resize.Rows > 0 {
					if err := attachment.Resize(resize.Cols, resize.Rows); err != nil {
						h.logger.Warn("[ws] resize error (ignored)", "session_id", sessionID, "terminal_id", terminalID, "error", err)
					}
				}
				continue
			}

			if err := attachment.Write(payload); err != nil {
				h.logger.Info("[ws] write error", "session_id", sessionID, "terminal_id", terminalID, "error", err)
				errCh <- err
				return
			}
		}
	}()

	select {
	case <-r.Context().Done():
	case <-errCh:
	}
}

func (h *sessionsHandler) listTabs(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	tabs, err := h.sessions.ListSessionTabs(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if tabs == nil {
		tabs = []domain.SessionTab{}
	}

	writeJSON(w, r, http.StatusOK, tabs)
}

func (h *sessionsHandler) createTerminal(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	var input domain.CreateTerminalParams
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	terminal, err := h.sessions.CreateTerminal(r.Context(), r.PathValue("id"), input)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, terminal)
}

func (h *sessionsHandler) listTerminalWorkdirs(w http.ResponseWriter, r *http.Request) {
	workdirs, err := h.sessions.ListTerminalWorkdirs(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, workdirs)
}

func (h *sessionsHandler) listTerminals(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	terminals, err := h.sessions.ListTerminals(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if terminals == nil {
		terminals = []domain.TerminalInfo{}
	}

	writeJSON(w, r, http.StatusOK, map[string][]domain.TerminalInfo{
		"terminals": terminals,
	})
}

func (h *sessionsHandler) killTerminal(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	if err := h.sessions.KillTerminal(r.Context(), r.PathValue("id"), r.PathValue("uuid")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeJSON(r *http.Request, dst any) error {
	defer func() {
		if r.Body != nil {
			_ = r.Body.Close()
		}
	}()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func websocketURL(r *http.Request, path string) string {
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	return scheme + "://" + r.Host + path
}

func writeSSEEvent(w http.ResponseWriter, event domain.SessionEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return writeSSEEventData(w, data)
}
