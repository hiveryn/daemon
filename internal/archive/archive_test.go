package archive

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestNewCreatesDir(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	archiveDir := filepath.Join(dir, archiveSubDir, eventsDir)
	if _, err := os.Stat(archiveDir); os.IsNotExist(err) {
		t.Fatalf("archive directory was not created at %s", archiveDir)
	}
}

func TestWriteSingleEvent(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	record := ArchivedEvent{
		IngestedAt:   time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		ArchitectKey: "hiveryn",
		SessionID:    "sess-1",
		SessionType:  "ticket",
		TicketID:     "T-001",
		ProfileName:  "claude-build",
		Agent:        "claude",
		Workdir:      "/Users/kareem/architects/hiveryn",
		AgentID:      "sess-1",
		AgentKind:    "claude",
		Status:       "working",
		Tool:         "bash",
		Message:      "Running command",
		At:           time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		Metadata:     map[string]string{"key": "val"},
		Raw:          map[string]any{"cmd": "ls"},
	}

	a.Write(record)

	lines := readArchiveLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var read ArchivedEvent
	if err := json.Unmarshal(lines[0], &read); err != nil {
		t.Fatalf("failed to unmarshal line: %v", err)
	}

	if read.SessionID != "sess-1" {
		t.Errorf("expected session_id 'sess-1', got %q", read.SessionID)
	}
	if read.TicketID != "T-001" {
		t.Errorf("expected ticket_id 'T-001', got %q", read.TicketID)
	}
	if read.AgentKind != "claude" {
		t.Errorf("expected agent_kind 'claude', got %q", read.AgentKind)
	}
	if read.Status != "working" {
		t.Errorf("expected status 'working', got %q", read.Status)
	}
	if read.Tool != "bash" {
		t.Errorf("expected tool 'bash', got %q", read.Tool)
	}
	if read.Message != "Running command" {
		t.Errorf("expected message 'Running command', got %q", read.Message)
	}
	if read.Metadata["key"] != "val" {
		t.Errorf("expected metadata key 'val', got %q", read.Metadata["key"])
	}
	rawCmd, _ := read.Raw["cmd"].(string)
	if rawCmd != "ls" {
		t.Errorf("expected raw.cmd 'ls', got %q", rawCmd)
	}
}

func TestWriteMultipleEvents(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	for i := 0; i < 5; i++ {
		a.Write(ArchivedEvent{
			IngestedAt:  time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
			SessionID:   "sess-1",
			SessionType: "ticket",
			Workdir:     "/tmp/work",
			AgentID:     "sess-1",
			AgentKind:   "claude",
			Status:      "working",
			At:          time.Now(),
			Message:     "event " + string(rune('0'+i)),
			Raw:         map[string]any{},
		})
	}

	lines := readArchiveLines(t, dir)
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()

	day1 := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	var now time.Time
	nowFunc := func() time.Time { return now }

	a, err := newArchiver(dir, testLogger(t), nowFunc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	now = day1
	a.Write(ArchivedEvent{
		IngestedAt:  day1,
		SessionID:   "sess-1",
		SessionType: "ticket",
		Workdir:     "/tmp/work",
		AgentID:     "sess-1",
		AgentKind:   "claude",
		Status:      "working",
		At:          day1,
		Message:     "day1-event",
		Raw:         map[string]any{},
	})

	now = day2
	a.Write(ArchivedEvent{
		IngestedAt:  day2,
		SessionID:   "sess-2",
		SessionType: "ticket",
		Workdir:     "/tmp/work",
		AgentID:     "sess-2",
		AgentKind:   "codex",
		Status:      "working",
		At:          day2,
		Message:     "day2-event",
		Raw:         map[string]any{},
	})

	archiveDir := filepath.Join(dir, archiveSubDir, eventsDir)
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 archive files, got %d", len(entries))
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}

	expectedDay1 := "agent_events_2026-08-04.jsonl"
	expectedDay2 := "agent_events_2026-08-05.jsonl"
	if !names[expectedDay1] {
		t.Errorf("expected file %s", expectedDay1)
	}
	if !names[expectedDay2] {
		t.Errorf("expected file %s", expectedDay2)
	}

	day1Lines := readFileLines(t, filepath.Join(archiveDir, expectedDay1))
	if len(day1Lines) != 1 {
		t.Fatalf("expected 1 line in day1 file, got %d", len(day1Lines))
	}
	var day1Event ArchivedEvent
	if err := json.Unmarshal(day1Lines[0], &day1Event); err != nil {
		t.Fatal(err)
	}
	if day1Event.Message != "day1-event" {
		t.Errorf("expected 'day1-event', got %q", day1Event.Message)
	}

	day2Lines := readFileLines(t, filepath.Join(archiveDir, expectedDay2))
	if len(day2Lines) != 1 {
		t.Fatalf("expected 1 line in day2 file, got %d", len(day2Lines))
	}
	var day2Event ArchivedEvent
	if err := json.Unmarshal(day2Lines[0], &day2Event); err != nil {
		t.Fatal(err)
	}
	if day2Event.Message != "day2-event" {
		t.Errorf("expected 'day2-event', got %q", day2Event.Message)
	}
}

func TestRawPayloadPreservation(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	raw := map[string]any{
		"command":      "npm install",
		"exit_code":    float64(0),
		"args":         []any{"install", "--save"},
		"nested":       map[string]any{"deep": "value", "num": float64(42)},
		"null_field":   nil,
		"bool_true":    true,
		"bool_false":   false,
		"empty_string": "",
		"empty_array":  []any{},
		"empty_map":    map[string]any{},
	}

	a.Write(ArchivedEvent{
		IngestedAt:  time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		SessionID:   "sess-1",
		SessionType: "ticket",
		Workdir:     "/tmp/work",
		AgentID:     "sess-1",
		AgentKind:   "claude",
		Status:      "working",
		At:          time.Now(),
		Raw:         raw,
	})

	lines := readArchiveLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var read ArchivedEvent
	if err := json.Unmarshal(lines[0], &read); err != nil {
		t.Fatal(err)
	}

	if read.Raw["command"] != "npm install" {
		t.Errorf("raw command mismatch: %v", read.Raw["command"])
	}
	if read.Raw["exit_code"] != float64(0) {
		t.Errorf("raw exit_code mismatch: %v", read.Raw["exit_code"])
	}
	if read.Raw["null_field"] != nil {
		t.Errorf("raw null_field should be nil, got %v", read.Raw["null_field"])
	}
	if read.Raw["bool_true"] != true {
		t.Errorf("raw bool_true mismatch")
	}
	nested, ok := read.Raw["nested"].(map[string]any)
	if !ok {
		t.Fatal("raw nested is not a map")
	}
	if nested["deep"] != "value" {
		t.Errorf("raw nested.deep mismatch: %v", nested["deep"])
	}
}

func TestCloseThenWrite(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	a.Write(ArchivedEvent{
		IngestedAt:  time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		SessionID:   "sess-1",
		SessionType: "ticket",
		Workdir:     "/tmp/work",
		AgentID:     "sess-1",
		AgentKind:   "claude",
		Status:      "working",
		At:          time.Now(),
		Raw:         map[string]any{},
	})

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a.Write(ArchivedEvent{
		IngestedAt:  time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		SessionID:   "sess-2",
		SessionType: "ticket",
		Workdir:     "/tmp/work",
		AgentID:     "sess-2",
		AgentKind:   "codex",
		Status:      "idle",
		At:          time.Now(),
		Raw:         map[string]any{},
	})

	lines := readArchiveLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line after close (second write dropped), got %d", len(lines))
	}
	var read ArchivedEvent
	if err := json.Unmarshal(lines[0], &read); err != nil {
		t.Fatal(err)
	}
	if read.SessionID != "sess-1" {
		t.Errorf("expected sess-1, got %q", read.SessionID)
	}
}

func TestAppendAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	a1, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a1.Write(ArchivedEvent{
		IngestedAt:  time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		SessionID:   "sess-1",
		SessionType: "ticket",
		Workdir:     "/tmp/work",
		AgentID:     "sess-1",
		AgentKind:   "claude",
		Status:      "working",
		At:          time.Now(),
		Raw:         map[string]any{},
	})
	_ = a1.Close()

	a2, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a2.Close() }()
	a2.Write(ArchivedEvent{
		IngestedAt:  time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		SessionID:   "sess-2",
		SessionType: "freeform",
		Workdir:     "/tmp/work2",
		AgentID:     "sess-2",
		AgentKind:   "codex",
		Status:      "idle",
		At:          time.Now(),
		Raw:         map[string]any{},
	})

	lines := readArchiveLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines after append-across-restart, got %d", len(lines))
	}
}

func TestFullEventRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	record := ArchivedEvent{
		IngestedAt:         now,
		ArchitectKey:       "hiveryn",
		ArchitectWorkspace: "/Users/kareem/architects/hiveryn",
		SessionID:          "sess-full",
		SessionType:        "ticket",
		TicketID:           "T-042",
		ProfileName:        "claude-build",
		Agent:              "claude",
		Workdir:            "/Users/kareem/hiveryn/daemon",
		AdditionalWorkdirs: []string{"/Users/kareem/hiveryn/shared", "/Users/kareem/hiveryn/agentruntime"},
		AdditionalRepos:    []string{"shared", "agentruntime"},
		AgentID:            "sess-full",
		AgentKind:          "claude",
		NativeID:           "abc-123",
		PrimaryNativeID:    "abc-123",
		NativeSessionRole:  "primary",
		Status:             "working",
		Tool:               "bash",
		Message:            "Running npm install",
		At:                 now,
		Metadata:           map[string]string{"version": "1.0", "env": "test"},
		Raw: map[string]any{
			"command": "npm install",
			"stdout":  "added 42 packages",
		},
	}

	a.Write(record)

	lines := readArchiveLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var read ArchivedEvent
	if err := json.Unmarshal(lines[0], &read); err != nil {
		t.Fatal(err)
	}

	if read.IngestedAt.UTC().Format(time.RFC3339) != now.Format(time.RFC3339) {
		t.Errorf("ingested_at mismatch: %v vs %v", read.IngestedAt, now)
	}
	if read.ArchitectKey != "hiveryn" {
		t.Errorf("architect_key mismatch: %q", read.ArchitectKey)
	}
	if read.ArchitectWorkspace != "/Users/kareem/architects/hiveryn" {
		t.Errorf("architect_workspace mismatch: %q", read.ArchitectWorkspace)
	}
	if read.SessionID != "sess-full" {
		t.Errorf("session_id mismatch: %q", read.SessionID)
	}
	if read.SessionType != "ticket" {
		t.Errorf("session_type mismatch: %q", read.SessionType)
	}
	if read.TicketID != "T-042" {
		t.Errorf("ticket_id mismatch: %q", read.TicketID)
	}
	if read.ProfileName != "claude-build" {
		t.Errorf("profile_name mismatch: %q", read.ProfileName)
	}
	if read.Agent != "claude" {
		t.Errorf("agent mismatch: %q", read.Agent)
	}
	if read.Workdir != "/Users/kareem/hiveryn/daemon" {
		t.Errorf("workdir mismatch: %q", read.Workdir)
	}
	if len(read.AdditionalWorkdirs) != 2 {
		t.Errorf("additional_workdirs length mismatch: %d", len(read.AdditionalWorkdirs))
	}
	if read.AdditionalWorkdirs[0] != "/Users/kareem/hiveryn/shared" {
		t.Errorf("additional_workdirs[0] mismatch")
	}
	if len(read.AdditionalRepos) != 2 {
		t.Errorf("additional_repos length mismatch: %d", len(read.AdditionalRepos))
	}
	if read.AdditionalRepos[0] != "shared" {
		t.Errorf("additional_repos[0] mismatch")
	}
	if read.AgentID != "sess-full" {
		t.Errorf("agent_id mismatch: %q", read.AgentID)
	}
	if read.AgentKind != "claude" {
		t.Errorf("agent_kind mismatch: %q", read.AgentKind)
	}
	if read.NativeID != "abc-123" {
		t.Errorf("native_id mismatch: %q", read.NativeID)
	}
	if read.PrimaryNativeID != "abc-123" {
		t.Errorf("primary_native_id mismatch")
	}
	if read.NativeSessionRole != "primary" {
		t.Errorf("native_session_role mismatch: %q", read.NativeSessionRole)
	}
	if read.Status != "working" {
		t.Errorf("status mismatch: %q", read.Status)
	}
	if read.Tool != "bash" {
		t.Errorf("tool mismatch: %q", read.Tool)
	}
	if read.Message != "Running npm install" {
		t.Errorf("message mismatch: %q", read.Message)
	}
	if read.Metadata["version"] != "1.0" {
		t.Errorf("metadata.version mismatch")
	}
	if read.Metadata["env"] != "test" {
		t.Errorf("metadata.env mismatch")
	}
	rawCmd, _ := read.Raw["command"].(string)
	if rawCmd != "npm install" {
		t.Errorf("raw.command mismatch: %q", rawCmd)
	}
}

func readArchiveLines(t *testing.T, dir string) []json.RawMessage {
	t.Helper()
	archiveDir := filepath.Join(dir, archiveSubDir, eventsDir)
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}

	var lines []json.RawMessage
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileLines := readFileLines(t, filepath.Join(archiveDir, entry.Name()))
		lines = append(lines, fileLines...)
	}
	return lines
}

func readFileLines(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var lines []json.RawMessage
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		lines = append(lines, json.RawMessage(line))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}
