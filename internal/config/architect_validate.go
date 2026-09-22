package config

import (
	"fmt"
	"strings"
)

// validateArchitect enforces the per-architect rules. It is the single ruleset
// shared by the loader (Config.Validate) and by ValidateArchitectConfig, the
// read-only inspection the workspace check and worker launch run, so an
// inspection reaches exactly the verdict the daemon does.
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
	return nil
}

// ValidateArchitectConfig reads the architect's hiveryn.yaml from disk and
// validates it against validateArchitect — the same ruleset the loader applies
// at startup and on every reload.
//
// Reusing that ruleset is the point: a read-only inspection of the config must
// reach the same verdict the daemon does, or the architect would be told its
// config is fine while spawning is broken (or the reverse). It reads the file
// directly rather than the runtime config because the runtime config keeps the
// last valid version when an edit is broken; this is how the broken edit is
// found.
//
// Repo paths are deliberately not stat'ed here. That check lives with the
// caller, because a repo directory that moved after being configured must not
// make the whole config invalid — the daemon still has to load it.
func ValidateArchitectConfig(workspacePath, architectKey string) (ArchitectConfig, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return ArchitectConfig{}, fmt.Errorf("architect %q workspace path is required", architectKey)
	}
	_, file, err := readArchitectFile(workspacePath)
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
