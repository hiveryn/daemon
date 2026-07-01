package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// MutationErrorKind classifies a config-mutation failure so callers (the HTTP
// layer) can map it onto the right status code / error envelope.
type MutationErrorKind string

const (
	MutationErrorValidation MutationErrorKind = "validation"
	MutationErrorConflict   MutationErrorKind = "conflict"
	MutationErrorNotFound   MutationErrorKind = "not_found"
)

// MutationError is a structured error returned by the architect-config mutation
// ops. Agents recover well from clear errors, so messages are specific.
type MutationError struct {
	Kind    MutationErrorKind
	Message string
}

func (e *MutationError) Error() string { return e.Message }

func mutationErr(kind MutationErrorKind, format string, args ...any) *MutationError {
	return &MutationError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// validateArchitect enforces the per-architect rules. It is the single ruleset
// shared by the loader (Config.Validate) and the mutation layer's write
// backstop, so a write that would fail to load is refused before it is
// persisted — session spawning can never be bricked by a mutation.
func validateArchitect(key string, architect ArchitectConfig) error {
	if strings.TrimSpace(architect.Name) == "" {
		return fmt.Errorf("architects.%s.name is required", key)
	}
	if strings.TrimSpace(architect.Path) == "" {
		return fmt.Errorf("architects.%s.path is required", key)
	}
	for repoKey, repoPath := range architect.Repos {
		if strings.TrimSpace(repoKey) == "" {
			return fmt.Errorf("architects.%s.repos keys must not be blank", key)
		}
		if strings.TrimSpace(repoPath) == "" {
			return fmt.Errorf("architects.%s.repos.%s is required", key, repoKey)
		}
	}

	defaultKickoffs := 0
	repoKickoffSeen := map[string]struct{}{}
	for i, kickoff := range architect.TicketKickoffs {
		if strings.TrimSpace(kickoff.Path) == "" {
			return fmt.Errorf("architects.%s.prompts.ticket.kickoffs[%d].path is required", key, i)
		}
		if len(kickoff.Repos) == 0 {
			defaultKickoffs++
			if defaultKickoffs > 1 {
				return fmt.Errorf("architects.%s.prompts.ticket.kickoffs has multiple default (no-repos) entries", key)
			}
			continue
		}
		for _, repoKey := range kickoff.Repos {
			if _, ok := architect.Repos[repoKey]; !ok {
				return fmt.Errorf("architects.%s.prompts.ticket.kickoffs[%d] references unknown repo %q", key, i, repoKey)
			}
			if _, dup := repoKickoffSeen[repoKey]; dup {
				return fmt.Errorf("architects.%s.prompts.ticket.kickoffs has multiple entries for repo %q", key, repoKey)
			}
			repoKickoffSeen[repoKey] = struct{}{}
		}
	}
	return nil
}

// storeArchitectPath converts a caller-supplied prompt path into the form stored
// in hiveryn.yaml (relative to the workspace when it lives under it, otherwise
// absolute) and the absolute path returned to the agent. Relative input is
// interpreted against the workspace, mirroring resolveArchitectPath; a leading
// ~ is home-expanded.
func storeArchitectPath(workspacePath, input string) (stored, absolute string, err error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", mutationErr(MutationErrorValidation, "path is required")
	}

	absWorkspace, err := filepath.Abs(workspacePath)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace path %q: %w", workspacePath, err)
	}

	var abs string
	switch {
	case trimmed == "~" || strings.HasPrefix(trimmed, "~/"):
		abs, err = expandHomePath(trimmed)
		if err != nil {
			return "", "", err
		}
	case filepath.IsAbs(trimmed):
		abs = filepath.Clean(trimmed)
	default:
		abs = filepath.Join(absWorkspace, trimmed)
	}

	rel, err := filepath.Rel(absWorkspace, abs)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel, abs, nil
	}
	return abs, abs, nil
}

// updateArchitectFile reads the architect's hiveryn.yaml, applies apply, then
// validates the resulting config against the loader's ruleset before writing.
// A validation failure aborts the write, so hiveryn.yaml is never left in a
// state that would fail to load.
func updateArchitectFile(workspacePath, architectKey string, apply func(*architectFile) error) error {
	if strings.TrimSpace(workspacePath) == "" {
		return mutationErr(MutationErrorValidation, "architect workspace path is required")
	}

	filePath := filepath.Join(workspacePath, architectConfigFileName)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read %q: %w", filePath, err)
	}

	var file architectFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("decode YAML %q: %w", filePath, err)
	}

	if err := apply(&file); err != nil {
		return err
	}

	resolved, err := architectConfigFromFile(architectKey, workspacePath, file)
	if err != nil {
		return err
	}
	if err := validateArchitect(architectKey, resolved); err != nil {
		return mutationErr(MutationErrorValidation, "%s", err.Error())
	}

	out, err := yaml.Marshal(file)
	if err != nil {
		return fmt.Errorf("marshal %q: %w", filePath, err)
	}
	if err := os.WriteFile(filePath, out, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", filePath, err)
	}
	return nil
}

func ensureTicketPrompts(file *architectFile) *ticketPrompts {
	if file.Prompts == nil {
		file.Prompts = &architectPrompts{}
	}
	if file.Prompts.Ticket == nil {
		file.Prompts.Ticket = &ticketPrompts{}
	}
	return file.Prompts.Ticket
}

func ensureArchitectPromptPaths(file *architectFile) *architectPromptPaths {
	if file.Prompts == nil {
		file.Prompts = &architectPrompts{}
	}
	if file.Prompts.Architect == nil {
		file.Prompts.Architect = &architectPromptPaths{}
	}
	return file.Prompts.Architect
}

func normalizeRepoScope(repos []string) []string {
	scope := make([]string, 0, len(repos))
	for _, repo := range repos {
		trimmed := strings.TrimSpace(repo)
		if trimmed != "" {
			scope = append(scope, trimmed)
		}
	}
	return scope
}

// AddRepo adds a repo entry to hiveryn.yaml. The path is stored verbatim
// (absolute or ~-prefixed) and the resolved absolute path is returned.
func AddRepo(workspacePath, architectKey, repoKey, repoPath string) (string, error) {
	repoKey = strings.TrimSpace(repoKey)
	if repoKey == "" {
		return "", mutationErr(MutationErrorValidation, "repo key is required")
	}
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return "", mutationErr(MutationErrorValidation, "repo path is required")
	}
	absolute, err := expandHomePath(repoPath)
	if err != nil {
		return "", err
	}

	err = updateArchitectFile(workspacePath, architectKey, func(file *architectFile) error {
		if file.Repos == nil {
			file.Repos = map[string]string{}
		}
		if _, exists := file.Repos[repoKey]; exists {
			return mutationErr(MutationErrorConflict, "repo %q already exists", repoKey)
		}
		file.Repos[repoKey] = repoPath
		return nil
	})
	if err != nil {
		return "", err
	}
	return absolute, nil
}

// RemoveRepo removes a repo entry. It refuses to remove a repo still referenced
// by a ticket-kickoff entry (which would leave a dangling reference).
func RemoveRepo(workspacePath, architectKey, repoKey string) error {
	repoKey = strings.TrimSpace(repoKey)
	if repoKey == "" {
		return mutationErr(MutationErrorValidation, "repo key is required")
	}

	return updateArchitectFile(workspacePath, architectKey, func(file *architectFile) error {
		if _, exists := file.Repos[repoKey]; !exists {
			return mutationErr(MutationErrorNotFound, "repo %q not found", repoKey)
		}
		if file.Prompts != nil && file.Prompts.Ticket != nil {
			for _, kickoff := range file.Prompts.Ticket.Kickoffs {
				if slices.Contains(kickoff.Repos, repoKey) {
					return mutationErr(MutationErrorConflict, "repo %q is referenced by a ticket kickoff entry (%s); re-scope or remove that entry first", repoKey, kickoff.Path)
				}
			}
		}
		delete(file.Repos, repoKey)
		return nil
	})
}

// AddKickoff adds a ticket-kickoff entry. An empty repos slice creates the
// default (no-repos) entry. The resolved absolute prompt path is returned.
func AddKickoff(workspacePath, architectKey, promptPath string, repos []string) (string, error) {
	stored, absolute, err := storeArchitectPath(workspacePath, promptPath)
	if err != nil {
		return "", err
	}
	scope := normalizeRepoScope(repos)

	err = updateArchitectFile(workspacePath, architectKey, func(file *architectFile) error {
		ticket := ensureTicketPrompts(file)
		for _, kickoff := range ticket.Kickoffs {
			if kickoff.Path == stored {
				return mutationErr(MutationErrorConflict, "a kickoff entry for path %q already exists", stored)
			}
		}
		if err := checkKickoffScope(file, ticket.Kickoffs, -1, scope); err != nil {
			return err
		}
		ticket.Kickoffs = append(ticket.Kickoffs, ticketKickoffEntry{Path: stored, Repos: scope})
		return nil
	})
	if err != nil {
		return "", err
	}
	return absolute, nil
}

// UpdateKickoff re-scopes an existing ticket-kickoff entry identified by path.
func UpdateKickoff(workspacePath, architectKey, promptPath string, repos []string) (string, error) {
	stored, absolute, err := storeArchitectPath(workspacePath, promptPath)
	if err != nil {
		return "", err
	}
	scope := normalizeRepoScope(repos)

	err = updateArchitectFile(workspacePath, architectKey, func(file *architectFile) error {
		if file.Prompts == nil || file.Prompts.Ticket == nil {
			return mutationErr(MutationErrorNotFound, "no kickoff entry for path %q", stored)
		}
		kickoffs := file.Prompts.Ticket.Kickoffs
		idx := kickoffIndex(kickoffs, stored)
		if idx == -1 {
			return mutationErr(MutationErrorNotFound, "no kickoff entry for path %q", stored)
		}
		if err := checkKickoffScope(file, kickoffs, idx, scope); err != nil {
			return err
		}
		kickoffs[idx].Repos = scope
		return nil
	})
	if err != nil {
		return "", err
	}
	return absolute, nil
}

// RemoveKickoff removes a ticket-kickoff entry and returns the repos that now
// fall back to the default entry (empty when the removed entry was the default).
func RemoveKickoff(workspacePath, architectKey, promptPath string) ([]string, error) {
	stored, _, err := storeArchitectPath(workspacePath, promptPath)
	if err != nil {
		return nil, err
	}

	fallback := []string{}
	err = updateArchitectFile(workspacePath, architectKey, func(file *architectFile) error {
		if file.Prompts == nil || file.Prompts.Ticket == nil {
			return mutationErr(MutationErrorNotFound, "no kickoff entry for path %q", stored)
		}
		kickoffs := file.Prompts.Ticket.Kickoffs
		idx := kickoffIndex(kickoffs, stored)
		if idx == -1 {
			return mutationErr(MutationErrorNotFound, "no kickoff entry for path %q", stored)
		}
		fallback = append(fallback, kickoffs[idx].Repos...)
		file.Prompts.Ticket.Kickoffs = slices.Delete(kickoffs, idx, idx+1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return fallback, nil
}

// SetArchitectSystem sets the architect system prompt path. The resolved
// absolute path is returned.
func SetArchitectSystem(workspacePath, architectKey, promptPath string) (string, error) {
	stored, absolute, err := storeArchitectPath(workspacePath, promptPath)
	if err != nil {
		return "", err
	}
	err = updateArchitectFile(workspacePath, architectKey, func(file *architectFile) error {
		ensureArchitectPromptPaths(file).System = stored
		return nil
	})
	if err != nil {
		return "", err
	}
	return absolute, nil
}

// SetArchitectKickoff sets the architect kickoff prompt path. The resolved
// absolute path is returned.
func SetArchitectKickoff(workspacePath, architectKey, promptPath string) (string, error) {
	stored, absolute, err := storeArchitectPath(workspacePath, promptPath)
	if err != nil {
		return "", err
	}
	err = updateArchitectFile(workspacePath, architectKey, func(file *architectFile) error {
		ensureArchitectPromptPaths(file).Kickoff = stored
		return nil
	})
	if err != nil {
		return "", err
	}
	return absolute, nil
}

func kickoffIndex(kickoffs []ticketKickoffEntry, storedPath string) int {
	for i, kickoff := range kickoffs {
		if kickoff.Path == storedPath {
			return i
		}
	}
	return -1
}

// checkKickoffScope validates a proposed repo scope for a kickoff entry against
// the other entries. skipIdx is the entry being updated (-1 when adding). The
// loader backstop enforces the same rules, but these explicit checks give the
// agent a precise, actionable error.
func checkKickoffScope(file *architectFile, kickoffs []ticketKickoffEntry, skipIdx int, scope []string) error {
	if len(scope) == 0 {
		for i, kickoff := range kickoffs {
			if i == skipIdx {
				continue
			}
			if len(kickoff.Repos) == 0 {
				return mutationErr(MutationErrorConflict, "a default (no-repos) kickoff entry already exists")
			}
		}
		return nil
	}

	claimed := map[string]struct{}{}
	for i, kickoff := range kickoffs {
		if i == skipIdx {
			continue
		}
		for _, repo := range kickoff.Repos {
			claimed[repo] = struct{}{}
		}
	}
	for _, repo := range scope {
		if _, ok := file.Repos[repo]; !ok {
			return mutationErr(MutationErrorValidation, "unknown repo %q", repo)
		}
		if _, dup := claimed[repo]; dup {
			return mutationErr(MutationErrorConflict, "repo %q is already claimed by another kickoff entry", repo)
		}
	}
	return nil
}
