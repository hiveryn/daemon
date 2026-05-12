package api

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

const ingestRoutePrefix = "/internal/agentruntime"

type Dependencies struct {
	Config        config.Config
	Logger        *slog.Logger
	Sessions      domain.SessionService
	Tickets       domain.TicketService
	IngestHandler http.Handler
}

type profilesHandler struct {
	config config.Config
	logger *slog.Logger
}

type architectGroupsHandler struct {
	config config.Config
	logger *slog.Logger
}

type architectsHandler struct {
	config   config.Config
	logger   *slog.Logger
	sessions domain.SessionService
}

type reposHandler struct {
	config config.Config
	logger *slog.Logger
}

type sessionsHandler struct {
	logger   *slog.Logger
	sessions domain.SessionService
}

type ticketsHandler struct {
	config  config.Config
	logger  *slog.Logger
	tickets domain.TicketService
}

type agentProfileResponse struct {
	Name  string            `json:"name"`
	Agent string            `json:"agent"`
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
}

type architectResponse struct {
	Key   string         `json:"key"`
	Path  string         `json:"path"`
	Group string         `json:"group"`
	Repos []repoResponse `json:"repos,omitempty"`
}

type architectGroupResponse struct {
	Name       string              `json:"name"`
	Architects []architectResponse `json:"architects"`
}

type repoResponse struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

func NewHandler(deps Dependencies) http.Handler {
	mux := http.NewServeMux()
	ph := &profilesHandler{config: deps.Config, logger: deps.Logger}
	gh := &architectGroupsHandler{config: deps.Config, logger: deps.Logger}
	ah := &architectsHandler{config: deps.Config, logger: deps.Logger, sessions: deps.Sessions}
	rh := &reposHandler{config: deps.Config, logger: deps.Logger}
	sh := &sessionsHandler{logger: deps.Logger, sessions: deps.Sessions}
	th := &ticketsHandler{config: deps.Config, logger: deps.Logger, tickets: deps.Tickets}

	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/system/home", func(w http.ResponseWriter, r *http.Request) {
		handleSystemHome(w, r, deps.Logger)
	})
	mux.HandleFunc("GET /api/agent-profiles", ph.list)
	mux.HandleFunc("GET /api/agent-profiles/{name}", ph.get)
	mux.HandleFunc("GET /api/architect-groups", gh.list)
	mux.HandleFunc("GET /api/architect-groups/{name}", gh.get)
	mux.HandleFunc("GET /api/architects", ah.list)
	mux.HandleFunc("GET /api/architects/{key}", ah.get)
	mux.HandleFunc("POST /api/architects/{key}/spawn", ah.spawn)
	mux.HandleFunc("GET /api/architects/{key}/tickets", th.list)
	mux.HandleFunc("POST /api/architects/{key}/tickets", th.create)
	mux.HandleFunc("GET /api/architects/{key}/tickets/{id}", th.get)
	mux.HandleFunc("PATCH /api/architects/{key}/tickets/{id}", th.edit)
	mux.HandleFunc("PATCH /api/architects/{key}/tickets/{id}/metadata", th.updateMetadata)
	mux.HandleFunc("DELETE /api/architects/{key}/tickets/{id}", th.delete)
	mux.HandleFunc("POST /api/architects/{key}/tickets/{id}/move", th.move)
	mux.HandleFunc("GET /api/architects/{key}/repos", rh.list)
	mux.HandleFunc("GET /api/architects/{key}/repos/{repoKey}", rh.get)
	mux.HandleFunc("GET /api/sessions", sh.list)
	mux.HandleFunc("GET /api/sessions/{id}", sh.get)
	mux.HandleFunc("DELETE /api/sessions/{id}", sh.delete)
	mux.HandleFunc("GET /api/sessions/{id}/events", sh.events)
	mux.HandleFunc("GET /ws/session/{id}", sh.ws)
	if deps.IngestHandler != nil {
		mux.Handle(ingestRoutePrefix+"/", deps.IngestHandler)
	}

	return requestID(recovery(accessLog(deps.Logger, mux)))
}

func listAgentProfiles(cfg config.Config) []agentProfileResponse {
	names := configKeys(cfg.AgentProfiles)
	profiles := make([]agentProfileResponse, 0, len(names))
	for _, name := range names {
		profile := cfg.AgentProfiles[name]
		profiles = append(profiles, agentProfileResponse{
			Name:  name,
			Agent: profile.Agent,
			Args:  append([]string(nil), profile.Args...),
			Env:   cloneStringMap(profile.Env),
		})
	}
	return profiles
}

func getAgentProfile(cfg config.Config, name string) (agentProfileResponse, bool) {
	profile, ok := cfg.AgentProfiles[name]
	if !ok {
		return agentProfileResponse{}, false
	}
	return agentProfileResponse{
		Name:  name,
		Agent: profile.Agent,
		Args:  append([]string(nil), profile.Args...),
		Env:   cloneStringMap(profile.Env),
	}, true
}

func listArchitects(cfg config.Config) []architectResponse {
	keys := configKeys(cfg.Architects)
	architects := make([]architectResponse, 0, len(keys))
	for _, key := range keys {
		architects = append(architects, buildArchitectResponse(key, cfg.Architects[key], true))
	}
	return architects
}

func getArchitect(cfg config.Config, key string, includeRepos bool) (architectResponse, bool) {
	architect, ok := cfg.Architects[key]
	if !ok {
		return architectResponse{}, false
	}
	return buildArchitectResponse(key, architect, includeRepos), true
}

func getArchitectPath(cfg config.Config, key string) (string, bool) {
	architect, ok := cfg.Architects[key]
	if !ok {
		return "", false
	}
	return architect.Path, true
}

func listArchitectGroups(cfg config.Config) []architectGroupResponse {
	groups := groupArchitects(cfg)
	names := configKeys(groups)
	items := make([]architectGroupResponse, 0, len(names))
	for _, name := range names {
		items = append(items, architectGroupResponse{Name: name, Architects: groups[name]})
	}
	return items
}

func getArchitectGroup(cfg config.Config, name string) (architectGroupResponse, bool) {
	groups := groupArchitects(cfg)
	architects, ok := groups[name]
	if !ok {
		return architectGroupResponse{}, false
	}
	return architectGroupResponse{Name: name, Architects: architects}, true
}

func listRepos(cfg config.Config, architectKey string) ([]repoResponse, bool) {
	architect, ok := cfg.Architects[architectKey]
	if !ok {
		return nil, false
	}
	return buildRepos(architect.Repos), true
}

func getRepo(cfg config.Config, architectKey, repoKey string) (repoResponse, bool, bool) {
	architect, ok := cfg.Architects[architectKey]
	if !ok {
		return repoResponse{}, false, false
	}
	path, ok := architect.Repos[repoKey]
	if !ok {
		return repoResponse{}, true, false
	}
	return repoResponse{Key: repoKey, Path: path}, true, true
}

func buildArchitectResponse(key string, architect config.ArchitectConfig, includeRepos bool) architectResponse {
	resp := architectResponse{
		Key:   key,
		Path:  architect.Path,
		Group: architect.Group,
	}
	if includeRepos {
		resp.Repos = buildRepos(architect.Repos)
	}
	return resp
}

func buildRepos(repos map[string]string) []repoResponse {
	keys := configKeys(repos)
	items := make([]repoResponse, 0, len(keys))
	for _, key := range keys {
		items = append(items, repoResponse{Key: key, Path: repos[key]})
	}
	return items
}

func groupArchitects(cfg config.Config) map[string][]architectResponse {
	grouped := map[string][]architectResponse{}
	for _, key := range configKeys(cfg.Architects) {
		architect := cfg.Architects[key]
		grouped[architect.Group] = append(grouped[architect.Group], buildArchitectResponse(key, architect, false))
	}
	return grouped
}

func configKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	var validationErr *domain.ValidationError
	if errors.As(err, &validationErr) {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), validationErr.Error(), map[string]string{
			"field": validationErr.Field,
		})
		return
	}

	var conflictErr *domain.ConflictError
	if errors.As(err, &conflictErr) {
		writeError(w, r, http.StatusConflict, string(domain.ErrCodeConflict), conflictErr.Error(), map[string]string{
			"resource": conflictErr.Resource,
			"field":    conflictErr.Field,
		})
		return
	}

	var notFoundErr *domain.NotFoundError
	if errors.As(err, &notFoundErr) {
		writeError(w, r, http.StatusNotFound, string(domain.ErrCodeNotFound), notFoundErr.Error(), map[string]string{
			"resource": notFoundErr.Resource,
			"id":       notFoundErr.ID,
		})
		return
	}

	writeError(w, r, http.StatusInternalServerError, string(domain.ErrCodeInternal), "internal server error", nil)
}
