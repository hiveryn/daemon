package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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

// validateArchitectRepoPaths is a write-time-only backstop, deliberately kept
// out of validateArchitect (the loader's shared ruleset): if a configured
// repo directory moves or is deleted after being written, the whole config
// must still load at daemon startup. A bad repo path only blocks a new
// mutation, giving the architect agent an actionable error to fix and retry.
func validateArchitectRepoPaths(key string, architect ArchitectConfig) error {
	for _, repoKey := range sortedKeys(architect.Repos) {
		repoPath := architect.Repos[repoKey]
		info, err := os.Stat(repoPath)
		if err != nil {
			if os.IsNotExist(err) {
				return mutationErr(MutationErrorValidation,
					"architects.%s.repos.%s: path does not exist: %s (correct the path, or run `git init` there once it exists)",
					key, repoKey, repoPath)
			}
			return fmt.Errorf("stat architects.%s.repos.%s path %q: %w", key, repoKey, repoPath, err)
		}
		if !info.IsDir() {
			return mutationErr(MutationErrorValidation,
				"architects.%s.repos.%s: path is not a directory: %s", key, repoKey, repoPath)
		}
		gitInfo, err := os.Stat(filepath.Join(repoPath, ".git"))
		if err != nil || !gitInfo.IsDir() {
			return mutationErr(MutationErrorValidation,
				"architects.%s.repos.%s: no .git directory found at %s — run `git init` in that directory, or correct the repo path",
				key, repoKey, repoPath)
		}
	}
	return nil
}

// ArchitectConfigDoc is the declarative, whole-document shape of an architect's
// hiveryn.yaml config, identical in ReadArchitectConfig's result and
// ReplaceArchitectConfig's input so an agent can read → mutate → write back
// without reshaping. Paths round-trip verbatim. The architect `name` is not
// part of this document — it is preserved across a replace.
type ArchitectConfigDoc struct {
	Repos   map[string]string
	Prompts ArchitectPromptsDoc
}

type ArchitectPromptsDoc struct {
	Architect ArchitectPromptPathsDoc
	Ticket    []TicketKickoffDoc
}

type ArchitectPromptPathsDoc struct {
	System  string
	Kickoff string
}

type TicketKickoffDoc struct {
	Path  string
	Repos []string
}

// ArchitectConfigView bundles the round-trippable document, the resolved
// (absolute-path) config, and the opaque version token from a read/replace.
type ArchitectConfigView struct {
	Config   ArchitectConfigDoc
	Resolved ArchitectConfig
	Version  string
}

// hashConfig returns the opaque version token for a set of hiveryn.yaml bytes.
// It is computed identically on read and on the write-side compare so the token
// detects any byte change — our own marshal, an editor, or another session.
func hashConfig(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readArchitectFile reads and parses the architect's hiveryn.yaml, returning the
// raw bytes, their version token, and the decoded file. A missing file is a
// MutationErrorNotFound so the agent gets a clean, actionable error.
func readArchitectFile(workspacePath string) (raw []byte, version string, file architectFile, err error) {
	if strings.TrimSpace(workspacePath) == "" {
		return nil, "", architectFile{}, mutationErr(MutationErrorValidation, "architect workspace path is required")
	}
	filePath := filepath.Join(workspacePath, architectConfigFileName)
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", architectFile{}, mutationErr(MutationErrorNotFound, "hiveryn.yaml not found at %q", filePath)
		}
		return nil, "", architectFile{}, fmt.Errorf("read %q: %w", filePath, err)
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, "", architectFile{}, fmt.Errorf("decode YAML %q: %w", filePath, err)
	}
	return data, hashConfig(data), file, nil
}

// writeArchitectFile validates file against the loader ruleset, then atomically
// replaces hiveryn.yaml. Validation parity guarantees a write that would fail to
// load is refused before the file is touched, so a bad edit can never brick
// session spawning. Returns the resolved config and the new version token.
func writeArchitectFile(workspacePath, architectKey string, file architectFile) (ArchitectConfig, string, error) {
	resolved, err := architectConfigFromFile(architectKey, workspacePath, file)
	if err != nil {
		return ArchitectConfig{}, "", err
	}
	if err := validateArchitect(architectKey, resolved); err != nil {
		return ArchitectConfig{}, "", mutationErr(MutationErrorValidation, "%s", err.Error())
	}
	if err := validateArchitectRepoPaths(architectKey, resolved); err != nil {
		return ArchitectConfig{}, "", err
	}

	out, err := yaml.Marshal(file)
	if err != nil {
		return ArchitectConfig{}, "", fmt.Errorf("marshal hiveryn.yaml: %w", err)
	}
	filePath := filepath.Join(workspacePath, architectConfigFileName)
	if err := atomicWriteFile(filePath, out, 0o600); err != nil {
		return ArchitectConfig{}, "", err
	}
	return resolved, hashConfig(out), nil
}

// atomicWriteFile writes data to a temp file in the same directory and renames
// it over path, so a crash mid-write can never leave a truncated hiveryn.yaml
// that would fail to load and brick spawning.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hiveryn-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %q: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %q to %q: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}

// ReadArchitectConfig reads the architect's hiveryn.yaml and returns the
// round-trippable document, the resolved (absolute-path) config, and the opaque
// version token to pass back to ReplaceArchitectConfig.
func ReadArchitectConfig(workspacePath, architectKey string) (ArchitectConfigView, error) {
	_, version, file, err := readArchitectFile(workspacePath)
	if err != nil {
		return ArchitectConfigView{}, err
	}
	resolved, err := architectConfigFromFile(architectKey, workspacePath, file)
	if err != nil {
		return ArchitectConfigView{}, err
	}
	return ArchitectConfigView{
		Config:   docFromFile(file),
		Resolved: resolved,
		Version:  version,
	}, nil
}

// ReplaceArchitectConfig replaces the whole architect config with doc, guarded
// by the version token from a prior ReadArchitectConfig (declarative PUT — any
// field omitted from doc is dropped). The architect `name` is preserved from
// disk. Paths are stored verbatim. On success it returns the fresh view
// (read-after-write) so chained edits can reuse the new version without
// re-reading.
func ReplaceArchitectConfig(workspacePath, architectKey, version string, doc ArchitectConfigDoc) (ArchitectConfigView, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return ArchitectConfigView{}, mutationErr(MutationErrorValidation, "version is required; read the config with readArchitectConfig before updating")
	}

	_, current, file, err := readArchitectFile(workspacePath)
	if err != nil {
		return ArchitectConfigView{}, err
	}
	if !strings.EqualFold(version, current) {
		return ArchitectConfigView{}, mutationErr(MutationErrorConflict, "config changed since you read it (version mismatch); re-read with readArchitectConfig and retry")
	}

	// Declarative replace: keep name, overwrite repos + prompts from the doc.
	file.Repos = reposFromDoc(doc.Repos)
	file.Prompts = promptsFromDoc(doc.Prompts)

	resolved, newVersion, err := writeArchitectFile(workspacePath, architectKey, file)
	if err != nil {
		return ArchitectConfigView{}, err
	}
	return ArchitectConfigView{
		Config:   docFromFile(file),
		Resolved: resolved,
		Version:  newVersion,
	}, nil
}

// docFromFile converts a parsed architectFile into the verbatim, round-trippable
// document. Repos is always a (possibly empty) map so the agent has something to
// add to.
func docFromFile(file architectFile) ArchitectConfigDoc {
	doc := ArchitectConfigDoc{Repos: map[string]string{}}
	maps.Copy(doc.Repos, file.Repos)
	if file.Prompts != nil {
		if file.Prompts.Architect != nil {
			doc.Prompts.Architect.System = file.Prompts.Architect.System
			doc.Prompts.Architect.Kickoff = file.Prompts.Architect.Kickoff
		}
		if file.Prompts.Ticket != nil {
			for _, e := range file.Prompts.Ticket.Kickoffs {
				doc.Prompts.Ticket = append(doc.Prompts.Ticket, TicketKickoffDoc{
					Path:  e.Path,
					Repos: append([]string(nil), e.Repos...),
				})
			}
		}
	}
	return doc
}

func reposFromDoc(repos map[string]string) map[string]string {
	if len(repos) == 0 {
		return nil
	}
	out := make(map[string]string, len(repos))
	maps.Copy(out, repos)
	return out
}

func promptsFromDoc(p ArchitectPromptsDoc) *architectPrompts {
	var architect *architectPromptPaths
	if strings.TrimSpace(p.Architect.System) != "" || strings.TrimSpace(p.Architect.Kickoff) != "" {
		architect = &architectPromptPaths{System: p.Architect.System, Kickoff: p.Architect.Kickoff}
	}

	var ticket *ticketPrompts
	if kickoffs := kickoffsFromDoc(p.Ticket); len(kickoffs) > 0 {
		ticket = &ticketPrompts{Kickoffs: kickoffs}
	}

	if architect == nil && ticket == nil {
		return nil
	}
	return &architectPrompts{Architect: architect, Ticket: ticket}
}

func kickoffsFromDoc(entries []TicketKickoffDoc) []ticketKickoffEntry {
	out := make([]ticketKickoffEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, ticketKickoffEntry{Path: e.Path, Repos: trimRepoScope(e.Repos)})
	}
	return out
}

// trimRepoScope drops blank repo keys and returns nil for an empty scope so the
// default (no-repos) kickoff entry marshals cleanly (no `repos:` key).
func trimRepoScope(repos []string) []string {
	scope := make([]string, 0, len(repos))
	for _, repo := range repos {
		if trimmed := strings.TrimSpace(repo); trimmed != "" {
			scope = append(scope, trimmed)
		}
	}
	if len(scope) == 0 {
		return nil
	}
	return scope
}

// ValidateArchitectConfig reads the architect's hiveryn.yaml and validates it
// against validateArchitect — the same ruleset the loader applies at startup
// and the mutation layer applies before persisting a write.
//
// Reusing that ruleset is the point: a read-only inspection of the config must
// reach the same verdict the daemon does, or the architect would be told its
// config is fine while spawning is broken (or the reverse).
//
// Repo paths are deliberately not stat'ed here. That check lives with the
// caller, because a repo directory that moved after being configured must not
// make the whole config invalid — the daemon still has to load it.
func ValidateArchitectConfig(workspacePath, architectKey string) (ArchitectConfig, error) {
	_, _, file, err := readArchitectFile(workspacePath)
	if err != nil {
		return ArchitectConfig{}, err
	}
	resolved, err := architectConfigFromFile(architectKey, workspacePath, file)
	if err != nil {
		return ArchitectConfig{}, err
	}
	if err := validateArchitect(architectKey, resolved); err != nil {
		return ArchitectConfig{}, err
	}
	return resolved, nil
}
