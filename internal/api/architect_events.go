package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/hiveryn/daemon/internal/archevents"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

type architectEventsHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
	hub          *archevents.Hub
}

// publishArchitectEvent stamps and fans out one architect event. Every handler
// that mutates architect-visible state goes through here so the wire shape is
// built in exactly one place — adding a field to domain.ArchitectEvent must not
// mean hand-editing a dozen struct literals again.
//
// ticketID is empty for reasons that are not about a ticket; sessionID is empty
// for reasons that are not about a session.
func publishArchitectEvent(
	publish func(string, domain.ArchitectEvent),
	architectKey string,
	reason domain.ArchitectEventReason,
	ticketID string,
	sessionID string,
) {
	if publish == nil || architectKey == "" {
		return
	}
	publish(architectKey, domain.ArchitectEvent{
		Type:         domain.ArchitectEventType,
		ArchitectKey: architectKey,
		Reason:       reason,
		TicketID:     ticketID,
		SessionID:    sessionID,
		At:           time.Now().UTC(),
	})
}

func (h *architectEventsHandler) events(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "architect events not configured", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "streaming not supported", nil)
		return
	}

	key := r.PathValue("key")
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if _, ok := getArchitect(cfg, key, false); !ok {
		writeArchitectNotFound(w, r, key)
		return
	}

	sub := h.hub.Subscribe(key)
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if _, err := fmt.Fprint(w, ":\n\n"); err != nil {
		return
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
			data, err := json.Marshal(event)
			if err != nil {
				h.logger.Error("[sse] marshal architect event error", "error", err)
				return
			}
			if err := writeSSEEventData(w, data); err != nil {
				h.logger.Warn("[sse] architect event write error", "error", err)
				return
			}
			flusher.Flush()
			h.logger.Debug("[sse] architect event delivered",
				"key", key,
				"reason", event.Reason,
				"ticket_id", event.TicketID,
			)
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				h.logger.Warn("[sse] keepalive write error", "error", err)
				return
			}
			flusher.Flush()
		}
	}
}
