package api

import (
	"encoding/json"
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

// referenceMigrationHandler serves `hiverynd references-migrate`: a read-only
// plan over the registered architects and the apply of a reviewed plan.
type referenceMigrationHandler struct {
	migrator domain.ReferenceMigrator
}

func (h *referenceMigrationHandler) available(w http.ResponseWriter, r *http.Request) bool {
	if h.migrator == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "reference migration not configured", nil)
		return false
	}
	return true
}

func (h *referenceMigrationHandler) plan(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	var request struct {
		Architects []string                        `json:"architects,omitempty"`
		Rules      []domain.ReferenceMigrationRule `json:"rules,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeDomainError(w, r, &domain.ValidationError{Field: "body", Message: err.Error()})
			return
		}
	}
	report, err := h.migrator.PlanReferenceMigration(r.Context(), request.Architects, request.Rules)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, report)
}

func (h *referenceMigrationHandler) apply(w http.ResponseWriter, r *http.Request) {
	if !h.available(w, r) {
		return
	}
	var plan domain.ReferenceMigrationReport
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		writeDomainError(w, r, &domain.ValidationError{Field: "plan", Message: err.Error()})
		return
	}
	report, err := h.migrator.ApplyReferenceMigration(r.Context(), plan)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, report)
}
