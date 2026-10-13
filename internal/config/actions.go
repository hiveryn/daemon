package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sd "github.com/hiveryn/shared/domain"
	"gopkg.in/yaml.v3"
)

// actionsFileName is the global Action registry beside config.yaml.
const actionsFileName = "actions.yaml"

// ActionRegistration is one actions.yaml entry: the existing checkout of an
// Action and the machine it lives on (empty means local). Registration never
// clones or synchronizes the checkout; Path is a path on Machine — expanded
// to absolute when local, an absolute POSIX path when remote.
type ActionRegistration struct {
	Path    string
	Machine string
}

// ActionsRegistryPath is the registry file read from configDir.
func ActionsRegistryPath(configDir string) string {
	return filepath.Join(configDir, actionsFileName)
}

// loadActions reads actions.yaml: a map of Action name to a checkout path, or
// to a {path, machine} object — the same shapes as hiveryn.yaml repos. A
// missing file is an empty registry.
func loadActions(file string) (map[string]ActionRegistration, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]ActionRegistration{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", file, err)
	}
	var raw map[string]repoFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]ActionRegistration{}, nil
		}
		return nil, fmt.Errorf("decode YAML %q: %w", file, err)
	}
	out := make(map[string]ActionRegistration, len(raw))
	for name, entry := range raw {
		if !sd.ValidActionName(name) {
			return nil, fmt.Errorf("actions.yaml: %q is not a valid action name: use lowercase letters, digits, '.', '_' or '-', starting with a letter or digit (at most 64 characters)", name)
		}
		if entry.Machine != "" {
			resolved, err := remoteRepoPath(entry.Path)
			if err != nil {
				return nil, fmt.Errorf("actions.yaml: action %q: %w", name, err)
			}
			out[name] = ActionRegistration{Path: resolved, Machine: entry.Machine}
			continue
		}
		if strings.TrimSpace(entry.Path) == "" {
			return nil, fmt.Errorf("actions.yaml: action %q: path is required", name)
		}
		expanded, err := expandHomePath(entry.Path)
		if err != nil {
			return nil, fmt.Errorf("actions.yaml: action %q: %w", name, err)
		}
		out[name] = ActionRegistration{Path: expanded}
	}
	return out, nil
}

func (c Config) validateActions() error {
	for _, name := range sortedKeys(c.Actions) {
		machine := c.Actions[name].Machine
		if machine == "" {
			continue
		}
		if _, ok := c.Machines[machine]; !ok {
			return fmt.Errorf("actions.%s.machine: unknown machine %q (omit machine for a local action)", name, machine)
		}
	}
	return nil
}

// BootstrapActionRegistry is the transition from directory discovery to the
// registry. When actionsDir holds Action checkouts and configDir has no
// actions.yaml yet, it writes one registering every non-hidden directory
// there as a local Action, so no existing Action disappears; it returns the
// registered names. With an existing registry, or nothing to register, it does
// nothing. The file is written atomically and never overwritten.
func BootstrapActionRegistry(configDir, actionsDir string) ([]string, error) {
	registry := ActionsRegistryPath(configDir)
	if _, err := os.Lstat(registry); err == nil {
		return nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect %s: %w", registry, err)
	}
	entries, err := os.ReadDir(actionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read actions directory %s: %w", actionsDir, err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := os.Stat(filepath.Join(actionsDir, entry.Name()))
		if err != nil || !info.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# Hiveryn Action registry: name -> existing checkout, optionally on a\n")
	b.WriteString("# machines.yaml machine (omitted means local; remote paths are absolute).\n")
	b.WriteString("#   my-action: ~/.hiveryn/actions/my-action\n")
	b.WriteString("#   remote-action: {path: /home/me/actions/remote-action, machine: buildbox}\n")
	b.WriteString("# Created from the directories found in " + actionsDir + ".\n")
	registered := names[:0:0]
	for _, name := range names {
		value, err := yaml.Marshal(filepath.Join(actionsDir, name))
		if err != nil {
			return nil, fmt.Errorf("encode path of action %s: %w", name, err)
		}
		// A directory whose name cannot be an Action name is left visible as
		// a comment: registering it would make the whole registry fail to
		// load.
		if !sd.ValidActionName(name) {
			b.WriteString(fmt.Sprintf("# not registered: %q is not a valid action name (lowercase letters, digits, '.', '_' or '-'); rename it and add it here\n", name))
			continue
		}
		b.WriteString(name + ": " + strings.TrimSpace(string(value)) + "\n")
		registered = append(registered, name)
	}
	tmp, err := os.CreateTemp(configDir, ".actions.yaml.*")
	if err != nil {
		return nil, fmt.Errorf("create action registry: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write action registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("write action registry: %w", err)
	}
	// Link, not rename: an actions.yaml created meanwhile is never replaced.
	if err := os.Link(tmp.Name(), registry); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("install action registry %s: %w", registry, err)
	}
	return registered, nil
}

// ActionPackagesConfig is config.yaml's optional action_packages section:
// the bounds of a delivered artifact package and how long frozen packages and
// caller copies are kept after the execution completes. It is daemon core,
// read at startup.
type ActionPackagesConfig struct {
	MaxBytes      int64 `yaml:"max_bytes,omitempty"`
	MaxFiles      int   `yaml:"max_files,omitempty"`
	RetentionDays int   `yaml:"retention_days,omitempty"`
}

const (
	DefaultActionPackageMaxBytes      = 100 << 20
	DefaultActionPackageMaxFiles      = 10000
	DefaultActionPackageRetentionDays = 30
)

// ActionPackageLimits returns the effective bounds, defaults applied.
func (c Config) ActionPackageLimits() (maxBytes int64, maxFiles int, retention time.Duration) {
	p := ActionPackagesConfig{}
	if c.ActionPackages != nil {
		p = *c.ActionPackages
	}
	maxBytes, maxFiles, days := p.MaxBytes, p.MaxFiles, p.RetentionDays
	if maxBytes == 0 {
		maxBytes = DefaultActionPackageMaxBytes
	}
	if maxFiles == 0 {
		maxFiles = DefaultActionPackageMaxFiles
	}
	if days == 0 {
		days = DefaultActionPackageRetentionDays
	}
	return maxBytes, maxFiles, time.Duration(days) * 24 * time.Hour
}

func (p *ActionPackagesConfig) validate() error {
	if p == nil {
		return nil
	}
	if p.MaxBytes < 0 || p.MaxFiles < 0 || p.RetentionDays < 0 {
		return fmt.Errorf("action_packages: max_bytes, max_files and retention_days must be positive (omit one for its default)")
	}
	return nil
}

func (p *ActionPackagesConfig) clone() *ActionPackagesConfig {
	if p == nil {
		return nil
	}
	out := *p
	return &out
}

func cloneActions(src map[string]ActionRegistration) map[string]ActionRegistration {
	if src == nil {
		return map[string]ActionRegistration{}
	}
	return maps.Clone(src)
}
