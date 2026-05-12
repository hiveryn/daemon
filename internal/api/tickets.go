package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *ticketsHandler) list(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	architectPath, ok := getArchitectPath(h.config, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	board, err := h.tickets.ListTickets(r.Context(), architectPath)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, board)
}

func (h *ticketsHandler) get(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	architectPath, ok := getArchitectPath(h.config, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	ticket, err := h.tickets.GetTicket(r.Context(), architectPath, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, ticket)
}

func (h *ticketsHandler) create(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	architectPath, ok := getArchitectPath(h.config, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	var request struct {
		Title      string   `json:"title"`
		Repo       string   `json:"repo,omitempty"`
		Body       string   `json:"body,omitempty"`
		References []string `json:"references,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}

	ticket, err := h.tickets.CreateTicket(r.Context(), architectPath, domain.CreateTicketParams{
		Title:      request.Title,
		Repo:       request.Repo,
		Body:       request.Body,
		References: request.References,
		Now:        time.Now().UTC(),
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if h.publishArchitect != nil {
		h.publishArchitect(r.PathValue("key"), domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: r.PathValue("key"),
			Reason:       "ticket_created",
			TicketID:     ticket.ID,
			At:           time.Now().UTC(),
		})
	}
	writeJSON(w, r, http.StatusCreated, ticket)
}

func (h *ticketsHandler) edit(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	architectPath, ok := getArchitectPath(h.config, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	var request struct {
		OldString  string `json:"oldString"`
		NewString  string `json:"newString"`
		ReplaceAll bool   `json:"replaceAll,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}

	ticket, err := h.tickets.EditTicket(r.Context(), architectPath, r.PathValue("id"), domain.EditTicketParams{
		OldString:  request.OldString,
		NewString:  request.NewString,
		ReplaceAll: request.ReplaceAll,
		Now:        time.Now().UTC(),
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if h.publishArchitect != nil {
		h.publishArchitect(r.PathValue("key"), domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: r.PathValue("key"),
			Reason:       "ticket_updated",
			TicketID:     ticket.ID,
			At:           time.Now().UTC(),
		})
	}
	writeJSON(w, r, http.StatusOK, ticket)
}

func (h *ticketsHandler) updateMetadata(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	architectPath, ok := getArchitectPath(h.config, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	var request struct {
		Title      *string   `json:"title,omitempty"`
		Repo       *string   `json:"repo,omitempty"`
		References *[]string `json:"references,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}

	ticket, err := h.tickets.UpdateTicketMetadata(r.Context(), architectPath, r.PathValue("id"), domain.UpdateTicketMetadataParams{
		Title:      request.Title,
		Repo:       request.Repo,
		References: request.References,
		Now:        time.Now().UTC(),
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if h.publishArchitect != nil {
		h.publishArchitect(r.PathValue("key"), domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: r.PathValue("key"),
			Reason:       "ticket_updated",
			TicketID:     ticket.ID,
			At:           time.Now().UTC(),
		})
	}
	writeJSON(w, r, http.StatusOK, ticket)
}

func (h *ticketsHandler) delete(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	architectPath, ok := getArchitectPath(h.config, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	if err := h.tickets.DeleteTicket(r.Context(), architectPath, r.PathValue("id")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	if h.publishArchitect != nil {
		h.publishArchitect(r.PathValue("key"), domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: r.PathValue("key"),
			Reason:       "ticket_deleted",
			TicketID:     r.PathValue("id"),
			At:           time.Now().UTC(),
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]bool{"deleted": true})
}

func (h *ticketsHandler) move(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	architectPath, ok := getArchitectPath(h.config, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	to := domain.TicketStatus(strings.TrimSpace(r.URL.Query().Get("to")))
	ticket, err := h.tickets.MoveTicket(r.Context(), architectPath, r.PathValue("id"), domain.MoveTicketParams{To: to})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if h.publishArchitect != nil {
		h.publishArchitect(r.PathValue("key"), domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: r.PathValue("key"),
			Reason:       "ticket_moved",
			TicketID:     ticket.ID,
			At:           time.Now().UTC(),
		})
	}
	writeJSON(w, r, http.StatusOK, ticket)
}

func writeArchitectNotFound(w http.ResponseWriter, r *http.Request, key string) {
	writeError(w, r, http.StatusNotFound, string(domain.ErrCodeNotFound), "architect "+key+" not found", map[string]string{
		"resource": "architect",
		"id":       key,
	})
}

func (h *ticketsHandler) spawn(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	key := r.PathValue("key")
	ticketID := r.PathValue("id")

	var request struct {
		ProfileName string `json:"profile_name"`
		Mode        string `json:"mode,omitempty"`
		Cols        uint16 `json:"cols,omitempty"`
		Rows        uint16 `json:"rows,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}

	h.logger.Info("[worker-spawn] request",
		"architect_key", key,
		"ticket_id", ticketID,
		"profile_name", request.ProfileName,
		"mode", request.Mode,
	)

	result, err := h.sessions.SpawnWorkSession(r.Context(), domain.SpawnWorkSessionRequest{
		ArchitectKey: key,
		TicketID:     ticketID,
		ProfileName:  request.ProfileName,
		Mode:         request.Mode,
		Cols:         request.Cols,
		Rows:         request.Rows,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if h.publishArchitect != nil {
		h.publishArchitect(key, domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: key,
			Reason:       "ticket_moved",
			TicketID:     ticketID,
			At:           time.Now().UTC(),
		})
	}

	writeJSON(w, r, http.StatusOK, map[string]string{
		"session_id": result.Session.ID,
		"ws_url":     websocketURL(r, "/ws/session/"+result.Session.ID),
	})
}
