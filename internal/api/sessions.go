package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hiveryn/daemon/internal/domain"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (h *sessionsHandler) list(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	filter := domain.SessionListFilter{}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		filter.Status = domain.SessionStatus(status)
	}

	sessions, err := h.sessions.ListSessions(r.Context(), filter)
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

func (h *sessionsHandler) delete(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	if err := h.sessions.TerminateSession(r.Context(), r.PathValue("id")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *sessionsHandler) ws(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	attachment, err := h.sessions.AttachTerminal(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	defer func() { _ = attachment.Close() }()

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	errCh := make(chan error, 2)
	go func() {
		// PTY output is a raw byte stream that may include incomplete UTF-8 sequences
		// (multi-byte chars split across read chunks) or non-text control bytes.
		// Use BinaryMessage; TextMessage requires valid UTF-8 per RFC 6455 and the
		// peer will close with 1007 on the first split codepoint.
		for chunk := range attachment.Output() {
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()

	sessionID := r.PathValue("id")
	h.logger.Info("[ws] attached", "session_id", sessionID)
	go func() {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				h.logger.Info("[ws] read goroutine exit", "session_id", sessionID, "err", err)
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
				h.logger.Info("[ws] resize", "session_id", sessionID, "cols", resize.Cols, "rows", resize.Rows)
				if resize.Cols > 0 && resize.Rows > 0 {
					// A transient resize failure (e.g. EBADF during teardown) must NOT tear down
					// the whole WebSocket. Log and continue — input must keep flowing.
					if err := attachment.Resize(resize.Cols, resize.Rows); err != nil {
						h.logger.Warn("[ws] resize error (ignored)", "session_id", sessionID, "err", err)
					}
				}
				continue
			}

			if err := attachment.Write(payload); err != nil {
				h.logger.Info("[ws] write error", "session_id", sessionID, "err", err)
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
