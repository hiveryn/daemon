package api

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/sessionruntime"
)

type architectConfigHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
}

type kickoffResponse struct {
	Path    string   `json:"path"`
	Repos   []string `json:"repos"`
	Default bool     `json:"default"`
}

type promptPathResponse struct {
	Path string `json:"path"`
}

type architectPromptsResponse struct {
	System  *promptPathResponse `json:"system"`
	Kickoff *promptPathResponse `json:"kickoff"`
}

type addKickoffResponse struct {
	Path    string   `json:"path"`
	Repos   []string `json:"repos"`
	Default bool     `json:"default"`
	Created bool     `json:"created"`
}

type updateKickoffResponse struct {
	Path    string   `json:"path"`
	Repos   []string `json:"repos"`
	Default bool     `json:"default"`
}

type removeKickoffResponse struct {
	FallbackRepos []string `json:"fallbackRepos"`
}

type setPromptResponse struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
}

type removeRepoResponse struct {
	Key string `json:"key"`
}

// architectWorkspace resolves the workspace path for the {key} path param from
// fresh config, writing a 404 and returning ok=false when the architect is
// unknown.
func (h *architectConfigHandler) architectWorkspace(w http.ResponseWriter, r *http.Request) (key, workspace string, ok bool) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return "", "", false
	}
	key = r.PathValue("key")
	architect, exists := cfg.Architects[key]
	if !exists {
		writeArchitectNotFound(w, r, key)
		return "", "", false
	}
	return key, architect.Path, true
}

func (h *architectConfigHandler) listRepos(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	repos, ok := listRepos(cfg, r.PathValue("key"))
	if !ok {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]repoResponse{"repos": repos})
}

func (h *architectConfigHandler) listKickoffs(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	architect, exists := cfg.Architects[r.PathValue("key")]
	if !exists {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}
	kickoffs := make([]kickoffResponse, 0, len(architect.TicketKickoffs))
	for _, kickoff := range architect.TicketKickoffs {
		repos := kickoff.Repos
		if repos == nil {
			repos = []string{}
		}
		kickoffs = append(kickoffs, kickoffResponse{
			Path:    kickoff.Path,
			Repos:   repos,
			Default: len(kickoff.Repos) == 0,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string][]kickoffResponse{"kickoffs": kickoffs})
}

func (h *architectConfigHandler) getArchitectPrompts(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	architect, exists := cfg.Architects[r.PathValue("key")]
	if !exists {
		writeArchitectNotFound(w, r, r.PathValue("key"))
		return
	}
	writeJSON(w, r, http.StatusOK, architectPromptsResponse{
		System:  promptPathOrNil(architect.SystemPromptPath),
		Kickoff: promptPathOrNil(architect.KickoffPromptPath),
	})
}

func (h *architectConfigHandler) describePromptSchema(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "kind query parameter is required (architect or ticket)", nil)
		return
	}
	variables, err := sessionruntime.PromptSchema(kind)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]sessionruntime.PromptVariable{"variables": variables})
}

func (h *architectConfigHandler) addRepo(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	var request struct {
		Key  string `json:"key"`
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	absPath, err := config.AddRepo(workspace, key, request.Key, request.Path)
	if err != nil {
		writeConfigError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, repoResponse{Key: strings.TrimSpace(request.Key), Path: absPath})
}

func (h *architectConfigHandler) removeRepo(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	repoKey := r.PathValue("repoKey")
	if err := config.RemoveRepo(workspace, key, repoKey); err != nil {
		writeConfigError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, removeRepoResponse{Key: repoKey})
}

func (h *architectConfigHandler) addKickoff(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	var request struct {
		Path  string   `json:"path"`
		Repos []string `json:"repos,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	absPath, err := config.AddKickoff(workspace, key, request.Path, request.Repos)
	if err != nil {
		writeConfigError(w, r, err)
		return
	}
	created, err := scaffoldPromptFile(absPath, "ticket-kickoff")
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, addKickoffResponse{
		Path:    absPath,
		Repos:   normalizedRepos(request.Repos),
		Default: len(normalizedRepos(request.Repos)) == 0,
		Created: created,
	})
}

func (h *architectConfigHandler) updateKickoff(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	var request struct {
		Path  string   `json:"path"`
		Repos []string `json:"repos,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	absPath, err := config.UpdateKickoff(workspace, key, request.Path, request.Repos)
	if err != nil {
		writeConfigError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, updateKickoffResponse{
		Path:    absPath,
		Repos:   normalizedRepos(request.Repos),
		Default: len(normalizedRepos(request.Repos)) == 0,
	})
}

func (h *architectConfigHandler) removeKickoff(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	fallback, err := config.RemoveKickoff(workspace, key, request.Path)
	if err != nil {
		writeConfigError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, removeKickoffResponse{FallbackRepos: fallback})
}

func (h *architectConfigHandler) setArchitectSystem(w http.ResponseWriter, r *http.Request) {
	h.setArchitectPrompt(w, r, config.SetArchitectSystem, "architect-system")
}

func (h *architectConfigHandler) setArchitectKickoff(w http.ResponseWriter, r *http.Request) {
	h.setArchitectPrompt(w, r, config.SetArchitectKickoff, "architect-kickoff")
}

func (h *architectConfigHandler) setArchitectPrompt(
	w http.ResponseWriter,
	r *http.Request,
	set func(workspacePath, architectKey, promptPath string) (string, error),
	templateKind string,
) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	absPath, err := set(workspace, key, request.Path)
	if err != nil {
		writeConfigError(w, r, err)
		return
	}
	created, err := scaffoldPromptFile(absPath, templateKind)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, setPromptResponse{Path: absPath, Created: created})
}

func promptPathOrNil(path string) *promptPathResponse {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return &promptPathResponse{Path: path}
}

func normalizedRepos(repos []string) []string {
	scope := make([]string, 0, len(repos))
	for _, repo := range repos {
		trimmed := strings.TrimSpace(repo)
		if trimmed != "" {
			scope = append(scope, trimmed)
		}
	}
	return scope
}

// scaffoldPromptFile writes the embedded default template to absPath when the
// file does not yet exist, so the agent edits from a correct starting point.
// Returns true when a file was created.
func scaffoldPromptFile(absPath, templateKind string) (bool, error) {
	if _, err := os.Stat(absPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	template, err := sessionruntime.DefaultPromptTemplate(templateKind)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(absPath, template, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// writeConfigError maps a config.MutationError onto the right status/envelope,
// falling back to writeDomainError for anything else.
func writeConfigError(w http.ResponseWriter, r *http.Request, err error) {
	var mErr *config.MutationError
	if errors.As(err, &mErr) {
		switch mErr.Kind {
		case config.MutationErrorValidation:
			writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), mErr.Message, nil)
		case config.MutationErrorConflict:
			writeError(w, r, http.StatusConflict, string(domain.ErrCodeConflict), mErr.Message, nil)
		case config.MutationErrorNotFound:
			writeError(w, r, http.StatusNotFound, string(domain.ErrCodeNotFound), mErr.Message, nil)
		default:
			writeError(w, r, http.StatusInternalServerError, string(domain.ErrCodeInternal), mErr.Message, nil)
		}
		return
	}
	writeDomainError(w, r, err)
}
