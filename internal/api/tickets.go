package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

func (h *ticketsHandler) list(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectPath, ok := getArchitectPath(cfg, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}

	board, err := h.tickets.ListTickets(r.Context(), architectPath)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	statusParam := strings.TrimSpace(r.URL.Query().Get("status"))
	if statusParam == "" {
		writeJSON(w, r, http.StatusOK, board)
		return
	}

	status := domain.TicketStatus(statusParam)
	if !status.Valid() {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid status: "+statusParam, map[string]string{
			"field": "status",
		})
		return
	}

	limit := 10
	if limitParam := strings.TrimSpace(r.URL.Query().Get("limit")); limitParam != "" {
		parsed, err := strconv.Atoi(limitParam)
		if err != nil || parsed < 1 {
			writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid limit: "+limitParam, map[string]string{
				"field": "limit",
			})
			return
		}
		limit = parsed
	}

	var filtered []domain.TicketSummary
	switch status {
	case domain.TicketStatusBacklog:
		filtered = board.Backlog
	case domain.TicketStatusProgress:
		filtered = board.Progress
	case domain.TicketStatusDone:
		filtered = board.Done
	}

	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	if filtered == nil {
		filtered = []domain.TicketSummary{}
	}
	writeJSON(w, r, http.StatusOK, filtered)
}

func (h *ticketsHandler) get(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "ticket service not configured", nil)
		return
	}

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectPath, ok := getArchitectPath(cfg, r.PathValue("key"))
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

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectKey := r.PathValue("key")
	architectPath, ok := getArchitectPath(cfg, architectKey)
	if !ok {
		writeArchitectNotFound(w, r, architectKey)
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

	if err := validateConfiguredRepo(cfg, architectKey, request.Repo); err != nil {
		writeDomainError(w, r, err)
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
		h.publishArchitect(architectKey, domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: architectKey,
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

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectPath, ok := getArchitectPath(cfg, r.PathValue("key"))
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

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectKey := r.PathValue("key")
	architectPath, ok := getArchitectPath(cfg, architectKey)
	if !ok {
		writeArchitectNotFound(w, r, architectKey)
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

	if request.Repo != nil {
		if err := validateConfiguredRepo(cfg, architectKey, *request.Repo); err != nil {
			writeDomainError(w, r, err)
			return
		}
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

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectPath, ok := getArchitectPath(cfg, r.PathValue("key"))
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

	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	architectPath, ok := getArchitectPath(cfg, r.PathValue("key"))
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

func (h *ticketsHandler) moveToDone(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	var input struct {
		Body            string             `json:"body"`
		Commits         []domain.CommitRef `json:"commits,omitempty"`
		Rejected        bool               `json:"rejected,omitempty"`
		RejectionReason string             `json:"rejection_reason,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}

	architectKey := r.PathValue("key")
	result, err := h.sessions.MoveTicketToDone(r.Context(), architectKey, r.PathValue("id"), domain.MoveTicketToDoneParams{
		Body:            input.Body,
		Commits:         input.Commits,
		Rejected:        input.Rejected,
		RejectionReason: input.RejectionReason,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if h.publishArchitect != nil {
		h.publishArchitect(architectKey, domain.ArchitectEvent{
			Type:         "workspace_changed",
			ArchitectKey: architectKey,
			Reason:       "ticket_concluded",
			TicketID:     result.TicketID,
			At:           time.Now().UTC(),
		})
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"success":   true,
		"ticket_id": result.TicketID,
	})
}

func validateConfiguredRepo(cfg config.Config, architectKey, repoKey string) error {
	repoKey = strings.TrimSpace(repoKey)
	if repoKey == "" {
		return nil
	}

	architect, ok := cfg.Architects[architectKey]
	if !ok {
		return &domain.NotFoundError{Resource: "architect", ID: architectKey}
	}
	if _, ok := architect.Repos[repoKey]; !ok {
		return &domain.ValidationError{Field: "repo", Message: fmt.Sprintf("repo key '%s' not found in architect config", repoKey)}
	}
	return nil
}

func writeArchitectNotFound(w http.ResponseWriter, r *http.Request, key string) {
	writeError(w, r, http.StatusNotFound, string(domain.ErrCodeNotFound), "architect "+key+" not found", map[string]string{
		"resource": "architect",
		"id":       key,
	})
}
