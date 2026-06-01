package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPort                      = 4201
	DefaultBindAddress               = "127.0.0.1"
	DefaultLogLevel                  = "info"
	DefaultDesktopHealthPollInterval = "1s"
	DefaultConclusionApprovalTimeout = 20
	configFileName                   = "config.yaml"
	variantsFileName                 = "variants.yaml"
	architectsFileName               = "architects.yaml"
	tabsFileName                     = "tabs.yaml"
	shortcutsFileName                = "shortcuts.yaml"
)

type Config struct {
	Port                      int                          `yaml:"port"`
	BindAddress               string                       `yaml:"bind_address"`
	LogLevel                  string                       `yaml:"log_level"`
	Shell                     string                       `yaml:"shell,omitempty"`
	DesktopHealthPollInterval string                       `yaml:"desktop_health_poll_interval,omitempty"`
	ConclusionApprovalTimeout int                          `yaml:"conclusion_approval_timeout,omitempty"`
	Variants                  map[string]VariantConfig     `yaml:"-"`
	Architects                map[string]ArchitectConfig   `yaml:"-"`
	Tabs                      map[string][]TabEntry        `yaml:"-"`
	Shortcuts                 map[string]map[string]string `yaml:"-"`
}

type Source interface {
	Current() (Config, error)
}

type staticSource struct {
	cfg Config
}

type architectsReloadingSource struct {
	path string
	base Config
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
	Command string `yaml:"command"`
}

func defaultShortcuts() map[string]map[string]string {
	return map[string]map[string]string{
		"global": {
			"focus-left":    "Cmd+Shift+h",
			"focus-right":   "Cmd+Shift+l",
			"focus-down":    "Cmd+Shift+j",
			"focus-up":      "Cmd+Shift+k",
			"focus-main":    "Cmd+1",
			"first-session": "Cmd+Shift+0",
			"prev-session":  "Cmd+Shift+[",
			"next-session":  "Cmd+Shift+]",
			"close-tab":     "Cmd+w",
			"new-terminal":  "Cmd+t",
			"maximize-pane": "Cmd+m",
			"quit":          "q",
		},
		"kanban": {
			"left":    "h",
			"right":   "l",
			"down":    "j",
			"up":      "k",
			"open":    "o",
			"spawn":   "s",
			"refresh": "r",
		},
		"event-log": {
			"down": "j",
			"up":   "k",
			"open": "o",
			"copy": "c",
		},
	}
}

func Default() Config {
	return Config{
		Port:                      DefaultPort,
		BindAddress:               DefaultBindAddress,
		LogLevel:                  DefaultLogLevel,
		DesktopHealthPollInterval: DefaultDesktopHealthPollInterval,
		ConclusionApprovalTimeout: DefaultConclusionApprovalTimeout,
		Variants:                  map[string]VariantConfig{},
		Architects:                map[string]ArchitectConfig{},
		Tabs:                      map[string][]TabEntry{},
		Shortcuts:                 defaultShortcuts(),
	}
}

func DefaultPath() (string, error) {
	runtime, err := ResolveRuntime("", "")
	if err != nil {
		return "", err
	}
	return runtime.ConfigPath, nil
}

func StaticSource(cfg Config) Source {
	return staticSource{cfg: cfg.Clone()}
}

func NewArchitectsReloadingSource(path string, base Config) (Source, error) {
	resolvedPath, err := resolvePath(path)
	if err != nil {
		return nil, err
	}

	return architectsReloadingSource{
		path: resolvedPath,
		base: base.Clone(),
	}, nil
}

func Load(path string) (Config, error) {
	var err error
	path, err = resolvePath(path)
	if err != nil {
		return Config{}, err
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

	if err := loadOptionalFile(filepath.Join(configDir, shortcutsFileName), &cfg.Shortcuts); err != nil {
		return Config{}, fmt.Errorf("load shortcuts: %w", err)
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Clone() Config {
	cloned := c
	cloned.Variants = cloneVariantConfigs(c.Variants)
	cloned.Architects = cloneArchitectConfigs(c.Architects)
	cloned.Tabs = cloneTabs(c.Tabs)
	cloned.Shortcuts = cloneShortcuts(c.Shortcuts)
	return cloned
}

func (s staticSource) Current() (Config, error) {
	return s.cfg.Clone(), nil
}

func (s architectsReloadingSource) Current() (Config, error) {
	cfg := s.base.Clone()
	architects, err := loadArchitects(filepath.Dir(s.path))
	if err != nil {
		return Config{}, fmt.Errorf("reload architects: %w", err)
	}
	cfg.Architects = architects
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config after architects reload: %w", err)
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

func loadArchitects(configDir string) (map[string]ArchitectConfig, error) {
	architects := map[string]ArchitectConfig{}
	if err := loadOptionalFile(filepath.Join(configDir, architectsFileName), &architects); err != nil {
		return nil, err
	}
	return architects, nil
}

type coreConfig struct {
	Port                      int    `yaml:"port"`
	BindAddress               string `yaml:"bind_address"`
	LogLevel                  string `yaml:"log_level"`
	Shell                     string `yaml:"shell,omitempty"`
	DesktopHealthPollInterval string `yaml:"desktop_health_poll_interval,omitempty"`
	ConclusionApprovalTimeout int    `yaml:"conclusion_approval_timeout,omitempty"`
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
		Port:                      c.Port,
		BindAddress:               c.BindAddress,
		LogLevel:                  c.LogLevel,
		Shell:                     c.Shell,
		DesktopHealthPollInterval: c.DesktopHealthPollInterval,
		ConclusionApprovalTimeout: c.ConclusionApprovalTimeout,
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

	rawInterval := strings.TrimSpace(c.DesktopHealthPollInterval)
	if rawInterval == "" {
		rawInterval = DefaultDesktopHealthPollInterval
	}

	interval, err := time.ParseDuration(rawInterval)
	if err != nil {
		return fmt.Errorf("desktop_health_poll_interval must be a valid duration: %w", err)
	}
	if interval <= 0 {
		return fmt.Errorf("desktop_health_poll_interval must be greater than 0")
	}

	if c.ConclusionApprovalTimeout < 0 {
		return fmt.Errorf("conclusion_approval_timeout must be >= 0")
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
		for i, entry := range c.Tabs[sessionType] {
			validTypes := map[string]bool{"kanban": true, "event-log": true, "terminal": true, "ticket": true}
			if !validTypes[entry.Type] {
				return fmt.Errorf("tabs.%s[%d].type %q is invalid; must be kanban, event-log, ticket, or terminal", sessionType, i, entry.Type)
			}
			if entry.Type != "terminal" {
				if entry.Command != "" {
					return fmt.Errorf("tabs.%s[%d].command is not allowed for %q entries", sessionType, i, entry.Type)
				}
			}
		}
	}

	return nil
}

func (c *Config) normalize() {
	c.Shell = strings.TrimSpace(c.Shell)
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if strings.TrimSpace(c.BindAddress) == "" {
		c.BindAddress = DefaultBindAddress
	}
	if strings.TrimSpace(c.LogLevel) == "" {
		c.LogLevel = DefaultLogLevel
	}
	c.DesktopHealthPollInterval = strings.TrimSpace(c.DesktopHealthPollInterval)
	if c.DesktopHealthPollInterval == "" {
		c.DesktopHealthPollInterval = DefaultDesktopHealthPollInterval
	}
	if c.ConclusionApprovalTimeout <= 0 {
		c.ConclusionApprovalTimeout = DefaultConclusionApprovalTimeout
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
	if c.Shortcuts == nil {
		c.Shortcuts = defaultShortcuts()
		return
	}
	for section, defaults := range defaultShortcuts() {
		bindings, ok := c.Shortcuts[section]
		if !ok {
			bindings = make(map[string]string, len(defaults))
			c.Shortcuts[section] = bindings
		}
		for action, key := range defaults {
			if _, exists := bindings[action]; !exists {
				bindings[action] = key
			}
		}
	}
}

func (c Config) DesktopHealthPollIntervalDuration() time.Duration {
	interval, err := time.ParseDuration(c.DesktopHealthPollInterval)
	if err != nil {
		panic(fmt.Sprintf("invalid desktop_health_poll_interval %q after validation: %v", c.DesktopHealthPollInterval, err))
	}
	return interval
}

func resolvePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return DefaultPath()
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

func cloneVariantConfigs(src map[string]VariantConfig) map[string]VariantConfig {
	if len(src) == 0 {
		return map[string]VariantConfig{}
	}
	dst := make(map[string]VariantConfig, len(src))
	for name, variant := range src {
		dst[name] = VariantConfig{
			Agent: variant.Agent,
			Args:  append([]string(nil), variant.Args...),
			Env:   cloneStringMap(variant.Env),
		}
	}
	return dst
}

func cloneArchitectConfigs(src map[string]ArchitectConfig) map[string]ArchitectConfig {
	if len(src) == 0 {
		return map[string]ArchitectConfig{}
	}
	dst := make(map[string]ArchitectConfig, len(src))
	for key, architect := range src {
		dst[key] = ArchitectConfig{
			Path:  architect.Path,
			Group: architect.Group,
			Repos: cloneStringMap(architect.Repos),
		}
	}
	return dst
}

func cloneTabs(src map[string][]TabEntry) map[string][]TabEntry {
	if len(src) == 0 {
		return map[string][]TabEntry{}
	}
	dst := make(map[string][]TabEntry, len(src))
	for sessionType, entries := range src {
		dst[sessionType] = append([]TabEntry(nil), entries...)
	}
	return dst
}

func cloneShortcuts(src map[string]map[string]string) map[string]map[string]string {
	if len(src) == 0 {
		return map[string]map[string]string{}
	}
	dst := make(map[string]map[string]string, len(src))
	for section, bindings := range src {
		dst[section] = cloneStringMap(bindings)
	}
	return dst
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
