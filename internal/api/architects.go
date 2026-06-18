package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

type architectStatusResponse struct {
	Key      string                           `json:"key"`
	Name     string                           `json:"name"`
	Path     string                           `json:"path"`
	Status   *string                          `json:"status"`
	Sessions []architectWorkerSessionResponse `json:"sessions"`
}

type architectWorkerSessionResponse struct {
	ID          string                  `json:"id"`
	Title       string                  `json:"title"`
	Status      domain.SessionRunStatus `json:"status"`
	AgentStatus string                  `json:"agent_status"`
	StartedAt   time.Time               `json:"started_at"`
}

func (h *architectsHandler) list(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]architectResponse{
		"architects": listArchitects(cfg),
	})
}

func (h *architectsHandler) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	key := r.PathValue("key")
	architect, ok := getArchitect(cfg, key, true)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect "+key+" not found", map[string]string{
			"resource": "architect",
			"id":       key,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, architect)
}

func (h *architectsHandler) listStatus(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
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

	statuses, byKey := architectStatusesFromConfig(cfg)
	intents, err := h.sessions.ListIntents(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	for _, intent := range intents {
		if intent.CurrentRun == nil || intent.CurrentRun.Status != domain.SessionRunStatusRunning {
			continue
		}

		architectStatus, ok := byKey[intent.ArchitectKey]
		if !ok {
			writeDomainError(w, r, fmt.Errorf("running session %s references architect %q not found in config", intent.ID, intent.ArchitectKey))
			return
		}

		switch intent.SessionType {
		case domain.SessionTypeArchitect:
			if architectStatus.Status != nil {
				writeDomainError(w, r, fmt.Errorf("multiple running architect sessions found for architect %q", intent.ArchitectKey))
				return
			}
			status := intent.CurrentRun.AgentStatus
			architectStatus.Status = &status
		case domain.SessionTypeTicket, domain.SessionTypeFreeform:
			session, err := h.buildArchitectWorkerSession(r.Context(), architectStatus.Path, intent)
			if err != nil {
				writeDomainError(w, r, err)
				return
			}
			architectStatus.Sessions = append(architectStatus.Sessions, session)
		default:
			writeDomainError(w, r, fmt.Errorf("session %s has unsupported session type %q", intent.ID, intent.SessionType))
			return
		}
	}

	for i := range statuses {
		sort.Slice(statuses[i].Sessions, func(a, b int) bool {
			left := statuses[i].Sessions[a]
			right := statuses[i].Sessions[b]
			if !left.StartedAt.Equal(right.StartedAt) {
				return left.StartedAt.Before(right.StartedAt)
			}
			return left.ID < right.ID
		})
	}

	writeJSON(w, r, http.StatusOK, map[string][]architectStatusResponse{
		"architects": statuses,
	})
}

func architectStatusesFromConfig(cfg config.Config) ([]architectStatusResponse, map[string]*architectStatusResponse) {
	keys := configKeys(cfg.Architects)
	statuses := make([]architectStatusResponse, len(keys))
	byKey := make(map[string]*architectStatusResponse, len(keys))
	for i, key := range keys {
		statuses[i] = architectStatusResponse{
			Key:      key,
			Name:     cfg.Architects[key].Name,
			Path:     cfg.Architects[key].Path,
			Sessions: []architectWorkerSessionResponse{},
		}
		byKey[key] = &statuses[i]
	}
	return statuses, byKey
}

func (h *architectsHandler) buildArchitectWorkerSession(ctx context.Context, architectPath string, intent domain.SessionIntent) (architectWorkerSessionResponse, error) {
	if intent.CurrentRun == nil {
		return architectWorkerSessionResponse{}, fmt.Errorf("running session %s is missing current run", intent.ID)
	}
	if intent.CurrentRun.StartedAt == nil {
		return architectWorkerSessionResponse{}, fmt.Errorf("running session %s current run is missing started_at", intent.ID)
	}

	title, err := h.architectWorkerSessionTitle(ctx, architectPath, intent)
	if err != nil {
		return architectWorkerSessionResponse{}, err
	}

	return architectWorkerSessionResponse{
		ID:          intent.ID,
		Title:       title,
		Status:      intent.CurrentRun.Status,
		AgentStatus: intent.CurrentRun.AgentStatus,
		StartedAt:   intent.CurrentRun.StartedAt.UTC(),
	}, nil
}

func (h *architectsHandler) architectWorkerSessionTitle(ctx context.Context, architectPath string, intent domain.SessionIntent) (string, error) {
	switch intent.SessionType {
	case domain.SessionTypeTicket:
		ticket, err := h.tickets.GetTicket(ctx, architectPath, intent.ContextID)
		if err != nil {
			return "", fmt.Errorf("read ticket title for session %s: %w", intent.ID, err)
		}
		return ticket.Title, nil
	case domain.SessionTypeFreeform:
		return readableFreeformTitle(intent.ContextID)
	default:
		return "", fmt.Errorf("session %s has unsupported session type %q", intent.ID, intent.SessionType)
	}
}

func readableFreeformTitle(contextID string) (string, error) {
	slug, err := freeformContextSlug(contextID)
	if err != nil {
		return "", err
	}
	title := strings.ReplaceAll(slug, "-", " ")
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("freeform context_id %q resolved to an empty title", contextID)
	}
	if title[0] >= 'a' && title[0] <= 'z' {
		title = strings.ToUpper(title[:1]) + title[1:]
	}
	return title, nil
}

func freeformContextSlug(contextID string) (string, error) {
	const prefixLength = len("2006-01-02-1504-")
	if len(contextID) <= prefixLength {
		return "", fmt.Errorf("freeform context_id %q is missing slug content", contextID)
	}
	if contextID[4] != '-' || contextID[7] != '-' || contextID[10] != '-' || contextID[15] != '-' {
		return "", fmt.Errorf("freeform context_id %q does not match expected timestamp-slug format", contextID)
	}
	for _, idx := range []int{0, 1, 2, 3, 5, 6, 8, 9, 11, 12, 13, 14} {
		if contextID[idx] < '0' || contextID[idx] > '9' {
			return "", fmt.Errorf("freeform context_id %q does not match expected timestamp-slug format", contextID)
		}
	}
	slug := strings.TrimSpace(contextID[prefixLength:])
	if slug == "" {
		return "", fmt.Errorf("freeform context_id %q is missing slug content", contextID)
	}
	return slug, nil
}

func (h *architectsHandler) listConclusions(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	summaries, err := h.sessions.ListConclusions(r.Context(), r.PathValue("key"), limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string][]domain.ConclusionSummary{
		"conclusions": summaries,
	})
}

func (h *architectsHandler) readRecentConclusion(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	conclusion, err := h.sessions.ReadRecentConclusion(r.Context(), r.PathValue("key"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, conclusion)
}

func (h *architectsHandler) readConclusion(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	conclusion, err := h.sessions.ReadConclusion(r.Context(), r.PathValue("key"), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, conclusion)
}
