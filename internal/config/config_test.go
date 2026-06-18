package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadCreatesDefaultConfigWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	expected := Default()
	if !reflect.DeepEqual(cfg, expected) {
		t.Fatalf("expected %#v, got %#v", expected, cfg)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected generated config file to be non-empty")
	}
}

func TestResolveRuntimeDefaultsToProductionHome(t *testing.T) {
	t.Setenv("HIVERYN_HOME", "")
	t.Setenv("HIVERYN_ENV", "")

	runtime, err := ResolveRuntime("", "")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}

	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	expectedHome := filepath.Join(userHome, ".hiveryn")
	if runtime.Environment != DefaultEnvironment {
		t.Fatalf("expected environment %q, got %q", DefaultEnvironment, runtime.Environment)
	}
	if runtime.Home != expectedHome {
		t.Fatalf("expected runtime home %q, got %q", expectedHome, runtime.Home)
	}
	if runtime.ConfigPath != filepath.Join(expectedHome, configFileName) {
		t.Fatalf("expected config path %q, got %q", filepath.Join(expectedHome, configFileName), runtime.ConfigPath)
	}
	if runtime.DBPath != filepath.Join(expectedHome, databaseFileName) {
		t.Fatalf("expected db path %q, got %q", filepath.Join(expectedHome, databaseFileName), runtime.DBPath)
	}
	if runtime.LogDir != filepath.Join(expectedHome, logDirName) {
		t.Fatalf("expected log dir %q, got %q", filepath.Join(expectedHome, logDirName), runtime.LogDir)
	}
}

func TestResolveRuntimeUsesEnvironmentAndHomeOverrides(t *testing.T) {
	home := filepath.Join(t.TempDir(), "runtime-dev")
	t.Setenv("HIVERYN_HOME", home)
	t.Setenv("HIVERYN_ENV", "development")

	runtime, err := ResolveRuntime("", "")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}

	if runtime.Environment != "development" {
		t.Fatalf("expected environment %q, got %q", "development", runtime.Environment)
	}
	if runtime.Home != home {
		t.Fatalf("expected runtime home %q, got %q", home, runtime.Home)
	}
	if runtime.ConfigPath != filepath.Join(home, configFileName) {
		t.Fatalf("expected config path %q, got %q", filepath.Join(home, configFileName), runtime.ConfigPath)
	}
	if runtime.DBPath != filepath.Join(home, databaseFileName) {
		t.Fatalf("expected db path %q, got %q", filepath.Join(home, databaseFileName), runtime.DBPath)
	}
	if runtime.LogDir != filepath.Join(home, logDirName) {
		t.Fatalf("expected log dir %q, got %q", filepath.Join(home, logDirName), runtime.LogDir)
	}
}

func TestResolveRuntimePrefersExplicitConfigAndDBPaths(t *testing.T) {
	t.Setenv("HIVERYN_HOME", filepath.Join(t.TempDir(), "runtime-dev"))
	configPath := filepath.Join("testdata", "config.yaml")
	databasePath := filepath.Join("testdata", "daemon.db")

	runtime, err := ResolveRuntime(configPath, databasePath)
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}

	expectedConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatalf("filepath.Abs(configPath): %v", err)
	}
	expectedDBPath, err := filepath.Abs(databasePath)
	if err != nil {
		t.Fatalf("filepath.Abs(databasePath): %v", err)
	}
	if runtime.ConfigPath != expectedConfigPath {
		t.Fatalf("expected config path %q, got %q", expectedConfigPath, runtime.ConfigPath)
	}
	if runtime.DBPath != expectedDBPath {
		t.Fatalf("expected db path %q, got %q", expectedDBPath, runtime.DBPath)
	}
}

func TestSaveAndLoadCoreConfig(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	path := filepath.Join(configDir, configFileName)
	input := Config{
		Port:                      4312,
		BindAddress:               "127.0.0.1",
		LogLevel:                  "debug",
		Shell:                     "/bin/zsh",
		DesktopHealthPollInterval: "2500ms",
		Variants:                  map[string]VariantConfig{},
		Architects:                map[string]ArchitectConfig{},
		Tabs:                      map[string][]TabEntry{},
	}

	if err := input.Save(path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loaded.Port != input.Port || loaded.BindAddress != input.BindAddress || loaded.LogLevel != input.LogLevel || loaded.Shell != input.Shell || loaded.DesktopHealthPollInterval != input.DesktopHealthPollInterval {
		t.Fatalf("core fields mismatch: expected port=%d addr=%s level=%s shell=%s interval=%s, got port=%d addr=%s level=%s shell=%s interval=%s",
			input.Port, input.BindAddress, input.LogLevel, input.Shell, input.DesktopHealthPollInterval,
			loaded.Port, loaded.BindAddress, loaded.LogLevel, loaded.Shell, loaded.DesktopHealthPollInterval)
	}
}

func TestLoadAllFiles(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()

	writeYAML(t, filepath.Join(configDir, configFileName), map[string]interface{}{
		"port":                         4201,
		"bind_address":                 "127.0.0.1",
		"log_level":                    "info",
		"shell":                        "/bin/zsh",
		"desktop_health_poll_interval": "3s",
	})

	writeYAML(t, filepath.Join(configDir, variantsFileName), map[string]VariantConfig{
		"codex-personal": {
			Agent: "codex",
			Args:  []string{"--dangerously-bypass-approvals-and-sandbox"},
			Env:   map[string]string{"CODEX_HOME": "/Users/kareem/.codex-personal"},
		},
	})

	architectDir := writeArchitect(t, map[string]any{
		"name":  "Hiveryn",
		"repos": map[string]string{"daemon": "/Users/kareem/hiveryn/daemon"},
	})
	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]string{
		"hiveryn": architectDir,
	})

	writeYAML(t, filepath.Join(configDir, tabsFileName), map[string][]TabEntry{
		"architect": {
			{Type: "kanban"},
			{Type: "terminal", Command: "yazi"},
		},
	})

	cfg, err := Load(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if len(cfg.Variants) != 1 || cfg.Variants["codex-personal"].Agent != "codex" {
		t.Fatalf("expected 1 variant, got %d", len(cfg.Variants))
	}
	if len(cfg.Architects) != 1 || cfg.Architects["hiveryn"].Name != "Hiveryn" {
		t.Fatalf("expected 1 architect, got %d", len(cfg.Architects))
	}
	if got := cfg.Architects["hiveryn"].Repos["daemon"]; got != "/Users/kareem/hiveryn/daemon" {
		t.Fatalf("expected daemon repo path, got %q", got)
	}
	if len(cfg.Tabs) != 1 || len(cfg.Tabs["architect"]) != 2 {
		t.Fatalf("expected 2 tabs for architect, got %d", len(cfg.Tabs["architect"]))
	}
	if cfg.Shell != "/bin/zsh" {
		t.Fatalf("expected shell to load, got %q", cfg.Shell)
	}
	if cfg.DesktopHealthPollInterval != "3s" {
		t.Fatalf("expected desktop health poll interval to load, got %q", cfg.DesktopHealthPollInterval)
	}
}

func TestLoadMissingOptionalFiles(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()

	writeYAML(t, filepath.Join(configDir, configFileName), map[string]interface{}{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "info",
	})

	cfg, err := Load(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if len(cfg.Variants) != 0 {
		t.Fatalf("expected empty variants when file missing, got %d", len(cfg.Variants))
	}
	if len(cfg.Architects) != 0 {
		t.Fatalf("expected empty architects when file missing, got %d", len(cfg.Architects))
	}
	if len(cfg.Tabs) != 0 {
		t.Fatalf("expected empty tabs when file missing, got %d", len(cfg.Tabs))
	}
}

func TestReloadingSourcePicksUpOptionalFileChanges(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	path := filepath.Join(configDir, configFileName)

	writeYAML(t, path, map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "info",
	})
	writeYAML(t, filepath.Join(configDir, variantsFileName), map[string]VariantConfig{
		"codex":    {Agent: "codex"},
		"opencode": {Agent: "opencode"},
	})
	hiverynDir := writeArchitect(t, map[string]any{
		"name":  "Hiveryn",
		"repos": map[string]string{"daemon": "/tmp/daemon"},
	})
	legacyDir := writeArchitect(t, map[string]any{
		"name":  "Legacy",
		"repos": map[string]string{"old": "/tmp/old"},
	})
	lithoDir := writeArchitect(t, map[string]any{
		"name":  "Litho",
		"repos": map[string]string{"app": "/tmp/lithoapp"},
	})
	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]string{
		"hiveryn": hiverynDir,
		"legacy":  legacyDir,
	})
	writeYAML(t, filepath.Join(configDir, tabsFileName), map[string][]TabEntry{
		"architect": {{Type: "kanban"}},
		"freeform":  {{Type: "terminal", Command: "oldfree"}},
	})
	writeYAML(t, filepath.Join(configDir, shortcutsFileName), map[string]map[string]string{
		"global": {"quit": "q"},
	})

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := NewReloadingSource(path, cfg)
	if err != nil {
		t.Fatalf("new source: %v", err)
	}

	reloaded, err := source.Current()
	if err != nil {
		t.Fatalf("current config: %v", err)
	}
	if _, ok := reloaded.Architects["litho"]; ok {
		t.Fatalf("unexpected architect before reload: %#v", reloaded.Architects)
	}
	if _, ok := reloaded.Variants["claude"]; ok {
		t.Fatalf("unexpected variant before reload: %#v", reloaded.Variants)
	}
	if _, ok := reloaded.Variants["opencode"]; !ok {
		t.Fatalf("expected initial opencode variant, got %#v", reloaded.Variants)
	}
	if got := reloaded.Tabs["architect"][0].Type; got != "kanban" {
		t.Fatalf("expected initial architect tab type kanban, got %q", got)
	}
	if got := reloaded.Tabs["freeform"][0].Command; got != "oldfree" {
		t.Fatalf("expected initial freeform terminal command oldfree, got %q", got)
	}
	if got := reloaded.Shortcuts["global"]["quit"]; got != "q" {
		t.Fatalf("expected initial quit shortcut q, got %q", got)
	}

	writeYAML(t, filepath.Join(configDir, variantsFileName), map[string]VariantConfig{
		"codex":  {Agent: "codex"},
		"claude": {Agent: "claude"},
	})

	writeYAML(t, filepath.Join(hiverynDir, architectConfigFileName), map[string]any{
		"name": "Hiveryn",
		"repos": map[string]string{
			"daemon":  "/tmp/daemon",
			"desktop": "/tmp/desktop",
		},
	})
	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]string{
		"hiveryn": hiverynDir,
		"litho":   lithoDir,
	})
	writeYAML(t, filepath.Join(configDir, tabsFileName), map[string][]TabEntry{
		"architect": {{Type: "terminal", Command: "btop"}},
		"ticket":    {{Type: "terminal", Command: "yazi"}},
	})
	writeYAML(t, filepath.Join(configDir, shortcutsFileName), map[string]map[string]string{
		"global": {"quit": "Cmd+q"},
	})

	reloaded, err = source.Current()
	if err != nil {
		t.Fatalf("current config after update: %v", err)
	}
	if _, ok := reloaded.Architects["litho"]; !ok {
		t.Fatalf("expected litho architect after reload, got %#v", reloaded.Architects)
	}
	if _, ok := reloaded.Architects["legacy"]; ok {
		t.Fatalf("expected removed architect to disappear after reload, got %#v", reloaded.Architects)
	}
	if _, ok := reloaded.Architects["hiveryn"].Repos["desktop"]; !ok {
		t.Fatalf("expected updated repo mappings after reload, got %#v", reloaded.Architects["hiveryn"].Repos)
	}
	if _, ok := reloaded.Variants["codex"]; !ok {
		t.Fatalf("expected base variants to remain available, got %#v", reloaded.Variants)
	}
	if _, ok := reloaded.Variants["claude"]; !ok {
		t.Fatalf("expected updated variant after reload, got %#v", reloaded.Variants)
	}
	if _, ok := reloaded.Variants["opencode"]; ok {
		t.Fatalf("expected removed variant to disappear after reload, got %#v", reloaded.Variants)
	}
	if got := reloaded.Tabs["architect"][0].Command; got != "btop" {
		t.Fatalf("expected reloaded architect terminal command btop, got %q", got)
	}
	if got := reloaded.Tabs["ticket"][0].Command; got != "yazi" {
		t.Fatalf("expected reloaded ticket terminal command yazi, got %q", got)
	}
	if _, ok := reloaded.Tabs["freeform"]; ok {
		t.Fatalf("expected removed freeform tabs to disappear after reload, got %#v", reloaded.Tabs)
	}
	if got := reloaded.Shortcuts["global"]["quit"]; got != "Cmd+q" {
		t.Fatalf("expected reloaded quit shortcut Cmd+q, got %q", got)
	}
}

func TestValidateRejectsInvalidDesktopHealthPollInterval(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:                      DefaultPort,
		BindAddress:               DefaultBindAddress,
		LogLevel:                  DefaultLogLevel,
		DesktopHealthPollInterval: "nope",
		Variants:                  map[string]VariantConfig{},
		Architects:                map[string]ArchitectConfig{},
		Tabs:                      map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected desktop health poll interval validation error")
	}
}

func TestValidateRejectsNonPositiveDesktopHealthPollInterval(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:                      DefaultPort,
		BindAddress:               DefaultBindAddress,
		LogLevel:                  DefaultLogLevel,
		DesktopHealthPollInterval: "0s",
		Variants:                  map[string]VariantConfig{},
		Architects:                map[string]ArchitectConfig{},
		Tabs:                      map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected non-positive desktop health poll interval validation error")
	}
}

func TestValidateRejectsNonLocalBindAddress(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: "0.0.0.0",
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs:        map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected bind_address validation error")
	}
}

func TestValidateRejectsBlankArchitectName(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects: map[string]ArchitectConfig{
			"hiveryn": {
				Name:  "",
				Path:  "/Users/kareem/architects/hiveryn",
				Repos: map[string]string{},
			},
		},
		Tabs: map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected architect name validation error")
	}
}

func TestValidateRejectsKickoffUnknownRepo(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects: map[string]ArchitectConfig{
			"hiveryn": {
				Name:  "Hiveryn",
				Path:  "/Users/kareem/architects/hiveryn",
				Repos: map[string]string{"daemon": "/tmp/daemon"},
				TicketKickoffs: []TicketKickoff{
					{Path: "/tmp/k.md", Repos: []string{"nonexistent"}},
				},
			},
		},
		Tabs: map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected unknown-repo kickoff validation error")
	}
}

func TestValidateRejectsMultipleDefaultKickoffs(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects: map[string]ArchitectConfig{
			"hiveryn": {
				Name:  "Hiveryn",
				Path:  "/Users/kareem/architects/hiveryn",
				Repos: map[string]string{"daemon": "/tmp/daemon"},
				TicketKickoffs: []TicketKickoff{
					{Path: "/tmp/a.md"},
					{Path: "/tmp/b.md"},
				},
			},
		},
		Tabs: map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected multiple-default kickoff validation error")
	}
}

func TestValidateRejectsDuplicateRepoKickoff(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects: map[string]ArchitectConfig{
			"hiveryn": {
				Name:  "Hiveryn",
				Path:  "/Users/kareem/architects/hiveryn",
				Repos: map[string]string{"daemon": "/tmp/daemon"},
				TicketKickoffs: []TicketKickoff{
					{Path: "/tmp/a.md", Repos: []string{"daemon"}},
					{Path: "/tmp/b.md", Repos: []string{"daemon"}},
				},
			},
		},
		Tabs: map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected duplicate-repo kickoff validation error")
	}
}

func TestValidateRejectsBlankTabType(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"architect": {
				{Type: ""},
			},
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected blank tab type validation error")
	}
}

func TestValidateAllowsPluginTabTypes(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"ticket": {
				{Type: "git-diff"},
				{Type: "kanban"},
			},
		},
	}.Validate()
	if err != nil {
		t.Fatalf("expected plugin tab types to be valid, got %v", err)
	}
}

func TestValidateAllowsUnnamedTerminalTabs(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"worker": {
				{Type: "terminal"},
			},
		},
	}.Validate()
	if err != nil {
		t.Fatalf("expected unnamed terminal tabs to be valid, got %v", err)
	}
}

func TestValidateAllowsDuplicateTerminalCommands(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"architect": {
				{Type: "terminal", Command: "yazi"},
				{Type: "terminal", Command: "yazi"},
			},
		},
	}.Validate()
	if err != nil {
		t.Fatalf("expected duplicate terminal commands to be valid, got %v", err)
	}
}

func TestValidateRejectsNonTerminalWithCommand(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"architect": {
				{Type: "event-log", Command: "ls"},
			},
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected non-terminal command rejection error")
	}
}

func TestValidateRejectsBlankTabSessionType(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"": {},
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected blank tab session type validation error")
	}
}

func TestLoadVariantWithMCPServers(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	writeYAML(t, filepath.Join(configDir, configFileName), map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "info",
	})

	variantsYAML := "claude-sonnet-plan:\n" +
		"  agent: claude\n" +
		"  args: [--model, claude-sonnet-4-6]\n" +
		"  mcp_servers:\n" +
		"    sentrux:\n" +
		"      command: sentrux\n" +
		"      args: [--mcp]\n"
	if err := os.WriteFile(filepath.Join(configDir, variantsFileName), []byte(variantsYAML), 0o600); err != nil {
		t.Fatalf("write variants: %v", err)
	}

	cfg, err := Load(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	variant, ok := cfg.Variants["claude-sonnet-plan"]
	if !ok {
		t.Fatalf("expected claude-sonnet-plan variant, got %#v", cfg.Variants)
	}
	server, ok := variant.MCP["sentrux"]
	if !ok {
		t.Fatalf("expected sentrux mcp server, got %#v", variant.MCP)
	}
	if server.Command != "sentrux" {
		t.Fatalf("expected command sentrux, got %q", server.Command)
	}
	if len(server.Args) != 1 || server.Args[0] != "--mcp" {
		t.Fatalf("expected args [--mcp], got %#v", server.Args)
	}
	if server.Env == nil {
		t.Fatal("expected normalized empty env, got nil")
	}
}

func TestValidateRejectsReservedMCPServerName(t *testing.T) {
	t.Parallel()

	err := variantConfigWithMCP(map[string]MCPServerConfig{
		ReservedMCPServerName: {Command: "sentrux"},
	}).Validate()
	if err == nil {
		t.Fatal("expected reserved mcp server name validation error")
	}
}

func TestValidateRejectsMCPServerWithoutTransport(t *testing.T) {
	t.Parallel()

	err := variantConfigWithMCP(map[string]MCPServerConfig{
		"sentrux": {},
	}).Validate()
	if err == nil {
		t.Fatal("expected missing transport validation error")
	}
}

func TestValidateRejectsMCPServerWithBothTransports(t *testing.T) {
	t.Parallel()

	err := variantConfigWithMCP(map[string]MCPServerConfig{
		"sentrux": {Command: "sentrux", URL: "http://127.0.0.1:9000"},
	}).Validate()
	if err == nil {
		t.Fatal("expected both-transports validation error")
	}
}

func TestValidateAllowsURLMCPServer(t *testing.T) {
	t.Parallel()

	err := variantConfigWithMCP(map[string]MCPServerConfig{
		"remote": {URL: "http://127.0.0.1:9000", BearerTokenEnvVar: "TOKEN"},
	}).Validate()
	if err != nil {
		t.Fatalf("expected url mcp server to be valid, got %v", err)
	}
}

func TestCloneIsolatesVariantMCPServers(t *testing.T) {
	t.Parallel()

	original := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants: map[string]VariantConfig{
			"claude-plan": {
				Agent: "claude",
				MCP: map[string]MCPServerConfig{
					"sentrux": {Command: "sentrux", Args: []string{"--mcp"}, Env: map[string]string{"K": "V"}},
				},
			},
		},
		Architects: map[string]ArchitectConfig{},
		Tabs:       map[string][]TabEntry{},
	}

	cloned := original.Clone()
	clonedServer := cloned.Variants["claude-plan"].MCP["sentrux"]
	clonedServer.Args[0] = "MUTATED"
	clonedServer.Env["K"] = "MUTATED"
	cloned.Variants["claude-plan"].MCP["added"] = MCPServerConfig{Command: "added"}

	origServer := original.Variants["claude-plan"].MCP["sentrux"]
	if origServer.Args[0] != "--mcp" {
		t.Fatalf("expected original args unchanged, got %#v", origServer.Args)
	}
	if origServer.Env["K"] != "V" {
		t.Fatalf("expected original env unchanged, got %#v", origServer.Env)
	}
	if _, ok := original.Variants["claude-plan"].MCP["added"]; ok {
		t.Fatal("expected original mcp map to be isolated from clone")
	}
}

func variantConfigWithMCP(mcp map[string]MCPServerConfig) Config {
	return Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants: map[string]VariantConfig{
			"claude-plan": {Agent: "claude", MCP: mcp},
		},
		Architects: map[string]ArchitectConfig{},
		Tabs:       map[string][]TabEntry{},
	}
}

// writeArchitect creates a fresh architect workspace directory containing a
// hiveryn.yaml built from file, and returns the workspace path.
func writeArchitect(t *testing.T, file map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, architectConfigFileName), file)
	return dir
}

func TestLoadFailsWhenArchitectConfigMissing(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	writeYAML(t, filepath.Join(configDir, configFileName), map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "info",
	})
	// Registry points at a workspace with no hiveryn.yaml.
	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]string{
		"hiveryn": t.TempDir(),
	})

	if _, err := Load(filepath.Join(configDir, configFileName)); err == nil {
		t.Fatal("expected error when architect hiveryn.yaml is missing")
	}
}

func TestLoadFailsOnDuplicateRepoKey(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	writeYAML(t, filepath.Join(configDir, configFileName), map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "info",
	})
	architectDir := t.TempDir()
	// A map literal can't express duplicate keys; write raw YAML so the
	// duplicate reaches the decoder.
	rawHiveryn := "name: Hiveryn\nrepos:\n  daemon: /tmp/a\n  daemon: /tmp/b\n"
	if err := os.WriteFile(filepath.Join(architectDir, architectConfigFileName), []byte(rawHiveryn), 0o600); err != nil {
		t.Fatalf("write hiveryn.yaml: %v", err)
	}
	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]string{
		"hiveryn": architectDir,
	})

	if _, err := Load(filepath.Join(configDir, configFileName)); err == nil {
		t.Fatal("expected error on duplicate repo key")
	}
}

func TestLoadResolvesPromptPathsRelativeToWorkspace(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	writeYAML(t, filepath.Join(configDir, configFileName), map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "info",
	})
	architectDir := writeArchitect(t, map[string]any{
		"name":  "Hiveryn",
		"repos": map[string]string{"daemon": "/tmp/daemon"},
		"prompts": map[string]any{
			"architect": map[string]string{
				"system":  "prompts/SYSTEM.md",
				"kickoff": "/abs/KICKOFF.md",
			},
			"ticket": map[string]any{
				"kickoffs": []map[string]any{
					{"path": "prompts/default.md"},
					{"path": "prompts/daemon.md", "repos": []string{"daemon"}},
				},
			},
		},
	})
	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]string{
		"hiveryn": architectDir,
	})

	cfg, err := Load(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	architect := cfg.Architects["hiveryn"]
	if want := filepath.Join(architectDir, "prompts/SYSTEM.md"); architect.SystemPromptPath != want {
		t.Fatalf("expected system prompt %q, got %q", want, architect.SystemPromptPath)
	}
	if architect.KickoffPromptPath != "/abs/KICKOFF.md" {
		t.Fatalf("expected absolute kickoff path preserved, got %q", architect.KickoffPromptPath)
	}
	if len(architect.TicketKickoffs) != 2 {
		t.Fatalf("expected 2 ticket kickoffs, got %d", len(architect.TicketKickoffs))
	}
	for _, kickoff := range architect.TicketKickoffs {
		if !filepath.IsAbs(kickoff.Path) {
			t.Fatalf("expected resolved absolute kickoff path, got %q", kickoff.Path)
		}
	}
}

func writeYAML(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %q: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
