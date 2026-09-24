package sessionruntime

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hinshun/vt10x"
	"github.com/hiveryn/agentruntime"

	"github.com/hiveryn/daemon/internal/domain"
)

const (
	// The screen is inspected once output has been quiet for attentionQuiet,
	// so a prompt is judged on a settled frame rather than mid-redraw, and at
	// least every attentionMaxDelay while output keeps streaming (spinners
	// never go quiet), so a prompt that clears is noticed while work runs.
	attentionQuiet    = 300 * time.Millisecond
	attentionMaxDelay = time.Second

	// attentionReasonAwaitingInput is the reason for a hook-reported prompt;
	// the event's own message says which prompt.
	attentionReasonAwaitingInput = "awaiting_input"
)

// attentionMonitor derives one Action agent's attention from explicit
// provider evidence only: prompts its hooks report as awaiting input, and
// prompts the provider's adapter recognizes on the agent's rendered terminal
// screen. It never infers a wait from idleness, silence or a missing hook.
//
// A monitor belongs to one main terminal: it is created with it and closed
// when that terminal is replaced (restore, resume after exit) or the session
// ends, so a killed agent's attention is never carried over. It renders the
// terminal through a headless emulator it feeds from the PTY output; it
// never writes to the terminal, so it never answers or dismisses a prompt.
type attentionMonitor struct {
	detector agentruntime.AttentionDetector // nil: nothing is recognized on screen
	coverage string
	onChange func()
	quiet    time.Duration
	maxDelay time.Duration

	mu         sync.Mutex
	closed     bool
	vt         vt10x.Terminal
	partial    []byte // an incomplete trailing UTF-8 rune, held for the next chunk
	timer      *time.Timer
	dirtySince time.Time
	lastOutput time.Time
	screen     *detectedAttention
	hooks      map[string]detectedAttention // by native session id
}

type detectedAttention struct {
	reason  string
	message string
	source  domain.ActionAttentionSource
	since   time.Time
}

func newAttentionMonitor(detector agentruntime.AttentionDetector, coverage string, size terminalSize, onChange func()) *attentionMonitor {
	if size.Cols == 0 {
		size.Cols = defaultPTYCols
	}
	if size.Rows == 0 {
		size.Rows = defaultPTYRows
	}
	return &attentionMonitor{
		detector: detector,
		coverage: coverage,
		onChange: onChange,
		quiet:    attentionQuiet,
		maxDelay: attentionMaxDelay,
		vt:       vt10x.New(vt10x.WithSize(int(size.Cols), int(size.Rows))),
		hooks:    map[string]detectedAttention{},
	}
}

// Output feeds terminal output to the emulator and schedules an inspection.
func (m *attentionMonitor) Output(chunk []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.detector == nil {
		return
	}
	// vt10x drops bytes of a rune split across writes, and PTY reads split
	// runes routinely (box drawing is three bytes), so hold the tail back.
	data := make([]byte, 0, len(m.partial)+len(chunk))
	data = append(append(data, m.partial...), chunk...)
	cut := completeUTF8Prefix(data)
	m.partial = append([]byte(nil), data[cut:]...)
	_, _ = m.vt.Write(data[:cut])
	m.scheduleLocked()
}

// Resize follows the terminal's size, which the agent redraws for.
func (m *attentionMonitor) Resize(cols, rows uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.detector == nil {
		return
	}
	m.vt.Resize(int(cols), int(rows))
	m.scheduleLocked()
}

func (m *attentionMonitor) scheduleLocked() {
	now := time.Now()
	m.lastOutput = now
	if m.timer == nil {
		m.dirtySince = now
		m.timer = time.AfterFunc(m.quiet, m.inspect)
	}
}

func (m *attentionMonitor) inspect() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	if quietFor := now.Sub(m.lastOutput); quietFor < m.quiet && now.Sub(m.dirtySince) < m.maxDelay {
		m.timer.Reset(m.quiet - quietFor)
		m.mu.Unlock()
		return
	}
	m.timer = nil
	found := m.detector.InspectScreen(strings.Split(m.vt.String(), "\n"))
	changed := false
	switch {
	case found == nil:
		changed = m.screen != nil
		m.screen = nil
	case m.screen == nil || m.screen.reason != found.Reason:
		m.screen = &detectedAttention{reason: found.Reason, message: found.Message, source: domain.ActionAttentionSourceTerminal, since: now.UTC()}
		changed = true
	}
	m.mu.Unlock()
	if changed {
		m.onChange()
	}
}

// ObserveEvent follows the agent's hook events: a prompt reported awaiting
// input stands until the same native session reports anything else (the
// prompt was answered), or the agent session ends.
func (m *attentionMonitor) ObserveEvent(event agentruntime.Event) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	changed := false
	switch {
	case event.Status == agentruntime.StatusAwaitingInput:
		prompt := event.Message
		if event.Tool != "" {
			prompt += " (" + event.Tool + ")"
		}
		message := "The agent reports a prompt waiting for the user: " + prompt + "."
		previous, ok := m.hooks[event.NativeID]
		if !ok || previous.message != message {
			since := event.At.UTC()
			if since.IsZero() {
				since = time.Now().UTC()
			}
			m.hooks[event.NativeID] = detectedAttention{reason: attentionReasonAwaitingInput, message: message, source: domain.ActionAttentionSourceHook, since: since}
			changed = true
		}
	case event.Status == agentruntime.StatusEnded && event.NativeSessionRole != agentruntime.NativeSessionRoleSubsession:
		changed = len(m.hooks) > 0
		clear(m.hooks)
	default:
		if _, ok := m.hooks[event.NativeID]; ok {
			delete(m.hooks, event.NativeID)
			changed = true
		}
	}
	m.mu.Unlock()
	if changed {
		m.onChange()
	}
}

// Current is the agent's attention now. What the screen shows wins over a
// hook report: it is what the user sees at the terminal.
func (m *attentionMonitor) Current() domain.ActionAgentAttention {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := m.screen
	for _, hook := range m.hooks {
		if found == nil || (found.source == domain.ActionAttentionSourceHook && hook.since.After(found.since)) {
			found = &hook
		}
	}
	if found == nil {
		return domain.ActionAgentAttention{State: domain.ActionAttentionNoneDetected, Coverage: m.coverage}
	}
	since := found.since
	return domain.ActionAgentAttention{
		State:    domain.ActionAttentionInputRequired,
		Reason:   found.reason,
		Message:  found.message,
		Source:   found.source,
		Since:    &since,
		Coverage: m.coverage,
	}
}

// Close stops the monitor and forgets what it detected. It reports whether
// input was required, i.e. whether closing clears a reported attention.
func (m *attentionMonitor) Close() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.closed = true
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	required := m.screen != nil || len(m.hooks) > 0
	m.screen = nil
	clear(m.hooks)
	return required
}

// completeUTF8Prefix returns the length of data without an incomplete
// trailing rune.
func completeUTF8Prefix(data []byte) int {
	for back := 1; back <= utf8.UTFMax && back <= len(data); back++ {
		start := len(data) - back
		if utf8.RuneStart(data[start]) {
			if utf8.FullRune(data[start:]) {
				return len(data)
			}
			return start
		}
	}
	return len(data)
}

// attentionKey is what makes an attention change worth waking a waiter:
// input_required appearing, clearing or changing reason. The none_detected /
// unavailable distinction and detection times are not.
func attentionKey(a domain.ActionAgentAttention) string {
	if a.State != domain.ActionAttentionInputRequired {
		return ""
	}
	return string(a.Source) + ":" + a.Reason + ":" + a.Message
}
