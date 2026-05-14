package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPort        = 4201
	DefaultBindAddress = "127.0.0.1"
	DefaultLogLevel    = "info"
	configDirName      = ".hiveryn"
	configFileName     = "config.yaml"
	variantsFileName   = "variants.yaml"
	architectsFileName = "architects.yaml"
	tabsFileName       = "tabs.yaml"
)

type Config struct {
	Port        int                        `yaml:"port"`
	BindAddress string                     `yaml:"bind_address"`
	LogLevel    string                     `yaml:"log_level"`
	Variants    map[string]VariantConfig   `yaml:"-"`
	Architects  map[string]ArchitectConfig `yaml:"-"`
	Tabs        map[string][]TabEntry      `yaml:"-"`
}

type VariantConfig struct {
	Agent string            `yaml:"agent"`
	Args  []string          `yaml:"args"`
	Env   map[string]string `yaml:"env"`
}

type ArchitectConfig struct {
	Path  string            `yaml:"path"`
	Group string            `yaml:"group"`
	Repos map[string]string `yaml:"repos"`
}

type TabEntry struct {
	Type    string `yaml:"type"`
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
}

func Default() Config {
	return Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs:        map[string][]TabEntry{},
	}
}

func DefaultPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(homeDir, configDirName, configFileName), nil
}

func Load(path string) (Config, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return Config{}, err
		}
	}

	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := cfg.Save(path); err != nil {
				return Config{}, err
			}
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode YAML config %q: %w", path, err)
	}

	configDir := filepath.Dir(path)

	if err := loadOptionalFile(filepath.Join(configDir, variantsFileName), &cfg.Variants); err != nil {
		return Config{}, fmt.Errorf("load variants: %w", err)
	}

	if err := loadOptionalFile(filepath.Join(configDir, architectsFileName), &cfg.Architects); err != nil {
		return Config{}, fmt.Errorf("load architects: %w", err)
	}

	if err := loadOptionalFile(filepath.Join(configDir, tabsFileName), &cfg.Tabs); err != nil {
		return Config{}, fmt.Errorf("load tabs: %w", err)
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func loadOptionalFile(path string, target interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode YAML %q: %w", path, err)
	}
	return nil
}

type coreConfig struct {
	Port        int    `yaml:"port"`
	BindAddress string `yaml:"bind_address"`
	LogLevel    string `yaml:"log_level"`
}

func (c Config) Save(path string) error {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return err
		}
	}

	c.normalize()
	if err := c.Validate(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	core := coreConfig{
		Port:        c.Port,
		BindAddress: c.BindAddress,
		LogLevel:    c.LogLevel,
	}

	data, err := yaml.Marshal(core)
	if err != nil {
		return fmt.Errorf("marshal config %q: %w", path, err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config %q: %w", path, err)
	}

	return nil
}

func (c Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	if !isLoopbackHost(c.BindAddress) {
		return fmt.Errorf("bind_address must stay on localhost")
	}

	if strings.TrimSpace(c.LogLevel) == "" {
		return fmt.Errorf("log_level is required")
	}

	variantNames := sortedKeys(c.Variants)
	for _, name := range variantNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("variants keys must not be blank")
		}
		variant := c.Variants[name]
		if strings.TrimSpace(variant.Agent) == "" {
			return fmt.Errorf("variants.%s.agent is required", name)
		}
		for key := range variant.Env {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("variants.%s.env keys must not be blank", name)
			}
		}
	}

	architectKeys := sortedKeys(c.Architects)
	for _, key := range architectKeys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("architects keys must not be blank")
		}
		architect := c.Architects[key]
		if strings.TrimSpace(architect.Path) == "" {
			return fmt.Errorf("architects.%s.path is required", key)
		}
		if strings.TrimSpace(architect.Group) == "" {
			return fmt.Errorf("architects.%s.group is required", key)
		}
		for repoKey, repoPath := range architect.Repos {
			if strings.TrimSpace(repoKey) == "" {
				return fmt.Errorf("architects.%s.repos keys must not be blank", key)
			}
			if strings.TrimSpace(repoPath) == "" {
				return fmt.Errorf("architects.%s.repos.%s is required", key, repoKey)
			}
		}
	}

	sessionTypes := sortedKeys(c.Tabs)
	for _, sessionType := range sessionTypes {
		if strings.TrimSpace(sessionType) == "" {
			return fmt.Errorf("tabs keys must not be blank")
		}
		terminalNames := map[string]struct{}{}
		for i, entry := range c.Tabs[sessionType] {
			validTypes := map[string]bool{"kanban": true, "event-log": true, "terminal": true}
			if !validTypes[entry.Type] {
				return fmt.Errorf("tabs.%s[%d].type %q is invalid; must be kanban, event-log, or terminal", sessionType, i, entry.Type)
			}
			if entry.Type == "terminal" {
				if strings.TrimSpace(entry.Name) == "" {
					return fmt.Errorf("tabs.%s[%d].name is required for terminal entries", sessionType, i)
				}
				if _, exists := terminalNames[entry.Name]; exists {
					return fmt.Errorf("tabs.%s: duplicate terminal name %q", sessionType, entry.Name)
				}
				terminalNames[entry.Name] = struct{}{}
			} else {
				if entry.Name != "" {
					return fmt.Errorf("tabs.%s[%d].name is not allowed for %q entries", sessionType, i, entry.Type)
				}
				if entry.Command != "" {
					return fmt.Errorf("tabs.%s[%d].command is not allowed for %q entries", sessionType, i, entry.Type)
				}
			}
		}
	}

	return nil
}

func (c *Config) normalize() {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if strings.TrimSpace(c.BindAddress) == "" {
		c.BindAddress = DefaultBindAddress
	}
	if strings.TrimSpace(c.LogLevel) == "" {
		c.LogLevel = DefaultLogLevel
	}
	if c.Variants == nil {
		c.Variants = map[string]VariantConfig{}
	}
	for name, variant := range c.Variants {
		if variant.Args == nil {
			variant.Args = []string{}
		}
		if variant.Env == nil {
			variant.Env = map[string]string{}
		}
		c.Variants[name] = variant
	}
	if c.Architects == nil {
		c.Architects = map[string]ArchitectConfig{}
	}
	for key, architect := range c.Architects {
		if architect.Repos == nil {
			architect.Repos = map[string]string{}
		}
		c.Architects[key] = architect
	}
	if c.Tabs == nil {
		c.Tabs = map[string][]TabEntry{}
	}
	for sessionType, entries := range c.Tabs {
		if entries == nil {
			c.Tabs[sessionType] = []TabEntry{}
		}
	}
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
