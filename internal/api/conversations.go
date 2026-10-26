package api

import (
	"net/http"
	"strconv"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *sessionsHandler) conversationService(w http.ResponseWriter, r *http.Request) domain.ConversationService {
	service, ok := h.sessions.(domain.ConversationService)
	if !ok {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "conversation service not configured", nil)
		return nil
	}
	return service
}
func (h *sessionsHandler) submitMessage(w http.ResponseWriter, r *http.Request) {
	service := h.conversationService(w, r)
	if service == nil {
		return
	}
	var req domain.SubmissionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, 400, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}
	receipt, err := service.SubmitMessage(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, receipt)
}
func (h *sessionsHandler) getSubmission(w http.ResponseWriter, r *http.Request) {
	service := h.conversationService(w, r)
	if service == nil {
		return
	}
	receipt, err := service.GetSubmission(r.Context(), r.PathValue("id"), r.PathValue("submissionID"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, 200, receipt)
}
func (h *sessionsHandler) conversation(w http.ResponseWriter, r *http.Request) {
	service := h.conversationService(w, r)
	if service == nil {
		return
	}
	q := domain.ConversationQuery{}
	for key, target := range map[string]*int64{"after_revision": &q.AfterRevision, "snapshot_revision": &q.SnapshotRevision} {
		if raw := r.URL.Query().Get(key); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeDomainError(w, r, &domain.ValidationError{Field: key, Message: err.Error()})
				return
			}
			*target = v
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			writeDomainError(w, r, &domain.ValidationError{Field: "limit", Message: err.Error()})
			return
		}
		q.Limit = v
	}
	page, err := service.ReadConversation(r.Context(), r.PathValue("id"), q)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, 200, page)
}

// Ordinary polling view of the same durable question/intent events the desktop
// reduces. Resolved entries remove required entries; chat text never answers.
func (h *sessionsHandler) conversationControls(w http.ResponseWriter, r *http.Request) {
	events, err := h.sessions.ListSessionEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	controls := []domain.SessionEvent{}
	for _, e := range events {
		if e.Type == "intent" || e.Type == "question" {
			controls = append(controls, e)
		}
	}
	writeJSON(w, r, 200, map[string]any{"events": controls})
}
