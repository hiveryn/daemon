package api

import (
	"fmt"
	"net/http"
	"sort"
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
		Title           string   `json:"title"`
		Repo            string   `json:"repo"`
		AdditionalRepos []string `json:"additional_repos"`
		Body            string   `json:"body,omitempty"`
		References      []string `json:"references,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}

	request.AdditionalRepos, err = validateRepoScope(cfg, architectKey, request.Repo, request.AdditionalRepos)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	ticket, err := h.tickets.CreateTicket(r.Context(), architectPath, domain.CreateTicketParams{
		Title:           request.Title,
		Repo:            request.Repo,
		AdditionalRepos: request.AdditionalRepos,
		Body:            request.Body,
		References:      request.References,
		Now:             time.Now().UTC(),
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	publishArchitectEvent(h.publishArchitect, architectKey, domain.ArchitectEventTicketCreated, ticket.ID, "")
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
	publishArchitectEvent(h.publishArchitect, r.PathValue("key"), domain.ArchitectEventTicketUpdated, ticket.ID, "")
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
		Title           *string   `json:"title,omitempty"`
		Repo            *string   `json:"repo,omitempty"`
		AdditionalRepos *[]string `json:"additional_repos,omitempty"`
		References      *[]string `json:"references,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}

	current, err := h.tickets.GetTicket(r.Context(), architectPath, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	repo := current.Repo
	additional := current.AdditionalRepos
	if request.Repo != nil {
		repo = *request.Repo
	}
	if request.AdditionalRepos != nil {
		additional = *request.AdditionalRepos
	}
	normalized, err := validateRepoScope(cfg, architectKey, repo, additional)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if request.AdditionalRepos != nil {
		request.AdditionalRepos = &normalized
	}

	ticket, err := h.tickets.UpdateTicketMetadata(r.Context(), architectPath, r.PathValue("id"), domain.UpdateTicketMetadataParams{
		Title:           request.Title,
		Repo:            request.Repo,
		AdditionalRepos: request.AdditionalRepos,
		References:      request.References,
		Now:             time.Now().UTC(),
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	publishArchitectEvent(h.publishArchitect, r.PathValue("key"), domain.ArchitectEventTicketUpdated, ticket.ID, "")
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
	publishArchitectEvent(h.publishArchitect, r.PathValue("key"), domain.ArchitectEventTicketDeleted, r.PathValue("id"), "")
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
	publishArchitectEvent(h.publishArchitect, r.PathValue("key"), domain.ArchitectEventTicketMoved, ticket.ID, "")
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
		Outcome         string             `json:"outcome,omitempty"`
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
		Outcome:         domain.TicketOutcome(input.Outcome),
		RejectionReason: input.RejectionReason,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	publishArchitectEvent(h.publishArchitect, architectKey, domain.ArchitectEventTicketConcluded, result.TicketID, "")

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

func validateRepoScope(cfg config.Config, architectKey, primary string, additional []string) ([]string, error) {
	primary = strings.TrimSpace(primary)
	if primary == "" {
		return nil, &domain.ValidationError{Field: "repo", Message: "is required"}
	}
	if err := validateConfiguredRepo(cfg, architectKey, primary); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{primary: {}}
	normalized := make([]string, 0, len(additional))
	for _, raw := range additional {
		key := strings.TrimSpace(raw)
		if key == "" {
			return nil, &domain.ValidationError{Field: "additional_repos", Message: "repo keys cannot be blank"}
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q is duplicated or overlaps primary repo", key)}
		}
		if err := validateConfiguredRepo(cfg, architectKey, key); err != nil {
			return nil, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q not found in architect config", key)}
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func writeArchitectNotFound(w http.ResponseWriter, r *http.Request, key string) {
	writeError(w, r, http.StatusNotFound, string(domain.ErrCodeNotFound), "architect "+key+" not found", map[string]string{
		"resource": "architect",
		"id":       key,
	})
}
