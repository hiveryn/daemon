package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/contextref"
	"github.com/hiveryn/daemon/internal/domain"
)

// contextHandler serves the worker context reads (readContext/listContext).
// The readable scope is the calling session's own architect — its workspace
// and configured repos — taken from the session record, never from the
// request; a session that does not exist (or has ended) reads nothing.
type contextHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
	sessions     domain.SessionService
	reader       *contextref.Reader
}

func (h *contextHandler) scope(w http.ResponseWriter, r *http.Request) (contextref.Scope, bool) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return contextref.Scope{}, false
	}
	session, err := h.sessions.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return contextref.Scope{}, false
	}
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return contextref.Scope{}, false
	}
	scope, err := contextref.ScopeFor(cfg, session.ArchitectKey)
	if err != nil {
		writeDomainError(w, r, err)
		return contextref.Scope{}, false
	}
	return scope, true
}

func queryInt(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &domain.ValidationError{Field: name, Message: "must be an integer"}
	}
	return n, nil
}

func (h *contextHandler) readFile(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	offset, err := queryInt(r, "offset")
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	q := r.URL.Query()
	file, err := h.reader.ReadFile(r.Context(), scope, domain.ContextReadRequest{Reference: q.Get("reference"), Offset: offset, Limit: limit, ExpectedSHA256: q.Get("expected_sha256")})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, file)
}

func (h *contextHandler) listDirectory(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	offset, err := queryInt(r, "offset")
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	q := r.URL.Query()
	listing, err := h.reader.List(r.Context(), scope, domain.ContextListRequest{Reference: q.Get("reference"), Offset: offset, Limit: limit, ExpectedSHA256: q.Get("expected_sha256")})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, listing)
}
