package api

import (
	"log/slog"
	"net/http"
	"sort"

	"github.com/hiveryn/daemon/internal/config"
)

type profilesHandler struct {
	config config.Config
	logger *slog.Logger
}

type architectGroupsHandler struct {
	config config.Config
	logger *slog.Logger
}

type architectsHandler struct {
	config config.Config
	logger *slog.Logger
}

type reposHandler struct {
	config config.Config
	logger *slog.Logger
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

func NewHandler(cfg config.Config, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	ph := &profilesHandler{config: cfg, logger: logger}
	gh := &architectGroupsHandler{config: cfg, logger: logger}
	ah := &architectsHandler{config: cfg, logger: logger}
	rh := &reposHandler{config: cfg, logger: logger}

	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/system/home", func(w http.ResponseWriter, r *http.Request) {
		handleSystemHome(w, r, logger)
	})
	mux.HandleFunc("GET /api/agent-profiles", ph.list)
	mux.HandleFunc("GET /api/agent-profiles/{name}", ph.get)
	mux.HandleFunc("GET /api/architect-groups", gh.list)
	mux.HandleFunc("GET /api/architect-groups/{name}", gh.get)
	mux.HandleFunc("GET /api/architects", ah.list)
	mux.HandleFunc("GET /api/architects/{key}", ah.get)
	mux.HandleFunc("GET /api/architects/{key}/repos", rh.list)
	mux.HandleFunc("GET /api/architects/{key}/repos/{repoKey}", rh.get)

	return requestID(recovery(accessLog(logger, mux)))
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
		architects = append(architects, buildArchitectResponse(key, cfg.Architects[key], false))
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
