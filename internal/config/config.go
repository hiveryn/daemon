package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPort                      = 4201
	DefaultBindAddress               = "127.0.0.1"
	DefaultLogLevel                  = "info"
	DefaultDesktopHealthPollInterval = "1s"
	DefaultIntentWaitTimeout         = 20
	configFileName                   = "config.yaml"
	variantsFileName                 = "variants.yaml"
	architectsFileName               = "architects.yaml"
	architectConfigFileName          = "hiveryn.yaml"
	tabsFileName                     = "tabs.yaml"
	shortcutsFileName                = "shortcuts.yaml"
)

type Config struct {
	Port                      int                          `yaml:"port"`
	BindAddress               string                       `yaml:"bind_address"`
	LogLevel                  string                       `yaml:"log_level"`
	Shell                     string                       `yaml:"shell,omitempty"`
	DesktopHealthPollInterval string                       `yaml:"desktop_health_poll_interval,omitempty"`
	IntentWaitTimeout         int                          `yaml:"intent_wait_timeout,omitempty"`
	ArchiveAgentEvents        bool                         `yaml:"archive_agent_events"`
	Variants                  map[string]VariantConfig     `yaml:"-"`
	Architects                map[string]ArchitectConfig   `yaml:"-"`
	Tabs                      map[string][]TabEntry        `yaml:"-"`
	Shortcuts                 map[string]map[string]string `yaml:"-"`
}

// Source is the daemon's view of the current configuration. Current never
// hands out a config that failed validation: a reloading source keeps serving
// the last valid config while an on-disk edit is broken, and reports that
// state through LoadStatus so it is visible rather than silently masked.
type Source interface {
	Current() (Config, error)
	// LoadStatus reports whether the most recent attempt to read the on-disk
	// configuration succeeded. A non-nil Error means Current is serving the
	// last valid config instead of what is on disk.
	LoadStatus() LoadStatus
}

// LoadStatus describes the most recent reload attempt of a Source.
type LoadStatus struct {
	// Error is the verbatim reload failure, empty when the on-disk config is
	// the one being served.
	Error string `json:"error,omitempty"`
	// FailedAt is when the failing reload happened; nil when Error is empty.
	FailedAt *time.Time `json:"failed_at,omitempty"`
	// LoadedAt is when the served config was last successfully loaded.
	LoadedAt time.Time `json:"loaded_at"`
}

type staticSource struct {
	cfg      Config
	loadedAt time.Time
}

// reloadingSource re-reads the optional YAML files on every Current call so
// edits apply without a restart. When a reload fails — a half-written
// hiveryn.yaml, an unknown key, a blank repo path — it keeps serving the last
// config that validated, logs the failure at ERROR once per distinct error,
// and exposes it through LoadStatus. An invalid edit therefore never replaces
// the runtime config, and never hides either: the workspace check reports the
// broken file and worker launch validates hiveryn.yaml on disk directly.
type reloadingSource struct {
	path string
	base Config

	mu        sync.Mutex
	lastGood  Config
	loadedAt  time.Time
	lastErr   error
	failedAt  time.Time
	loggedErr string
}

type VariantConfig struct {
	Agent string                     `yaml:"agent"`
	Model string                     `yaml:"model"`
	Yolo  bool                       `yaml:"yolo"`
	Mode  string                     `yaml:"mode"`
	Args  []string                   `yaml:"args"`
	Env   map[string]string          `yaml:"env"`
	MCP   map[string]MCPServerConfig `yaml:"mcp_servers"`
}

type MCPServerConfig struct {
	Command           string            `yaml:"command"`
	Args              []string          `yaml:"args"`
	Env               map[string]string `yaml:"env"`
	CWD               string            `yaml:"cwd"`
	URL               string            `yaml:"url"`
	BearerTokenEnvVar string            `yaml:"bearer_token_env_var"`
}

// ReservedMCPServerName is the name of the base hiveryn-daemon MCP server that
// every session always receives. Variants must not declare a server under this
// name to avoid colliding with the base server.
const ReservedMCPServerName = "hiveryn-daemon"

// ArchitectConfig is the resolved configuration for a single architect,
// assembled from the global architects.yaml registry (which supplies the key
// and workspace path) and the architect's own hiveryn.yaml (name, repos and
// the optional availableActions). Repos is keyed by repo key for lookups, with
// paths expanded to absolute. AvailableActions names the global Actions this
// architect may discover and request, in file order; omitted means none. It
// never restricts the user's own manual launches.
//
// There are no prompt fields: architect and worker instructions are built into
// the daemon, and the only per-architect customization is the optional
// ARCHITECT_SYSTEM.md file in the workspace, read at session start.
type ArchitectConfig struct {
	Name             string
	Path             string
	Repos            map[string]string
	AvailableActions []string
}

// architectFile is the on-disk shape of <architectPath>/hiveryn.yaml. It is
// decoded strictly: an unknown key (such as a leftover prompts: block) is an
// error, because a key the daemon silently ignores would look configured while
// doing nothing.
type architectFile struct {
	Name             string            `yaml:"name,omitempty"`
	Repos            map[string]string `yaml:"repos,omitempty"`
	AvailableActions []string          `yaml:"availableActions,omitempty"`
}

type TabEntry struct {
	Type    string `yaml:"type"`
	Command string `yaml:"command"`
}

func defaultShortcuts() map[string]map[string]string {
	return map[string]map[string]string{
		"global": {
			"focus-left":       "Cmd+Shift+h",
			"focus-right":      "Cmd+Shift+l",
			"focus-down":       "Cmd+Shift+j",
			"focus-up":         "Cmd+Shift+k",
			"focus-main":       "Cmd+1",
			"command-palette":  "Cmd+p",
			"first-session":    "Cmd+Shift+0",
			"prev-session":     "Cmd+Shift+[",
			"next-session":     "Cmd+Shift+]",
			"close-tab":        "Cmd+w",
			"new-terminal":     "Cmd+t",
			"split-horizontal": "Cmd+d",
			"maximize-pane":    "Cmd+m",
			"quit":             "q",
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
		IntentWaitTimeout:         DefaultIntentWaitTimeout,
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
	return staticSource{cfg: cfg.Clone(), loadedAt: time.Now().UTC()}
}

// NewReloadingSource returns a Source that re-reads the optional YAML files
// under path's directory on every Current call. base is the config loaded at
// startup and is the first "last valid" config.
func NewReloadingSource(path string, base Config) (Source, error) {
	resolvedPath, err := resolvePath(path)
	if err != nil {
		return nil, err
	}

	return &reloadingSource{
		path:     resolvedPath,
		base:     base.Clone(),
		lastGood: base.Clone(),
		loadedAt: time.Now().UTC(),
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

	if err := loadOptionalConfigFiles(configDir, &cfg); err != nil {
		return Config{}, err
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

func (s staticSource) LoadStatus() LoadStatus {
	return LoadStatus{LoadedAt: s.loadedAt}
}

// Current reloads the optional YAML files. On failure it returns the last
// valid config: the daemon keeps running on what it last validated instead of
// failing every request until the file is repaired, and the failure is logged
// and exposed through LoadStatus rather than swallowed.
func (s *reloadingSource) Current() (Config, error) {
	cfg, err := s.reload()

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if err != nil {
		s.lastErr = err
		s.failedAt = now
		if msg := err.Error(); msg != s.loggedErr {
			s.loggedErr = msg
			slog.Error("config reload failed; serving the last valid config until the file is repaired",
				"config_path", s.path,
				"loaded_at", s.loadedAt,
				"error", err,
			)
		}
		return s.lastGood.Clone(), nil
	}
	if s.lastErr != nil {
		slog.Info("config reload recovered", "config_path", s.path)
	}
	s.lastErr = nil
	s.failedAt = time.Time{}
	s.loggedErr = ""
	s.lastGood = cfg.Clone()
	s.loadedAt = now
	return cfg, nil
}

func (s *reloadingSource) reload() (Config, error) {
	cfg := s.base.Clone()
	cfg.Variants = map[string]VariantConfig{}
	cfg.Architects = map[string]ArchitectConfig{}
	cfg.Tabs = map[string][]TabEntry{}
	cfg.Shortcuts = defaultShortcuts()
	if err := loadOptionalConfigFiles(filepath.Dir(s.path), &cfg); err != nil {
		return Config{}, fmt.Errorf("reload optional config files: %w", err)
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config after reload: %w", err)
	}
	return cfg, nil
}

// LoadStatus reloads first so the answer describes the file as it is now, not
// as it was at the last request that happened to read config: a repaired file
// must read as repaired immediately.
func (s *reloadingSource) LoadStatus() LoadStatus {
	if _, err := s.Current(); err != nil {
		// Current never returns an error for a reload failure (it serves the
		// last valid config instead), so this is unreachable in practice; keep
		// the status honest if that ever changes.
		return LoadStatus{Error: err.Error()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := LoadStatus{LoadedAt: s.loadedAt}
	if s.lastErr != nil {
		status.Error = s.lastErr.Error()
		failedAt := s.failedAt
		status.FailedAt = &failedAt
	}
	return status
}

func loadOptionalConfigFiles(configDir string, cfg *Config) error {
	if err := loadOptionalFile(filepath.Join(configDir, variantsFileName), &cfg.Variants); err != nil {
		return fmt.Errorf("load variants: %w", err)
	}

	architects, err := loadArchitects(configDir)
	if err != nil {
		return fmt.Errorf("load architects: %w", err)
	}
	cfg.Architects = architects

	if err := loadOptionalFile(filepath.Join(configDir, tabsFileName), &cfg.Tabs); err != nil {
		return fmt.Errorf("load tabs: %w", err)
	}

	if err := loadOptionalFile(filepath.Join(configDir, shortcutsFileName), &cfg.Shortcuts); err != nil {
		return fmt.Errorf("load shortcuts: %w", err)
	}

	return nil
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

// loadArchitects reads the global architects.yaml registry (a bare key ->
// workspace path map) and, for each registered architect, loads its own
// hiveryn.yaml. A missing or unparseable hiveryn.yaml is a hard error.
func loadArchitects(configDir string) (map[string]ArchitectConfig, error) {
	registry := map[string]string{}
	if err := loadOptionalFile(filepath.Join(configDir, architectsFileName), &registry); err != nil {
		return nil, err
	}

	architects := make(map[string]ArchitectConfig, len(registry))
	for key, workspacePath := range registry {
		architect, err := loadArchitectFile(key, workspacePath)
		if err != nil {
			return nil, err
		}
		architects[key] = architect
	}
	return architects, nil
}

func loadArchitectFile(key, workspacePath string) (ArchitectConfig, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return ArchitectConfig{}, fmt.Errorf("architects.%s path is required", key)
	}

	_, file, err := readArchitectFile(workspacePath)
	if err != nil {
		return ArchitectConfig{}, err
	}

	// Duplicate repo keys are rejected by the YAML decoder.
	return architectConfigFromFile(key, workspacePath, file)
}

// readArchitectFile reads and strictly decodes <workspacePath>/hiveryn.yaml,
// returning the raw bytes and the decoded file. It is the single reader used
// by the loader and by ValidateArchitectConfig so both see the same document
// the same way.
func readArchitectFile(workspacePath string) ([]byte, architectFile, error) {
	filePath := filepath.Join(workspacePath, architectConfigFileName)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, architectFile{}, fmt.Errorf("read %q: %w", filePath, err)
	}

	file, err := decodeArchitectFile(data)
	if err != nil {
		return nil, architectFile{}, fmt.Errorf("decode YAML %q: %w", filePath, err)
	}
	return data, file, nil
}

// decodeArchitectFile decodes hiveryn.yaml with unknown keys rejected. The
// file holds name, repos and the optional availableActions; anything else — most likely a prompts:
// block left over from before prompts became built in — is reported so it can
// be removed instead of sitting there looking configured.
func decodeArchitectFile(data []byte) (architectFile, error) {
	var file architectFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file decodes to nothing; validation reports the missing
			// name, which is the actionable message.
			return architectFile{}, nil
		}
		return architectFile{}, fmt.Errorf("%w (hiveryn.yaml holds only `name`, `repos` and the optional `availableActions`; prompt overrides were removed — architect and worker instructions are built into the daemon, and ARCHITECT_SYSTEM.md carries per-architect preferences)", err)
	}
	return file, nil
}

// architectConfigFromFile converts a decoded architectFile into a resolved
// ArchitectConfig without touching the filesystem: repo paths are home-expanded
// to absolute. It is the single conversion used by the loader and by
// ValidateArchitectConfig, guaranteeing they agree.
func architectConfigFromFile(key, workspacePath string, file architectFile) (ArchitectConfig, error) {
	repos := make(map[string]string, len(file.Repos))
	for name, path := range file.Repos {
		if strings.TrimSpace(path) == "" {
			// filepath.Abs("") would silently resolve to the daemon's working
			// directory, turning a blank entry into a real, wrong repo path.
			return ArchitectConfig{}, fmt.Errorf("architect %q repo %q: path is required", key, name)
		}
		expanded, err := expandHomePath(path)
		if err != nil {
			return ArchitectConfig{}, fmt.Errorf("architect %q repo %q: %w", key, name, err)
		}
		repos[name] = expanded
	}

	return ArchitectConfig{
		Name:             file.Name,
		Path:             workspacePath,
		Repos:            repos,
		AvailableActions: append([]string(nil), file.AvailableActions...),
	}, nil
}

// expandHomePath expands a leading ~ or ~/ to the user's home directory and
// returns an absolute path, mirroring runtimeHome's resolution in runtime.go.
func expandHomePath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if p == "~" {
			p = home
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve repo path %q: %w", p, err)
	}
	return abs, nil
}

type coreConfig struct {
	Port                      int    `yaml:"port"`
	BindAddress               string `yaml:"bind_address"`
	LogLevel                  string `yaml:"log_level"`
	Shell                     string `yaml:"shell,omitempty"`
	DesktopHealthPollInterval string `yaml:"desktop_health_poll_interval,omitempty"`
	IntentWaitTimeout         int    `yaml:"intent_wait_timeout,omitempty"`
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
		IntentWaitTimeout:         c.IntentWaitTimeout,
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

	if c.IntentWaitTimeout < 0 {
		return fmt.Errorf("intent_wait_timeout must be >= 0")
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
		switch variant.Mode {
		case "", "build", "plan":
		default:
			return fmt.Errorf("variants.%s.mode must be \"build\" or \"plan\", got %q", name, variant.Mode)
		}
		if variant.Mode == "plan" && variant.Agent == "codex" {
			return fmt.Errorf("variants.%s: mode \"plan\" is not supported by agent codex", name)
		}
		for key := range variant.Env {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("variants.%s.env keys must not be blank", name)
			}
		}
		for _, serverName := range sortedKeys(variant.MCP) {
			if strings.TrimSpace(serverName) == "" {
				return fmt.Errorf("variants.%s.mcp_servers keys must not be blank", name)
			}
			if serverName == ReservedMCPServerName {
				return fmt.Errorf("variants.%s.mcp_servers.%s name is reserved for the base daemon server", name, serverName)
			}
			server := variant.MCP[serverName]
			hasCommand := strings.TrimSpace(server.Command) != ""
			hasURL := strings.TrimSpace(server.URL) != ""
			if hasCommand == hasURL {
				return fmt.Errorf("variants.%s.mcp_servers.%s must set exactly one of command or url", name, serverName)
			}
			for key := range server.Env {
				if strings.TrimSpace(key) == "" {
					return fmt.Errorf("variants.%s.mcp_servers.%s.env keys must not be blank", name, serverName)
				}
			}
		}
	}

	architectKeys := sortedKeys(c.Architects)
	for _, key := range architectKeys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("architects keys must not be blank")
		}
		if err := validateArchitect(key, c.Architects[key]); err != nil {
			return err
		}
	}

	sessionTypes := sortedKeys(c.Tabs)
	for _, sessionType := range sessionTypes {
		if strings.TrimSpace(sessionType) == "" {
			return fmt.Errorf("tabs keys must not be blank")
		}
		for i, entry := range c.Tabs[sessionType] {
			if strings.TrimSpace(entry.Type) == "" {
				return fmt.Errorf("tabs.%s[%d].type is required", sessionType, i)
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
	if c.IntentWaitTimeout <= 0 {
		c.IntentWaitTimeout = DefaultIntentWaitTimeout
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
		if variant.MCP == nil {
			variant.MCP = map[string]MCPServerConfig{}
		}
		for serverName, server := range variant.MCP {
			if server.Args == nil {
				server.Args = []string{}
			}
			if server.Env == nil {
				server.Env = map[string]string{}
			}
			variant.MCP[serverName] = server
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
			Model: variant.Model,
			Yolo:  variant.Yolo,
			Mode:  variant.Mode,
			Args:  append([]string(nil), variant.Args...),
			Env:   cloneStringMap(variant.Env),
			MCP:   cloneMCPServerConfigs(variant.MCP),
		}
	}
	return dst
}

func cloneMCPServerConfigs(src map[string]MCPServerConfig) map[string]MCPServerConfig {
	if len(src) == 0 {
		return map[string]MCPServerConfig{}
	}
	dst := make(map[string]MCPServerConfig, len(src))
	for name, server := range src {
		dst[name] = MCPServerConfig{
			Command:           server.Command,
			Args:              append([]string(nil), server.Args...),
			Env:               cloneStringMap(server.Env),
			CWD:               server.CWD,
			URL:               server.URL,
			BearerTokenEnvVar: server.BearerTokenEnvVar,
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
			Name:             architect.Name,
			Path:             architect.Path,
			Repos:            cloneStringMap(architect.Repos),
			AvailableActions: append([]string(nil), architect.AvailableActions...),
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
