package config

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"sort"
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
	for name, v := range c.Variants {
		if v.Machine == "" {
			continue
		}
		if _, ok := c.Machines[v.Machine]; !ok {
			return fmt.Errorf("variants.%s.machine: unknown machine %q (omit machine for a local variant)", name, v.Machine)
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

// MachineLabel names an execution location in messages: "local" or
// "machine <key>".
func MachineLabel(machine string) string {
	if machine == "" {
		return "local"
	}
	return "machine " + machine
}

// VariantsForMachine lists, sorted, the variants that may run on machine
// (empty means local): exactly those assigned to it. An unassigned variant is
// local only and is never reused on a remote machine.
func (c Config) VariantsForMachine(machine string) []string {
	names := []string{}
	for name, v := range c.Variants {
		if v.Machine == machine {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// VariantForMachine resolves a launch's variant for its execution location.
// A missing, unknown or other-machine variant is a ValidationError naming the
// target location and the eligible choices; subject names what runs ("worker",
// "Action", "session").
func (c Config) VariantForMachine(name, machine, subject string) (VariantConfig, error) {
	target := MachineLabel(machine)
	eligible := c.VariantsForMachine(machine)
	choices := make([]string, 0, len(eligible))
	for _, n := range eligible {
		choices = append(choices, fmt.Sprintf("%s (%s)", n, c.Variants[n].Agent))
	}
	available := "Eligible variants for " + target + ": " + strings.Join(choices, ", ")
	if len(eligible) == 0 {
		hint := "add a variant without machine to variants.yaml"
		if machine != "" {
			hint = fmt.Sprintf("add a variant with machine: %s to variants.yaml", machine)
		}
		available = fmt.Sprintf("No agent variant is configured for %s; %s", target, hint)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return VariantConfig{}, &sd.ValidationError{Field: "variant", Message: fmt.Sprintf("is required and has no default; choose the variant that runs this %s on %s. %s", subject, target, available)}
	}
	v, ok := c.Variants[name]
	if !ok {
		return VariantConfig{}, &sd.ValidationError{Field: "variant", Message: fmt.Sprintf("%q is not a configured agent variant. This %s runs on %s. %s", name, subject, target, available)}
	}
	if v.Machine != machine {
		return VariantConfig{}, &sd.ValidationError{Field: "variant", Message: fmt.Sprintf("%q is configured for %s, but this %s runs on %s. %s", name, MachineLabel(v.Machine), subject, target, available)}
	}
	return v, nil
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
