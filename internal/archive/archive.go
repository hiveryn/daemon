package archive

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	archiveSubDir = "archive"
	eventsDir     = "agent_events"
	filePrefix    = "agent_events_"
	fileExt       = ".jsonl"
	dateLayout    = "2006-01-02"
)

type ArchivedEvent struct {
	IngestedAt         time.Time         `json:"ingested_at"`
	ArchitectKey       string            `json:"architect_key"`
	ArchitectWorkspace string            `json:"architect_workspace"`
	SessionID          string            `json:"session_id"`
	SessionType        string            `json:"session_type"`
	TicketID           string            `json:"ticket_id,omitempty"`
	ProfileName        string            `json:"profile_name,omitempty"`
	Agent              string            `json:"agent,omitempty"`
	Workdir            string            `json:"workdir"`
	AdditionalWorkdirs []string          `json:"additional_workdirs,omitempty"`
	AdditionalRepos    []string          `json:"additional_repos,omitempty"`
	AgentID            string            `json:"agent_id"`
	AgentKind          string            `json:"agent_kind"`
	NativeID           string            `json:"native_id,omitempty"`
	PrimaryNativeID    string            `json:"primary_native_id,omitempty"`
	NativeSessionRole  string            `json:"native_session_role,omitempty"`
	Status             string            `json:"status"`
	Tool               string            `json:"tool,omitempty"`
	Message            string            `json:"message,omitempty"`
	At                 time.Time         `json:"at"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Raw                map[string]any    `json:"raw"`
}

type Archiver struct {
	mu      sync.Mutex
	logger  *slog.Logger
	dir     string
	nowFunc func() time.Time
	current *os.File
	date    string
}

func New(homeDir string, logger *slog.Logger) (*Archiver, error) {
	return newArchiver(homeDir, logger, time.Now)
}

func newArchiver(homeDir string, logger *slog.Logger, nowFunc func() time.Time) (*Archiver, error) {
	dir := filepath.Join(homeDir, archiveSubDir, eventsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create archive dir %q: %w", dir, err)
	}
	return &Archiver{logger: logger, dir: dir, nowFunc: nowFunc}, nil
}

func (a *Archiver) Write(record ArchivedEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	today := a.nowFunc().UTC().Format(dateLayout)
	if a.date != today {
		a.rotate(today)
	}

	if a.current == nil {
		return
	}

	line, err := json.Marshal(record)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("failed to marshal archived event", "error", err)
		}
		return
	}

	if _, err := a.current.Write(append(line, '\n')); err != nil {
		if a.logger != nil {
			a.logger.Warn("failed to write archived event", "path", a.current.Name(), "error", err)
		}
	}
}

func (a *Archiver) rotate(today string) {
	if a.current != nil {
		if err := a.current.Close(); err != nil {
			if a.logger != nil {
				a.logger.Warn("failed to close archive file", "path", a.current.Name(), "error", err)
			}
		}
		a.current = nil
	}

	path := filepath.Join(a.dir, filePrefix+today+fileExt)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("failed to open archive file", "path", path, "error", err)
		}
		return
	}
	a.current = f
	a.date = today
}

func (a *Archiver) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.current == nil {
		return nil
	}
	err := a.current.Close()
	a.current = nil
	return err
}
