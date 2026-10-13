package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultEnvironment = "production"
	defaultHomeDirName = ".hiveryn"
	homeEnvVar         = "HIVERYN_HOME"
	environmentEnvVar  = "HIVERYN_ENV"
	databaseFileName   = "daemon.db"
	logDirName         = "logs"
	actionsDirName     = "actions"
	actionRunsDirName  = "action-runs"
	actionArtifactsDir = "action-artifacts"
)

type Runtime struct {
	Environment string
	Home        string
	ConfigPath  string
	DBPath      string
	LogDir      string
	// ActionsRegistry is actions.yaml, which maps each Action name to its
	// checkout and machine.
	ActionsRegistry string
	// ActionsDir is the conventional location of local Action checkouts. It
	// is not discovered: only registered checkouts are Actions, and the
	// registry is bootstrapped from it once (config.BootstrapActionRegistry).
	ActionsDir string
	// ActionRunsDir holds each local execution's working output directory, at
	// <ActionRunsDir>/<action>/<execution id>, outside every action repo.
	ActionRunsDir string
	// ActionArtifactsDir holds the managed artifact directories on this
	// machine: frozen packages (packages/<execution id>) and copies delivered
	// to local callers (deliveries/<execution id>).
	ActionArtifactsDir string
}

func ResolveRuntime(configPath, databasePath string) (Runtime, error) {
	home, err := runtimeHome()
	if err != nil {
		return Runtime{}, err
	}

	resolvedConfigPath, err := resolveRuntimePath(configPath, filepath.Join(home, configFileName))
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve config path: %w", err)
	}

	resolvedDBPath, err := resolveRuntimePath(databasePath, filepath.Join(home, databaseFileName))
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve database path: %w", err)
	}

	resolvedLogDir, err := resolveRuntimePath("", filepath.Join(home, logDirName))
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve log directory: %w", err)
	}

	environment := strings.TrimSpace(os.Getenv(environmentEnvVar))
	if environment == "" {
		environment = DefaultEnvironment
	}

	return Runtime{
		Environment:     environment,
		Home:            home,
		ConfigPath:      resolvedConfigPath,
		DBPath:          resolvedDBPath,
		LogDir:          resolvedLogDir,
		ActionsRegistry: ActionsRegistryPath(filepath.Dir(resolvedConfigPath)),
		ActionsDir:      filepath.Join(home, actionsDirName),
		// Output folders sit beside, never inside, the action repositories.
		ActionRunsDir:      filepath.Join(home, actionRunsDirName),
		ActionArtifactsDir: filepath.Join(home, actionArtifactsDir),
	}, nil
}

func runtimeHome() (string, error) {
	home := strings.TrimSpace(os.Getenv(homeEnvVar))
	if home == "" {
		userHomeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		home = filepath.Join(userHomeDir, defaultHomeDirName)
	}

	resolvedHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve runtime home %q: %w", home, err)
	}
	return resolvedHome, nil
}

func resolveRuntimePath(path, defaultPath string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = defaultPath
	}

	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	return resolvedPath, nil
}
