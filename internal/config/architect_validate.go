package config

import (
	"fmt"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
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
		if machine := architect.RepoMachines[repoKey]; machine != "" && !machineName.MatchString(machine) {
			return fmt.Errorf("repos.%s.machine is invalid: %q", repoKey, machine)
		}
		if strings.TrimSpace(repoKey) == "" {
			return fmt.Errorf("architects.%s.repos keys must not be blank", key)
		}
		// Every repo key is a context reference prefix (<repoKey>:<path>), and
		// architect:<path> addresses the workspace itself.
		if strings.EqualFold(repoKey, domain.ArchitectReferencePrefix) {
			return fmt.Errorf("architects.%s.repos.%s: %q is reserved for architect:<path> workspace references; rename the repo key", key, repoKey, domain.ArchitectReferencePrefix)
		}
		if !domain.ValidRepoKey(repoKey) {
			return fmt.Errorf("architects.%s.repos.%s: repo keys must start with a letter or digit and use only letters, digits, '.', '_' or '-' so they can prefix <repoKey>:<path> references", key, repoKey)
		}
		if strings.TrimSpace(repoPath) == "" {
			return fmt.Errorf("architects.%s.repos.%s is required", key, repoKey)
		}
	}
	for name, companion := range architect.Companions {
		// The name addresses the companion in sendMessageToCompanion and
		// requestCompanionShutdown; it follows the repo-key grammar.
		if !domain.ValidRepoKey(name) {
			return fmt.Errorf("architects.%s.companions.%s: companion names must start with a letter or digit and use only letters, digits, '.', '_' or '-'", key, name)
		}
		if companion.Repo == "" {
			return fmt.Errorf("architects.%s.companions.%s.repo is required: name a key of repos", key, name)
		}
		if _, ok := architect.Repos[companion.Repo]; !ok {
			return fmt.Errorf("architects.%s.companions.%s.repo %q is not a key of repos", key, name, companion.Repo)
		}
		if companion.Variant == "" {
			return fmt.Errorf("architects.%s.companions.%s.variant is required: name a claude or codex variant from variants.yaml for the repo's machine", key, name)
		}
	}
	return nil
}

// CompanionVariant resolves a companion's variant for the machine its
// repository is on (empty: local): it must exist, be a claude or codex variant
// (OpenCode companions are unsupported) and be configured for that machine.
// The error names the companion and what to fix.
func (c Config) CompanionVariant(companion, variantName, machine string) (VariantConfig, error) {
	variant, err := c.VariantForMachine(variantName, machine, "companion "+companion)
	if err != nil {
		return VariantConfig{}, err
	}
	switch variant.Agent {
	case "claude", "codex":
	default:
		return VariantConfig{}, &domain.ValidationError{Field: "variant", Message: fmt.Sprintf("companion %s: variant %q uses agent %q; companions support claude and codex variants only", companion, variantName, variant.Agent)}
	}
	return variant, nil
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
