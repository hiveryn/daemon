package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPort        = 4200
	DefaultBindAddress = "127.0.0.1"
	DefaultLogLevel    = "info"
	configDirName      = ".hiveryn"
	configFileName     = "daemon.yaml"
)

type Config struct {
	Port        int    `json:"port" yaml:"port"`
	BindAddress string `json:"bind_address" yaml:"bind_address"`
	LogLevel    string `json:"log_level" yaml:"log_level"`
}

func Default() Config {
	return Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
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
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode JSON config %q: %w", path, err)
		}
	default:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode YAML config %q: %w", path, err)
		}
	}

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

	if err := c.Validate(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	var (
		data []byte
		err  error
	)

	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		data, err = json.MarshalIndent(c, "", "  ")
		if err == nil {
			data = append(data, '\n')
		}
	default:
		data, err = yaml.Marshal(c)
	}
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

	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
