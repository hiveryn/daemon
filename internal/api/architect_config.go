package api

import (
	"errors"
	"fmt"
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

// --- wire shapes ---
//
// architectConfigDocWire is the declarative, round-trippable config document —
// identical in readArchitectConfig's response and updateArchitectConfig's input.
type architectConfigDocWire struct {
	Repos   map[string]string       `json:"repos"`
	Prompts architectPromptsDocWire `json:"prompts"`
}

type architectPromptsDocWire struct {
	Architect architectPromptPathsDocWire `json:"architect"`
	Ticket    ticketKickoffsDocWire       `json:"ticket"`
}

type architectPromptPathsDocWire struct {
	System  string `json:"system,omitempty"`
	Kickoff string `json:"kickoff,omitempty"`
}

type ticketKickoffsDocWire struct {
	Kickoffs []ticketKickoffDocWire `json:"kickoffs"`
}

type ticketKickoffDocWire struct {
	Path  string   `json:"path"`
	Repos []string `json:"repos"`
}

// resolvedConfigWire mirrors the config document with absolute paths and, for
// each wired prompt, whether the file currently exists. Read-only.
type resolvedConfigWire struct {
	Repos   map[string]string   `json:"repos"`
	Prompts resolvedPromptsWire `json:"prompts"`
}

type resolvedPromptsWire struct {
	Architect resolvedArchitectPromptsWire `json:"architect"`
	Ticket    resolvedTicketWire           `json:"ticket"`
}

type resolvedArchitectPromptsWire struct {
	System  *resolvedPathWire `json:"system"`
	Kickoff *resolvedPathWire `json:"kickoff"`
}

type resolvedTicketWire struct {
	Kickoffs []resolvedKickoffWire `json:"kickoffs"`
}

type resolvedPathWire struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type resolvedKickoffWire struct {
	Path   string   `json:"path"`
	Repos  []string `json:"repos"`
	Exists bool     `json:"exists"`
}

type readArchitectConfigResponse struct {
	Config   architectConfigDocWire `json:"config"`
	Resolved resolvedConfigWire     `json:"resolved"`
	Warnings []string               `json:"warnings"`
	Version  string                 `json:"version"`
}

type updateArchitectConfigResponse struct {
	Config   architectConfigDocWire `json:"config"`
	Resolved resolvedConfigWire     `json:"resolved"`
	Created  []string               `json:"created"`
	Version  string                 `json:"version"`
}

type readDefaultPromptResponse struct {
	Template  string                          `json:"template"`
	Variables []sessionruntime.PromptVariable `json:"variables"`
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

func (h *architectConfigHandler) readArchitectConfig(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	view, err := config.ReadArchitectConfig(workspace, key)
	if err != nil {
		writeConfigError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, readArchitectConfigResponse{
		Config:   docToWire(view.Config),
		Resolved: resolvedToWire(view.Resolved),
		Warnings: promptWarnings(view.Resolved),
		Version:  view.Version,
	})
}

func (h *architectConfigHandler) updateArchitectConfig(w http.ResponseWriter, r *http.Request) {
	key, workspace, ok := h.architectWorkspace(w, r)
	if !ok {
		return
	}
	var request struct {
		Config  architectConfigDocWire `json:"config"`
		Version string                 `json:"version"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	view, err := config.ReplaceArchitectConfig(workspace, key, request.Version, docFromWire(request.Config))
	if err != nil {
		writeConfigError(w, r, err)
		return
	}
	created, err := scaffoldWiredPrompts(view.Resolved)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, updateArchitectConfigResponse{
		Config:   docToWire(view.Config),
		Resolved: resolvedToWire(view.Resolved),
		Created:  created,
		Version:  view.Version,
	})
}

func (h *architectConfigHandler) readDefaultPrompt(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "kind query parameter is required (architect-system, architect-kickoff, or ticket-kickoff)", nil)
		return
	}
	template, err := sessionruntime.DefaultPromptTemplate(kind)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	variables, err := sessionruntime.DefaultPromptVariables(kind)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	if variables == nil {
		variables = []sessionruntime.PromptVariable{}
	}
	writeJSON(w, r, http.StatusOK, readDefaultPromptResponse{
		Template:  string(template),
		Variables: variables,
	})
}

func docToWire(d config.ArchitectConfigDoc) architectConfigDocWire {
	repos := d.Repos
	if repos == nil {
		repos = map[string]string{}
	}
	wire := architectConfigDocWire{
		Repos: repos,
		Prompts: architectPromptsDocWire{
			Architect: architectPromptPathsDocWire{
				System:  d.Prompts.Architect.System,
				Kickoff: d.Prompts.Architect.Kickoff,
			},
			Ticket: ticketKickoffsDocWire{Kickoffs: []ticketKickoffDocWire{}},
		},
	}
	for _, k := range d.Prompts.Ticket {
		wire.Prompts.Ticket.Kickoffs = append(wire.Prompts.Ticket.Kickoffs, ticketKickoffDocWire{
			Path:  k.Path,
			Repos: nonNilStrings(k.Repos),
		})
	}
	return wire
}

func docFromWire(w architectConfigDocWire) config.ArchitectConfigDoc {
	doc := config.ArchitectConfigDoc{
		Repos: w.Repos,
		Prompts: config.ArchitectPromptsDoc{
			Architect: config.ArchitectPromptPathsDoc{
				System:  w.Prompts.Architect.System,
				Kickoff: w.Prompts.Architect.Kickoff,
			},
		},
	}
	for _, k := range w.Prompts.Ticket.Kickoffs {
		doc.Prompts.Ticket = append(doc.Prompts.Ticket, config.TicketKickoffDoc{Path: k.Path, Repos: k.Repos})
	}
	return doc
}

func resolvedToWire(a config.ArchitectConfig) resolvedConfigWire {
	repos := a.Repos
	if repos == nil {
		repos = map[string]string{}
	}
	wire := resolvedConfigWire{
		Repos: repos,
		Prompts: resolvedPromptsWire{
			Architect: resolvedArchitectPromptsWire{
				System:  resolvedPathOrNil(a.SystemPromptPath),
				Kickoff: resolvedPathOrNil(a.KickoffPromptPath),
			},
			Ticket: resolvedTicketWire{Kickoffs: []resolvedKickoffWire{}},
		},
	}
	for _, k := range a.TicketKickoffs {
		wire.Prompts.Ticket.Kickoffs = append(wire.Prompts.Ticket.Kickoffs, resolvedKickoffWire{
			Path:   k.Path,
			Repos:  nonNilStrings(k.Repos),
			Exists: fileExists(k.Path),
		})
	}
	return wire
}

func resolvedPathOrNil(path string) *resolvedPathWire {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return &resolvedPathWire{Path: path, Exists: fileExists(path)}
}

// promptWarnings lists wired prompt files that do not exist on disk. A missing
// prompt file passes validation but hard-errors at spawn, so surfacing it lets
// the agent create the file (updateArchitectConfig also auto-scaffolds these).
func promptWarnings(a config.ArchitectConfig) []string {
	warnings := []string{}
	check := func(path string) {
		if strings.TrimSpace(path) != "" && !fileExists(path) {
			warnings = append(warnings, fmt.Sprintf("wired prompt file %q does not exist; create it (fetch a starting point via readDefaultPrompt) or a session using it will fail to start", path))
		}
	}
	check(a.SystemPromptPath)
	check(a.KickoffPromptPath)
	for _, k := range a.TicketKickoffs {
		check(k.Path)
	}
	return warnings
}

// scaffoldWiredPrompts writes the embedded default template for every wired
// prompt path whose file is missing, returning the absolute paths created.
func scaffoldWiredPrompts(a config.ArchitectConfig) ([]string, error) {
	created := []string{}
	scaffold := func(path, kind string) error {
		if strings.TrimSpace(path) == "" {
			return nil
		}
		didCreate, err := scaffoldPromptFile(path, kind)
		if err != nil {
			return err
		}
		if didCreate {
			created = append(created, path)
		}
		return nil
	}
	if err := scaffold(a.SystemPromptPath, "architect-system"); err != nil {
		return nil, err
	}
	if err := scaffold(a.KickoffPromptPath, "architect-kickoff"); err != nil {
		return nil, err
	}
	for _, k := range a.TicketKickoffs {
		if err := scaffold(k.Path, "ticket-kickoff"); err != nil {
			return nil, err
		}
	}
	return created, nil
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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
