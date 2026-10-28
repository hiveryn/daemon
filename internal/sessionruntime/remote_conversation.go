package sessionruntime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

// Conversations of remote (SSH) terminal workers.
//
// Nothing Hiveryn-specific runs on the remote machine. The daemon reaches the
// worker's native conversation with ordinary SSH commands against the
// installed provider CLI and the account's own provider home:
//
//   - History: the provider's record files (Claude transcript, Codex rollout
//     segments, located by agentruntime's RecordLocator under the root the
//     frozen launch environment selects there) are mirrored incrementally into
//     a private local directory — only appended bytes travel, with a 64-byte
//     overlap that detects a rewritten file and refetches it — and read by the
//     unchanged agentruntime readers. The laptop's provider home is never read
//     for a remote run.
//   - Claude input: the asyncRewake waiter long-polls through the worker's
//     reverse tunnel to its authenticated gateway (remote_gateway.go), so the
//     same idle-gated hand-off as a local session applies.
//   - Codex input: `codex app-server` runs over SSH with the frozen variant
//     environment applied there; thread/queue/add and queue checks are the
//     same JSON-RPC calls as locally.
//   - Remote OpenCode is unsupported: its database and session server live on
//     the remote machine and no SSH route to them is provided.
//
// Every command of a session's conversation runs over one OpenSSH master that
// is opened on first use, replaced when it dies and closed after
// remoteConversationIdle without use or when the session ends, so polling does
// not pay a handshake per read.

// remoteConversationIdle closes a session's conversation SSH master unused this long.
var remoteConversationIdle = 2 * time.Minute

// remoteHistoryBound bounds one remote history read (all its SSH calls).
var remoteHistoryBound = 20 * time.Second

// remoteRecordChunk bounds the bytes one SSH call returns; a larger backlog is
// fetched over further calls of the same read (a variable so tests can shrink it).
var remoteRecordChunk int64 = 8 << 20

const (
	// remoteRecordMax bounds the mirrored record of one session.
	remoteRecordMax = 512 << 20
	// remoteRecordOverlap is how many already-mirrored bytes each call
	// re-reads to detect a file rewritten in place.
	remoteRecordOverlap = 64
	// remoteQueueRecheck spaces Codex queue checks of one open remote input.
	remoteQueueRecheck = 15 * time.Second
)

type remoteConversation struct {
	mu    sync.Mutex
	alias string
	conn  *remoteexec.Connection
	inUse int
	used  time.Time
	timer *time.Timer
	// mirror state (guarded by mirrorMu; reads are already serialized per
	// session by terminalHistoryState).
	mirrorMu sync.Mutex
	dir      string
	files    map[string]int64
	// queueChecked spaces Codex queue checks per submission; queued holds
	// the last observed answer, reused until the next check is due.
	queueChecked map[string]time.Time
	queued       map[string]bool
}

func (s *Service) remoteConversation(session domain.Session) *remoteConversation {
	s.remoteConvMu.Lock()
	defer s.remoteConvMu.Unlock()
	if s.remoteConvs == nil {
		s.remoteConvs = map[string]*remoteConversation{}
	}
	rc := s.remoteConvs[session.ID]
	if rc == nil {
		rc = &remoteConversation{alias: session.SSH, files: map[string]int64{}, queueChecked: map[string]time.Time{}, queued: map[string]bool{}}
		s.remoteConvs[session.ID] = rc
	}
	return rc
}

// withRemoteConversation runs fn with a context carrying the session's
// conversation SSH master, establishing (or replacing) it first. A master that
// cannot be established is a NotDelivered error: nothing ran on the remote
// machine.
func (s *Service) withRemoteConversation(ctx context.Context, session domain.Session, fn func(context.Context) error) error {
	if session.SSH == "" {
		return fmt.Errorf("session %s has no SSH alias for machine %s", session.ID, config.MachineLabel(session.Machine))
	}
	rc := s.remoteConversation(session)
	rc.mu.Lock()
	if rc.conn == nil || !rc.conn.Alive() {
		if rc.conn != nil {
			rc.conn.Close()
			rc.conn = nil
		}
		started := time.Now()
		conn, err := remoteexec.Dial(ctx, rc.alias)
		if err != nil {
			rc.mu.Unlock()
			conn.Close()
			// Nothing ran on the remote machine.
			return agentruntime.NotDelivered(fmt.Errorf("SSH connection to %s for the conversation: %w", config.MachineLabel(session.Machine), err))
		}
		s.logger.Info("remote conversation connection established", "session_id", session.ID, "machine", session.Machine, "duration", time.Since(started).String())
		rc.conn = conn
	}
	conn := rc.conn
	rc.inUse++
	rc.mu.Unlock()
	defer func() {
		rc.mu.Lock()
		rc.inUse--
		rc.used = time.Now()
		if rc.timer == nil {
			rc.timer = time.AfterFunc(remoteConversationIdle, func() { rc.closeIdle() })
		} else {
			rc.timer.Reset(remoteConversationIdle)
		}
		rc.mu.Unlock()
	}()
	return fn(remoteexec.WithConnection(ctx, conn))
}

// closeIdle closes the master once nothing has used it for the idle period.
func (rc *remoteConversation) closeIdle() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.conn == nil || rc.inUse > 0 {
		return
	}
	if wait := remoteConversationIdle - time.Since(rc.used); wait > 0 {
		rc.timer.Reset(wait)
		return
	}
	rc.conn.Close()
	rc.conn = nil
}

// closeRemoteConversation stops the session's conversation master and removes
// its record mirror (session end, daemon shutdown).
func (s *Service) closeRemoteConversation(id string) {
	s.remoteConvMu.Lock()
	rc := s.remoteConvs[id]
	delete(s.remoteConvs, id)
	s.remoteConvMu.Unlock()
	if rc == nil {
		return
	}
	rc.mu.Lock()
	if rc.timer != nil {
		rc.timer.Stop()
	}
	if rc.conn != nil {
		rc.conn.Close()
		rc.conn = nil
	}
	rc.mu.Unlock()
	rc.mirrorMu.Lock()
	if rc.dir != "" {
		if err := os.RemoveAll(rc.dir); err != nil {
			s.logger.Warn("remove remote record mirror", "session_id", id, "error", err)
		}
		rc.dir, rc.files = "", map[string]int64{}
	}
	rc.mirrorMu.Unlock()
}

// remoteConversationUnsupported names why a remote session has no conversation
// route ("" when it has one).
func (s *Service) remoteConversationUnsupported(session domain.Session, kind agentruntime.AgentKind) string {
	if session.Machine == "" {
		return ""
	}
	if session.InteractionMode == domain.InteractionChat {
		return "Remote SSH chat sessions are unsupported"
	}
	adapter := s.adapters[kind]
	_, locates := adapter.(agentruntime.RecordLocator)
	_, requests := adapter.(agentruntime.HistoryRecordRequester)
	if kind == agentruntime.AgentOpenCode || !locates || !requests {
		return fmt.Sprintf("Remote %s conversations are unsupported: the daemon reaches remote workers only through SSH and the provider CLI, and %s's record and session server on %s have no such route. Use the worker's terminal", kind, kind, config.MachineLabel(session.Machine))
	}
	return ""
}

// nativeIDPattern is what a native id must look like before it enters a
// remote glob: no glob, quote or path characters.
var nativeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// readRemoteRecord mirrors the run's native record from its machine and reads
// the mirror with the provider's own reader.
func (s *Service) readRemoteRecord(ctx context.Context, session domain.Session, kind agentruntime.AgentKind, adapter agentruntime.Adapter, reader agentruntime.HistoryReader) (agentruntime.History, error) {
	if reason := s.remoteConversationUnsupported(session, kind); reason != "" {
		return agentruntime.History{}, &domain.ConversationError{Code: domain.HistoryUnsupported, Message: reason}
	}
	run := session.CurrentRun
	if !nativeIDPattern.MatchString(run.NativeID) {
		return agentruntime.History{}, fmt.Errorf("native session id %q is not a plain identifier; the remote record is not looked up", run.NativeID)
	}
	location := adapter.(agentruntime.RecordLocator).RecordLocation(run.NativeID)
	ctx, cancel := context.WithTimeout(ctx, remoteHistoryBound)
	defer cancel()
	rc := s.remoteConversation(session)
	var files []string
	err := s.withRemoteConversation(ctx, session, func(ctx context.Context) error {
		var err error
		files, err = rc.syncMirror(ctx, session.SSH, location, run.ProfileSnapshot.Env)
		return err
	})
	if err != nil {
		return agentruntime.History{}, fmt.Errorf("read the native record on %s: %w", config.MachineLabel(session.Machine), err)
	}
	rc.mirrorMu.Lock()
	dir := rc.dir
	rc.mirrorMu.Unlock()
	req, err := adapter.(agentruntime.HistoryRecordRequester).HistoryRecordRequest(run.NativeID, dir, files)
	if err != nil {
		return agentruntime.History{}, fmt.Errorf("native record on %s: %w", config.MachineLabel(session.Machine), err)
	}
	return reader.ReadHistory(ctx, req)
}

// remoteRecordRoot is the shell expression of the provider root on the remote
// machine: the frozen launch environment's value there, else the default under
// the remote account's home. Never a laptop path.
func remoteRecordRoot(location agentruntime.RecordLocation, env map[string]string) string {
	if root := env[location.RootEnv]; root != "" {
		return remoteexec.Quote(root)
	}
	return `"$HOME"/` + remoteexec.Quote(location.DefaultRoot)
}

// syncMirror brings the local mirror up to date with the remote record files
// and returns their relative paths. It fetches appended bytes only, verifies
// a 64-byte overlap with what is already mirrored and refetches a file whose
// content changed underneath.
func (rc *remoteConversation) syncMirror(ctx context.Context, alias string, location agentruntime.RecordLocation, env map[string]string) ([]string, error) {
	rc.mirrorMu.Lock()
	defer rc.mirrorMu.Unlock()
	if rc.dir == "" {
		dir, err := os.MkdirTemp("", "hiveryn-record-")
		if err != nil {
			return nil, fmt.Errorf("record mirror directory: %w", err)
		}
		rc.dir = dir
	}
	for pass := 0; ; pass++ {
		if pass >= 256 {
			return nil, fmt.Errorf("the remote record kept changing during the read")
		}
		again, err := rc.fetchRecord(ctx, alias, location, env)
		if err != nil {
			return nil, err
		}
		if !again {
			break
		}
	}
	files := make([]string, 0, len(rc.files))
	for rel := range rc.files {
		files = append(files, rel)
	}
	sort.Strings(files)
	return files, nil
}

// remoteRecordScript lists the record files under root and prints, per file, a
// header (path, size, the mirrored offset it continues from, the first byte it
// sends, the byte count) followed by those bytes: up to 64 already-mirrored
// bytes, then new bytes within one chunk budget shared by all files.
func remoteRecordScript(root string, patterns []string, known map[string]int64) string {
	var b strings.Builder
	b.WriteString("set -u\nexport LC_ALL=C\n")
	b.WriteString("cd " + root + " 2>/dev/null || { echo 'provider record root not found' >&2; exit 3; }\n")
	b.WriteString("set --")
	rels := make([]string, 0, len(known))
	for rel := range known {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		b.WriteString(" " + remoteexec.Quote(rel) + " " + strconv.FormatInt(known[rel], 10))
	}
	b.WriteString("\n")
	b.WriteString(`known() { want=$1; shift; while [ $# -ge 2 ]; do if [ "$1" = "$want" ]; then echo "$2"; return; fi; shift 2; done; echo -1; }` + "\n")
	fmt.Fprintf(&b, "budget=%d\n", remoteRecordChunk)
	// Patterns are fixed provider layouts around a validated native id, so
	// they are left unquoted to glob.
	b.WriteString("for f in " + strings.Join(patterns, " ") + "; do\n")
	b.WriteString(`  [ -f "$f" ] || continue
  size=$(wc -c < "$f") || exit 4
  size=$((size + 0))
  off=$(known "$f" "$@")
  if [ "$off" -lt 0 ] || [ "$off" -gt "$size" ]; then off=0; fi
  from=$((off - ` + strconv.Itoa(remoteRecordOverlap) + `)); if [ "$from" -lt 0 ]; then from=0; fi
  new=$((size - off)); if [ "$new" -gt "$budget" ]; then new=$budget; fi
  budget=$((budget - new))
  len=$((off - from + new))
  printf 'F\n%s\n%s\n%s\n%s\n%s\n' "$f" "$size" "$off" "$from" "$len"
  if [ "$len" -gt 0 ]; then tail -c +$((from + 1)) "$f" | head -c "$len" || exit 5; fi
done
printf 'E\n'
`)
	return b.String()
}

// fetchRecord runs one remote call and applies it to the mirror; again reports
// that more bytes are pending (a chunk budget ran out or a rewrite was found).
func (rc *remoteConversation) fetchRecord(ctx context.Context, alias string, location agentruntime.RecordLocation, env map[string]string) (bool, error) {
	out, err := remoteexec.Run(ctx, alias, remoteRecordScript(remoteRecordRoot(location, env), location.Patterns, rc.files), nil)
	if err != nil {
		return false, err
	}
	r := bufio.NewReader(bytes.NewReader(out))
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("truncated remote record listing: %w", err)
		}
		return strings.TrimSuffix(s, "\n"), nil
	}
	number := func() (int64, error) {
		s, err := line()
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(s, 10, 64)
	}
	again := false
	seen := map[string]bool{}
	var total int64
	for {
		tag, err := line()
		if err != nil {
			return false, err
		}
		if tag == "E" {
			break
		}
		if tag != "F" {
			return false, fmt.Errorf("unexpected remote record listing entry %q", tag)
		}
		rel, err := line()
		if err != nil {
			return false, err
		}
		var size, off, from, n int64
		for _, v := range []*int64{&size, &off, &from, &n} {
			if *v, err = number(); err != nil {
				return false, fmt.Errorf("remote record header of %s: %w", rel, err)
			}
		}
		if !recordPathAllowed(rel, location.Patterns) {
			return false, fmt.Errorf("remote record listing named an unexpected path %q", rel)
		}
		if n < 0 || from < 0 || off < from || size < from+n || n > remoteRecordChunk+remoteRecordOverlap {
			return false, fmt.Errorf("inconsistent remote record header for %s", rel)
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			// The file shrank between its size and its bytes: refetch.
			delete(rc.files, rel)
			return false, fmt.Errorf("remote record %s changed during the read: %w", rel, err)
		}
		seen[rel] = true
		total += size
		if total > remoteRecordMax {
			return false, fmt.Errorf("the remote record exceeds %d MiB; it is not mirrored", remoteRecordMax>>20)
		}
		more, err := rc.applyRecord(rel, size, off, from, data)
		if err != nil {
			return false, err
		}
		again = again || more
	}
	for rel := range rc.files {
		if !seen[rel] {
			delete(rc.files, rel)
			_ = os.Remove(filepath.Join(rc.dir, filepath.FromSlash(rel)))
		}
	}
	return again, nil
}

// applyRecord writes one file's bytes into the mirror.
func (rc *remoteConversation) applyRecord(rel string, size, off, from int64, data []byte) (bool, error) {
	local := filepath.Join(rc.dir, filepath.FromSlash(rel))
	mirrored, known := rc.files[rel]
	if !known || mirrored != off {
		// New, or the remote restarted it from zero (it is shorter than what
		// was mirrored).
		mirrored, off = 0, 0
		if from != 0 {
			delete(rc.files, rel)
			return true, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return false, fmt.Errorf("record mirror: %w", err)
	}
	flags := os.O_RDWR | os.O_CREATE
	if mirrored == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(local, flags, 0o600)
	if err != nil {
		return false, fmt.Errorf("record mirror: %w", err)
	}
	defer func() { _ = f.Close() }()
	overlap := off - from
	if overlap > 0 {
		have := make([]byte, overlap)
		if _, err := f.ReadAt(have, from); err != nil || !bytes.Equal(have, data[:overlap]) {
			// Rewritten in place: refetch it from the start.
			delete(rc.files, rel)
			return true, nil
		}
	}
	if _, err := f.WriteAt(data[overlap:], off); err != nil {
		return false, fmt.Errorf("record mirror: %w", err)
	}
	end := from + int64(len(data))
	if err := f.Truncate(end); err != nil {
		return false, fmt.Errorf("record mirror: %w", err)
	}
	rc.files[rel] = end
	return end < size, nil
}

// recordPathAllowed accepts only a clean relative path matching a pattern.
func recordPathAllowed(rel string, patterns []string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || path.Clean(rel) != rel || strings.Contains(rel, "..") || strings.ContainsAny(rel, "\n\r\x00") {
		return false
	}
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, rel); err == nil && ok {
			return true
		}
	}
	return false
}

// remoteProviderCommand builds a provider helper command (Codex app-server)
// that runs on the session's machine over the conversation master the context
// carries, with the frozen launch environment applied there and nothing of
// the daemon's own environment. stdin closing (or the SSH channel ending)
// ends it.
func remoteProviderCommand(alias string) agentruntime.CommandFunc {
	return func(ctx context.Context, command string, args []string, env map[string]string) (*exec.Cmd, error) {
		if command == "" || strings.ContainsAny(command, "\n\x00") {
			return nil, fmt.Errorf("invalid provider command %q", command)
		}
		script := "cd / || exit 1\n"
		for _, key := range configKeys(env) {
			if !envKeyPattern.MatchString(key) {
				return nil, fmt.Errorf("invalid environment variable name %q", key)
			}
			script += "export " + remoteexec.Quote(key+"="+env[key]) + "\n"
		}
		script += "exec " + remoteexec.Args(append([]string{command}, args...)...) + "\n"
		return remoteexec.Command(ctx, alias, script), nil
	}
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// remoteConnectionState is the SSH attachment state of a remote worker
// ("connected", "disconnected", "missing"), or "" for a local session or one
// this daemon process does not hold.
func (s *Service) remoteConnectionState(session domain.Session) string {
	if session.Machine == "" {
		return ""
	}
	s.remoteMu.Lock()
	w := s.remotes[session.ID]
	s.remoteMu.Unlock()
	if w == nil {
		return "not attached by this daemon process"
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connection
}

// remoteMessageQueued asks the remote Codex queue whether it still holds the
// input. Each check starts an app-server over SSH, so checks of one input are
// spaced by remoteQueueRecheck and the last answer stands in between (never a
// skipped check read as "not queued"); a failed check keeps the last answer.
func (s *Service) remoteMessageQueued(ctx context.Context, session domain.Session, submissionID string, check func(context.Context) (bool, error)) bool {
	rc := s.remoteConversation(session)
	rc.mu.Lock()
	last, checked := rc.queueChecked[submissionID]
	previous := rc.queued[submissionID]
	if checked && time.Since(last) < remoteQueueRecheck {
		rc.mu.Unlock()
		return previous
	}
	rc.queueChecked[submissionID] = time.Now()
	rc.mu.Unlock()
	queued := false
	err := s.withRemoteConversation(ctx, session, func(ctx context.Context) error {
		var err error
		queued, err = check(ctx)
		return err
	})
	if err != nil {
		s.logger.Warn("remote codex queue check", "session_id", session.ID, "submission_id", submissionID, "error", err)
		return previous
	}
	rc.mu.Lock()
	rc.queued[submissionID] = queued
	rc.mu.Unlock()
	return queued
}

// historyError turns a native record failure into the conversation API's
// explicit read error, keeping the original cause.
func historyError(err error) error {
	var conv *domain.ConversationError
	if errors.As(err, &conv) {
		return err
	}
	return &domain.ConversationError{Code: domain.HistoryReadFailed, Message: err.Error()}
}
