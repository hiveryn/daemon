package api

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"

	"github.com/hiveryn/daemon/internal/archevents"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/logging"
)

const ingestRoutePrefix = "/internal/agentruntime"

type Dependencies struct {
	Config          config.Config
	ConfigSource    config.Source
	Runtime         config.Runtime
	BaseURL         string
	Logger          *slog.Logger
	RequestLogger   *logging.RequestLogger
	Sessions        domain.SessionService
	Tickets         domain.TicketService
	Workspaces      domain.WorkspaceService
	Actions         domain.ActionService
	IngestHandler   http.Handler
	ArchitectEvents *archevents.Hub
}

type profilesHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
}

type architectsHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
	sessions     domain.SessionService
	tickets      domain.TicketService
}

type reposHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
}

type shortcutsHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
}

type sessionsHandler struct {
	config           config.Config
	configSource     config.Source
	logger           *slog.Logger
	sessions         domain.SessionService
	tickets          domain.TicketService
	publishArchitect func(key string, event domain.ArchitectEvent)
}

type ticketsHandler struct {
	config           config.Config
	configSource     config.Source
	logger           *slog.Logger
	sessions         domain.SessionService
	tickets          domain.TicketService
	publishArchitect func(key string, event domain.ArchitectEvent)
}

type agentProfileResponse struct {
	Name  string            `json:"name"`
	Agent string            `json:"agent"`
	Model string            `json:"model,omitempty"`
	Yolo  bool              `json:"yolo,omitempty"`
	Mode  string            `json:"mode,omitempty"`
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
}

type architectResponse struct {
	Key   string         `json:"key"`
	Name  string         `json:"name"`
	Path  string         `json:"path"`
	Repos []repoResponse `json:"repos,omitempty"`
}

type repoResponse struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

func NewHandler(deps Dependencies) http.Handler {
	mux := http.NewServeMux()
	ph := &profilesHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger}
	ah := &architectsHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger, sessions: deps.Sessions, tickets: deps.Tickets}
	rh := &reposHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger}
	sch := &shortcutsHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger}
	dch := &desktopConfigHandler{config: deps.Config}
	srh := &systemRuntimeHandler{runtime: deps.Runtime, bindAddress: deps.Config.BindAddress, port: deps.Config.Port, baseURL: deps.BaseURL, configSource: deps.ConfigSource}
	sh := &sessionsHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger, sessions: deps.Sessions, tickets: deps.Tickets}
	th := &ticketsHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger, sessions: deps.Sessions, tickets: deps.Tickets}
	eh := &architectEventsHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger, hub: deps.ArchitectEvents}
	fh := &fsHandler{logger: deps.Logger}
	wh := &workspaceHandler{config: deps.Config, configSource: deps.ConfigSource, logger: deps.Logger, workspaces: deps.Workspaces}
	ach := &actionsHandler{logger: deps.Logger, actions: deps.Actions}

	if deps.ArchitectEvents != nil {
		hub := deps.ArchitectEvents
		th.publishArchitect = hub.Publish
		sh.publishArchitect = hub.Publish
	}

	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/system/runtime", srh.get)
	mux.HandleFunc("GET /api/agent-profiles", ph.list)
	mux.HandleFunc("GET /api/agent-profiles/{name}", ph.get)
	mux.HandleFunc("GET /api/architects", ah.list)
	mux.HandleFunc("GET /api/architects/status", ah.listStatus)
	mux.HandleFunc("GET /api/architects/{key}", ah.get)
	mux.HandleFunc("GET /api/architects/{key}/conclusions", ah.listConclusions)
	mux.HandleFunc("GET /api/architects/{key}/conclusions/recent", ah.readRecentConclusion)
	mux.HandleFunc("GET /api/architects/{key}/conclusions/{id}", ah.readConclusion)
	mux.HandleFunc("GET /api/architects/{key}/tickets", th.list)
	mux.HandleFunc("POST /api/architects/{key}/tickets", th.create)
	mux.HandleFunc("GET /api/architects/{key}/tickets/{id}", th.get)
	mux.HandleFunc("PATCH /api/architects/{key}/tickets/{id}", th.edit)
	mux.HandleFunc("PATCH /api/architects/{key}/tickets/{id}/metadata", th.updateMetadata)
	mux.HandleFunc("DELETE /api/architects/{key}/tickets/{id}", th.delete)
	mux.HandleFunc("POST /api/architects/{key}/tickets/{id}/move", th.move)
	mux.HandleFunc("POST /api/architects/{key}/tickets/{id}/move-to-done", th.moveToDone)
	mux.HandleFunc("GET /api/architects/{key}/repos", rh.list)
	mux.HandleFunc("GET /api/architects/{key}/repos/{repoKey}", rh.get)
	mux.HandleFunc("GET /api/architects/{key}/repos/{repoKey}/diff", rh.diff)
	mux.HandleFunc("GET /api/architects/{key}/repos/{repoKey}/status", rh.status)
	mux.HandleFunc("GET /api/architects/{key}/repos/{repoKey}/commits/{sha}/diff", rh.commitDiff)
	mux.HandleFunc("GET /api/architects/{key}/workspace/check", wh.check)
	mux.HandleFunc("GET /api/architects/{key}/workspace/worker-preflight", wh.workerPreflight)
	mux.HandleFunc("GET /api/architects/{key}/workspace/artifacts/{kind}", wh.describeArtifact)
	mux.HandleFunc("GET /api/architects/{key}/workflows", wh.listWorkflows)
	mux.HandleFunc("GET /api/config/shortcuts", sch.get)
	mux.HandleFunc("GET /api/config/desktop", dch.get)
	mux.HandleFunc("GET /api/fs/tree", fh.tree)
	mux.HandleFunc("GET /api/fs/file", fh.file)
	mux.HandleFunc("PUT /api/fs/file", fh.writeFile)
	mux.HandleFunc("GET /api/fs/search", fh.search)
	mux.HandleFunc("GET /api/fs/search-content", fh.searchContent)
	mux.HandleFunc("GET /api/architects/{key}/events", eh.events)
	mux.HandleFunc("POST /api/sessions", sh.createSession)
	mux.HandleFunc("GET /api/sessions", sh.list)
	mux.HandleFunc("GET /api/sessions/{id}", sh.get)
	mux.HandleFunc("POST /api/sessions/{id}/runs", sh.createRun)
	mux.HandleFunc("POST /api/sessions/{id}/conclude", sh.conclude)
	mux.HandleFunc("POST /api/sessions/{id}/discard", sh.discard)
	// Agent-facing intent requests. These block until the user answers or the
	// tool's policy fires.
	mux.HandleFunc("POST /api/sessions/{id}/intents/conclude-session", sh.concludeSessionIntent)
	mux.HandleFunc("POST /api/sessions/{id}/intents/create-work-ticket", sh.createWorkTicketIntent)
	// Desktop-facing intent resolution, addressed by intent id.
	mux.HandleFunc("GET /api/sessions/{id}/intents/{intentID}", sh.getDeferredIntent)
	mux.HandleFunc("POST /api/sessions/{id}/intents/{intentID}/approve", sh.approveIntent)
	mux.HandleFunc("POST /api/sessions/{id}/intents/{intentID}/deny", sh.denyIntent)
	mux.HandleFunc("GET /api/sessions/{id}/events", sh.events)
	mux.HandleFunc("GET /api/sessions/{id}/tabs", sh.listTabs)
	mux.HandleFunc("GET /api/sessions/{id}/ticket", sh.getTicket)
	mux.HandleFunc("POST /api/sessions/{id}/terminals", sh.createTerminal)
	mux.HandleFunc("GET /api/sessions/{id}/terminal-workdirs", sh.listTerminalWorkdirs)
	mux.HandleFunc("GET /api/sessions/{id}/terminals", sh.listTerminals)
	mux.HandleFunc("DELETE /api/sessions/{id}/terminals/{uuid}", sh.killTerminal)
	mux.HandleFunc("GET /ws/session/{id}/terminal/{uuid}", sh.wsTerminal)
	// Actions: global definitions, manual launches and the durable execution
	// records. Executions live under /api/action-runs so no action name can
	// collide with a route segment.
	mux.HandleFunc("GET /api/actions", ach.list)
	mux.HandleFunc("GET /api/actions/events", ach.events)
	mux.HandleFunc("GET /api/actions/{name}", ach.get)
	mux.HandleFunc("POST /api/actions/{name}/runs", ach.launch)
	mux.HandleFunc("GET /api/action-runs", ach.listRuns)
	mux.HandleFunc("GET /api/action-runs/{id}", ach.getRun)
	mux.HandleFunc("POST /api/action-runs/{id}/cancel", ach.cancelRun)
	// Agent-facing action tools, addressed by the calling action session.
	mux.HandleFunc("POST /api/sessions/{id}/action/conclude", ach.conclude)
	mux.HandleFunc("GET /api/sessions/{id}/action/conclusions", ach.recentConclusions)
	// Architect-facing Actions, addressed by the calling architect session:
	// discovery, the deferred execute request, and the architect-scoped
	// result and bounded wait under the one execution id.
	mux.HandleFunc("GET /api/sessions/{id}/available-actions", ach.available)
	mux.HandleFunc("POST /api/sessions/{id}/intents/execute-action", ach.executeIntent)
	mux.HandleFunc("GET /api/sessions/{id}/action-results/{executionID}", ach.result)
	mux.HandleFunc("GET /api/sessions/{id}/action-results/{executionID}/wait", ach.wait)
	if deps.IngestHandler != nil {
		mux.Handle(ingestRoutePrefix+"/", deps.IngestHandler)
	}

	return requestID(accessLog(deps.Logger, deps.RequestLogger, recovery(mux)))
}

func listAgentProfiles(cfg config.Config) []agentProfileResponse {
	names := configKeys(cfg.Variants)
	profiles := make([]agentProfileResponse, 0, len(names))
	for _, name := range names {
		profile := cfg.Variants[name]
		profiles = append(profiles, agentProfileResponse{
			Name:  name,
			Agent: profile.Agent,
			Model: profile.Model,
			Yolo:  profile.Yolo,
			Mode:  profile.Mode,
			Args:  append([]string(nil), profile.Args...),
			Env:   cloneStringMap(profile.Env),
		})
	}
	return profiles
}

func currentConfig(static config.Config, source config.Source) (config.Config, error) {
	if source == nil {
		return static.Clone(), nil
	}
	return source.Current()
}

func getAgentProfile(cfg config.Config, name string) (agentProfileResponse, bool) {
	profile, ok := cfg.Variants[name]
	if !ok {
		return agentProfileResponse{}, false
	}
	return agentProfileResponse{
		Name:  name,
		Agent: profile.Agent,
		Model: profile.Model,
		Yolo:  profile.Yolo,
		Mode:  profile.Mode,
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
		Key:  key,
		Name: architect.Name,
		Path: architect.Path,
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
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), map[string]string{
			"field": validationErr.Field,
		})
		return
	}

	var conflictErr *domain.ConflictError
	if errors.As(err, &conflictErr) {
		writeError(w, r, http.StatusConflict, string(domain.ErrCodeConflict), err.Error(), map[string]string{
			"resource": conflictErr.Resource,
			"field":    conflictErr.Field,
		})
		return
	}

	var notFoundErr *domain.NotFoundError
	if errors.As(err, &notFoundErr) {
		writeError(w, r, http.StatusNotFound, string(domain.ErrCodeNotFound), err.Error(), map[string]string{
			"resource": notFoundErr.Resource,
			"id":       notFoundErr.ID,
		})
		return
	}

	slog.Error("unexpected internal error", "error", err)
	writeError(w, r, http.StatusInternalServerError, string(domain.ErrCodeInternal), "internal server error: "+err.Error(), nil)
}
