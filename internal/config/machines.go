package config

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"strings"

	sd "github.com/hiveryn/shared/domain"
	"gopkg.in/yaml.v3"
)

// MachineConfig refers to an SSH config alias. SSH owns credentials and routing.
type MachineConfig struct {
	SSH string `yaml:"ssh"`
}

type repoFile struct {
	Path    string `yaml:"path"`
	Machine string `yaml:"machine,omitempty"`
}

func (r *repoFile) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		r.Path = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("repo must be a path string or a path/machine object")
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		if seen[k] {
			return fmt.Errorf("duplicate repo field %q", k)
		}
		seen[k] = true
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			return fmt.Errorf("repo %s must be a string", k)
		}
		switch k {
		case "path":
			r.Path = v.Value
		case "machine":
			r.Machine = v.Value
		default:
			return fmt.Errorf("unknown repo field %q", k)
		}
	}
	return nil
}

var machineName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (c Config) validateMachines() error {
	for key, m := range c.Machines {
		if !machineName.MatchString(key) || !machineName.MatchString(m.SSH) {
			return fmt.Errorf("machines.%s: key and ssh must be nonblank SSH aliases (letters, digits, '.', '_' or '-')", key)
		}
	}
	for key, a := range c.Architects {
		for repo, machine := range a.RepoMachines {
			if machine == "" {
				continue
			}
			if _, ok := c.Machines[machine]; !ok {
				return fmt.Errorf("architects.%s.repos.%s: unknown machine %q", key, repo, machine)
			}
		}
	}
	return nil
}

func cloneMachines(src map[string]MachineConfig) map[string]MachineConfig { return maps.Clone(src) }

// ScopeMachine validates location using current configuration. An empty result
// means local; local and a named SSH machine are distinct execution locations.
func (c Config) ScopeMachine(a ArchitectConfig, primary string, additional []string) (string, error) {
	machine := a.RepoMachines[primary]
	for _, key := range append([]string{primary}, additional...) {
		if _, ok := a.Repos[key]; !ok {
			return "", fmt.Errorf("repo %q is not configured", key)
		}
		m := a.RepoMachines[key]
		if m != "" {
			if _, ok := c.Machines[m]; !ok {
				return "", fmt.Errorf("repo %q references unknown machine %q", key, m)
			}
		}
		if m != machine {
			return "", fmt.Errorf("writable repos must use one machine: repo %q uses %q, primary %q uses %q (empty means local)", key, m, primary, machine)
		}
	}
	return machine, nil
}

func remoteRepoPath(p string) (string, error) {
	if !path.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n") {
		return "", fmt.Errorf("remote path must be an absolute POSIX path; use the remote account's full path, not ~")
	}
	return path.Clean(p), nil
}

func loadMachines(file string) (map[string]MachineConfig, error) {
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return map[string]MachineConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var machines map[string]MachineConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&machines); err != nil {
		if err == io.EOF {
			return map[string]MachineConfig{}, nil
		}
		return nil, fmt.Errorf("decode machines.yaml: %w", err)
	}
	return machines, nil
}

// WritableConfig refuses to resolve new mutation scope from a stale fallback.
// Read APIs keep using Current so a broken file remains visible and repairable.
func WritableConfig(cfg Config, source Source) (Config, error) {
	if source == nil {
		return cfg, nil
	}
	current, err := source.Current()
	if err != nil {
		return Config{}, err
	}
	if status := source.LoadStatus(); status.Error != "" {
		return Config{}, &sd.ValidationError{Field: "config", Message: "repair configuration before changing repository scope or launching workers: " + status.Error}
	}
	return current, nil
}
