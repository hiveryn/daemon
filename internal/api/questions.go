package api

import (
	"log/slog"
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

// Agent notifications and questions. notify and the question request are
// agent-facing, addressed by the calling session; the question request BLOCKS
// until the question resolves (at most domain.QuestionTimeout). The answer
// route is the desktop's, addressed by session and question id.
type questionsHandler struct {
	logger    *slog.Logger
	questions domain.QuestionService
}

func (h *questionsHandler) ready(w http.ResponseWriter, r *http.Request) bool {
	if h.questions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "question service not configured", nil)
		return false
	}
	return true
}

func (h *questionsHandler) notify(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	var input domain.NotifyRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}
	res, err := h.questions.Notify(r.Context(), r.PathValue("id"), input.Message)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (h *questionsHandler) ask(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	var input domain.AskQuestionRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}
	res, err := h.questions.AskQuestion(r.Context(), r.PathValue("id"), input)
	if err != nil {
		if r.Context().Err() != nil {
			// The caller is gone; the question was resolved as cancelled.
			if h.logger != nil {
				h.logger.Info("question request ended by caller", "session_id", r.PathValue("id"), "error", err)
			}
			return
		}
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (h *questionsHandler) answer(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	var input domain.AnswerQuestionRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}
	question, err := h.questions.AnswerQuestion(r.Context(), r.PathValue("id"), r.PathValue("questionID"), input.Answer)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, question)
}
