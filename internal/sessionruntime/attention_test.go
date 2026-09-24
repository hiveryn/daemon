package sessionruntime

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	artclaude "github.com/hiveryn/agentruntime/adapter/claude"
	artcodex "github.com/hiveryn/agentruntime/adapter/codex"

	"github.com/hiveryn/daemon/internal/domain"
)

type monitorProbe struct {
	monitor *attentionMonitor
	changes atomic.Int32
}

func newMonitorProbe(detector agentruntime.AttentionDetector) *monitorProbe {
	p := &monitorProbe{}
	p.monitor = newAttentionMonitor(detector, "coverage", terminalSize{Cols: 120, Rows: 40}, func() { p.changes.Add(1) })
	p.monitor.quiet = 20 * time.Millisecond
	p.monitor.maxDelay = 100 * time.Millisecond
	return p
}

// feed writes data in small chunks, splitting multibyte runes the way PTY
// reads do.
func (p *monitorProbe) feed(data []byte) {
	for len(data) > 0 {
		n := min(7, len(data))
		p.monitor.Output(data[:n])
		data = data[n:]
	}
}

func (p *monitorProbe) settle(t *testing.T, want domain.ActionAttentionState, reason string) domain.ActionAgentAttention {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := p.monitor.Current()
		if got.State == want && got.Reason == reason {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("attention = %+v, want %s %q", got, want, reason)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readRaw(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The phases are the terminal output of one real Codex 0.156.1 resume after a
// killed turn, launched without --dangerously-bypass-hook-trust: its
// hook-review dialog, which Hiveryn launches never show and which is not
// recognized; then (answered) the resumed conversation with its interrupted
// banner and a message typed but not sent; then that message sent and
// answered.
func TestAttentionMonitorFollowsRealCodexScreens(t *testing.T) {
	p := newMonitorProbe(artcodex.New(artcodex.DefaultOptions()))
	if got := p.monitor.Current(); got.State != domain.ActionAttentionNoneDetected || got.Coverage != "coverage" {
		t.Fatalf("initial attention = %+v", got)
	}

	p.feed(readRaw(t, "codex_resume_phase1.raw"))
	time.Sleep(100 * time.Millisecond)
	if got := p.monitor.Current(); got.State != domain.ActionAttentionNoneDetected {
		t.Fatalf("attention on an unrecognized dialog = %+v", got)
	}

	p.feed(readRaw(t, "codex_resume_phase2.raw"))
	got := p.settle(t, domain.ActionAttentionInputRequired, artcodex.AttentionConversationInterrupted)
	if got.Source != domain.ActionAttentionSourceTerminal || got.Message == "" || got.Since == nil {
		t.Fatalf("interrupted attention = %+v", got)
	}

	p.feed(readRaw(t, "codex_resume_phase3.raw"))
	p.settle(t, domain.ActionAttentionNoneDetected, "")
	time.Sleep(50 * time.Millisecond)
	if n := p.changes.Load(); n != 2 {
		t.Fatalf("changes = %d, want 2 (raised, cleared)", n)
	}
}

// The phases are the terminal output of one real Claude Code 2.1.281
// --resume after the process was killed during a foreground tool call: the
// resumed conversation with its interrupted notice and a message typed but not
// sent; then that message sent and answered.
func TestAttentionMonitorFollowsRealClaudeScreens(t *testing.T) {
	p := newMonitorProbe(artclaude.New(artclaude.DefaultOptions()))
	p.feed(readRaw(t, "claude_resume_phase1.raw"))
	got := p.settle(t, domain.ActionAttentionInputRequired, artclaude.AttentionConversationInterrupted)
	if got.Source != domain.ActionAttentionSourceTerminal || got.Message == "" || got.Since == nil {
		t.Fatalf("interrupted attention = %+v", got)
	}

	p.feed(readRaw(t, "claude_resume_phase2.raw"))
	p.settle(t, domain.ActionAttentionNoneDetected, "")
	time.Sleep(50 * time.Millisecond)
	if n := p.changes.Load(); n != 2 {
		t.Fatalf("changes = %d, want 2 (raised, cleared)", n)
	}
}

// A quiet agent — working, or silent for a long time — is never reported as
// waiting: only a recognized prompt is.
func TestAttentionMonitorDoesNotReportQuietAgent(t *testing.T) {
	p := newMonitorProbe(artcodex.New(artcodex.DefaultOptions()))
	p.feed([]byte("\x1b[2J\x1b[1;1H• Running the comparison\r\n\r\nWorking (5m 02s • esc to interrupt)\r\n\r\n› Ask Codex to do anything\r\n"))
	time.Sleep(200 * time.Millisecond)
	if got := p.monitor.Current(); got.State != domain.ActionAttentionNoneDetected || p.changes.Load() != 0 {
		t.Fatalf("attention of a quiet agent = %+v after %d changes", got, p.changes.Load())
	}
}

// Words drawn with cursor moves between them, as Codex draws its trust
// dialog, are matched on the rendered screen.
func TestAttentionMonitorRendersCursorPositionedText(t *testing.T) {
	p := newMonitorProbe(artcodex.New(artcodex.DefaultOptions()))
	p.feed([]byte("\x1b[1;1H\x1b[J\x1b[2;1H  Folder\x1b[2;10Haccess\x1b[5;3HTrust\x1b[5;9Hthis\x1b[5;14Hfolder?\x1b[8;1H›\x1b[8;3H1.\x1b[8;6HTrust\x1b[8;12Hand\x1b[8;16Hcontinue\x1b[9;3H2.\x1b[9;6HQuit\x1b[11;3Henter\x1b[11;9Hcontinue\x1b[11;18H·\x1b[11;20Hesc\x1b[11;24Hquit"))
	p.settle(t, domain.ActionAttentionInputRequired, artcodex.AttentionFolderTrust)

	// Answering it clears the screen and draws the session: cleared.
	p.feed([]byte("\x1b[1;1H\x1b[J╭──────╮\r\n│ Codex │\r\n╰──────╯\r\n\r\n› Ask Codex to do anything\r\n"))
	p.settle(t, domain.ActionAttentionNoneDetected, "")
}

func TestAttentionMonitorFollowsHookPrompts(t *testing.T) {
	p := newMonitorProbe(nil)
	at := time.Now().UTC()
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "main", Status: agentruntime.StatusAwaitingInput, Message: "permission requested", Tool: "Bash", At: at})
	got := p.monitor.Current()
	if got.State != domain.ActionAttentionInputRequired || got.Source != domain.ActionAttentionSourceHook || got.Reason != attentionReasonAwaitingInput || got.Since == nil || !got.Since.Equal(at) {
		t.Fatalf("hook attention = %+v", got)
	}
	// The same prompt reported again (a follow-up notification) is no change.
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "main", Status: agentruntime.StatusAwaitingInput, Message: "permission requested", Tool: "Bash", At: at.Add(time.Second)})
	// Another native session's progress does not answer this prompt.
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "sub", Status: agentruntime.StatusWorking})
	if got := p.monitor.Current(); got.State != domain.ActionAttentionInputRequired || p.changes.Load() != 1 {
		t.Fatalf("attention after unrelated events = %+v, %d changes", got, p.changes.Load())
	}
	// The prompting session's next status means it was answered.
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "main", Status: agentruntime.StatusWorking})
	if got := p.monitor.Current(); got.State != domain.ActionAttentionNoneDetected || p.changes.Load() != 2 {
		t.Fatalf("attention after answer = %+v, %d changes", got, p.changes.Load())
	}

	// A subagent's prompt is cleared when the agent session ends, and closing
	// reports whether anything was cleared.
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "sub", NativeSessionRole: agentruntime.NativeSessionRoleSubsession, Status: agentruntime.StatusAwaitingInput, Message: "permission requested"})
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "main", NativeSessionRole: agentruntime.NativeSessionRolePrimary, Status: agentruntime.StatusEnded})
	if got := p.monitor.Current(); got.State != domain.ActionAttentionNoneDetected {
		t.Fatalf("attention after session end = %+v", got)
	}
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "main", Status: agentruntime.StatusAwaitingInput, Message: "question"})
	if !p.monitor.Close() {
		t.Fatal("Close did not report the cleared attention")
	}
	if got := p.monitor.Current(); got.State != domain.ActionAttentionNoneDetected {
		t.Fatalf("attention after close = %+v", got)
	}
	p.monitor.ObserveEvent(agentruntime.Event{NativeID: "main", Status: agentruntime.StatusAwaitingInput, Message: "question"})
	if got := p.monitor.Current(); got.State != domain.ActionAttentionNoneDetected || p.monitor.Close() {
		t.Fatalf("a closed monitor kept state: %+v", got)
	}
}

func TestCompleteUTF8Prefix(t *testing.T) {
	box := []byte("ab─") // "─" is three bytes
	for cut := 0; cut <= len(box); cut++ {
		want := cut
		if cut == 3 || cut == 4 {
			want = 2
		}
		if got := completeUTF8Prefix(box[:cut]); got != want {
			t.Errorf("completeUTF8Prefix(%q) = %d, want %d", box[:cut], got, want)
		}
	}
}
