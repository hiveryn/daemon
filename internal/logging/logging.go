package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/hiveryn/daemon/internal/config"
)

const (
	daemonLogName   = "daemon.jsonl"
	requestLogName  = "requests.jsonl"
	timestampLayout = "2006-01-02T15:04:05.000Z07:00"
)

type Manager struct {
	level         *slog.LevelVar
	appFile       *lineFile
	requestFile   *lineFile
	appLogger     *slog.Logger
	requestLogger *RequestLogger
}

func New(level string) (*Manager, error) {
	logDir, err := defaultLogDir()
	if err != nil {
		return nil, err
	}
	return NewWithDir(level, logDir)
}

func NewWithDir(level, logDir string) (*Manager, error) {
	if logDir == "" {
		return nil, fmt.Errorf("log directory is required")
	}

	resolvedLogDir, err := filepath.Abs(logDir)
	if err != nil {
		return nil, fmt.Errorf("resolve log directory %q: %w", logDir, err)
	}
	if err := os.MkdirAll(resolvedLogDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory %q: %w", resolvedLogDir, err)
	}

	appFile, err := newLineFile(filepath.Join(resolvedLogDir, daemonLogName), "app")
	if err != nil {
		return nil, err
	}
	requestFile, err := newLineFile(filepath.Join(resolvedLogDir, requestLogName), "request")
	if err != nil {
		_ = appFile.Close()
		return nil, err
	}

	levelVar := &slog.LevelVar{}
	manager := &Manager{
		level:       levelVar,
		appFile:     appFile,
		requestFile: requestFile,
	}
	if err := manager.SetLevel(level); err != nil {
		_ = requestFile.Close()
		_ = appFile.Close()
		return nil, err
	}

	manager.appLogger = slog.New(&appHandler{level: levelVar, file: appFile})
	manager.requestLogger = &RequestLogger{file: requestFile}
	return manager, nil
}

func (m *Manager) AppLogger() *slog.Logger {
	return m.appLogger
}

func (m *Manager) RequestLogger() *RequestLogger {
	return m.requestLogger
}

func (m *Manager) SetLevel(level string) error {
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("parse log level %q: %w", level, err)
	}
	m.level.Set(slogLevel)
	return nil
}

func (m *Manager) Close() error {
	var errs []error
	if err := m.requestFile.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := m.appFile.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func defaultLogDir() (string, error) {
	runtime, err := config.ResolveRuntime("", "")
	if err != nil {
		return "", err
	}
	return runtime.LogDir, nil
}

type lineFile struct {
	mu    sync.Mutex
	path  string
	kind  string
	file  *os.File
	warns stderrWarnWriter
}

func newLineFile(path, kind string) (*lineFile, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s log %q: %w", kind, path, err)
	}
	return &lineFile{path: path, kind: kind, file: file}, nil
}

func (f *lineFile) WriteLine(line []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		f.warns.Warnf("warning: attempted to write closed %s log %s\n", f.kind, f.path)
		return
	}
	if _, err := f.file.Write(append(line, '\n')); err != nil {
		f.warns.Warnf("warning: failed to write %s log %s: %v\n", f.kind, f.path, err)
	}
}

func (f *lineFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	if err != nil {
		return fmt.Errorf("close %s log %q: %w", f.kind, f.path, err)
	}
	return nil
}

type stderrWarnWriter struct {
	mu sync.Mutex
}

func (w *stderrWarnWriter) Warnf(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = fmt.Fprintf(os.Stderr, format, args...)
}
