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
	DefaultPort        = 4200
	DefaultBindAddress = "127.0.0.1"
	DefaultLogLevel    = "info"
	configDirName      = ".hiveryn"
	configFileName     = "config.yaml"
)

type Config struct {
	Port          int                           `yaml:"port"`
	BindAddress   string                        `yaml:"bind_address"`
	LogLevel      string                        `yaml:"log_level"`
	AgentProfiles map[string]AgentProfileConfig `yaml:"agent_profiles"`
	Architects    map[string]ArchitectConfig    `yaml:"architects"`
}

type AgentProfileConfig struct {
	Agent string            `yaml:"agent"`
	Args  []string          `yaml:"args"`
	Env   map[string]string `yaml:"env"`
}

type ArchitectConfig struct {
	Path  string            `yaml:"path"`
	Group string            `yaml:"group"`
	Repos map[string]string `yaml:"repos"`
}

func Default() Config {
	return Config{
		Port:          DefaultPort,
		BindAddress:   DefaultBindAddress,
		LogLevel:      DefaultLogLevel,
		AgentProfiles: map[string]AgentProfileConfig{},
		Architects:    map[string]ArchitectConfig{},
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

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := Default()
			if err := cfg.Save(path); err != nil {
				return Config{}, err
			}
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode YAML config %q: %w", path, err)
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
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

	data, err := yaml.Marshal(c)
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

	profileNames := sortedKeys(c.AgentProfiles)
	for _, name := range profileNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("agent_profiles keys must not be blank")
		}
		profile := c.AgentProfiles[name]
		if strings.TrimSpace(profile.Agent) == "" {
			return fmt.Errorf("agent_profiles.%s.agent is required", name)
		}
		for key := range profile.Env {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("agent_profiles.%s.env keys must not be blank", name)
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
	if c.AgentProfiles == nil {
		c.AgentProfiles = map[string]AgentProfileConfig{}
	}
	for name, profile := range c.AgentProfiles {
		if profile.Args == nil {
			profile.Args = []string{}
		}
		if profile.Env == nil {
			profile.Env = map[string]string{}
		}
		c.AgentProfiles[name] = profile
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
