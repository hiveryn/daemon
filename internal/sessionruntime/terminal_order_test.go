package sessionruntime

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"

	"github.com/hiveryn/daemon/internal/domain"
)

// displayed is the feed as a client shows it: latest revision per item,
// ordered by sequence.
func (f *terminalFixture) displayed(sessionID string) []domain.ConversationItem {
	items := f.history2(sessionID)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Sequence < items[j].Sequence })
	return items
}

// The live failure: replies of the launch turn and a later turn were
// persisted before the record yielded the kickoff. The kickoff must be shown
// once, before its own replies, and the turn must stay contiguous.
func TestLateDiscoveredKickoffKeepsNativeOrder(t *testing.T) {
	f := newTerminalFixture(t)
	session := f.terminalSession("a-order", agentruntime.AgentCodex)
	f.repo.listedSessions = []domain.Session{session}
	h := f.history[agentruntime.AgentCodex]
	t0 := time.Date(2026, 10, 27, 4, 53, 0, 0, time.UTC)
	kickoff := agentruntime.HistoryItem{ID: "kick", TurnID: "T1", Role: "user", NativeOrigin: "user", Text: session.Prompt + "\n\n<document name=\"OVERVIEW.md\">x</document>", At: t0}
	replies := []agentruntime.HistoryItem{
		{ID: "r1", TurnID: "T1", Role: "assistant", Text: "reading", At: t0.Add(3 * time.Second)},
		{ID: "tool1", TurnID: "T1", Role: "tool", Tool: "exec", Status: "completed", Text: "ok", At: t0.Add(4 * time.Second)},
		{ID: "end1", TurnID: "T1", Role: "system", Status: "completed", TurnEnd: true, At: t0.Add(5 * time.Second)},
		{ID: "u2", TurnID: "T2", Role: "user", NativeOrigin: "user", Text: "typed", At: t0.Add(time.Minute)},
		{ID: "r2", TurnID: "T2", Role: "assistant", Text: "answer", At: t0.Add(time.Minute)},
	}
	h.history.Items = replies
	if _, err := f.service.syncTerminalHistory(context.Background(), session, true); err != nil {
		t.Fatal(err)
	}
	before := f.displayed(session.ID)
	h.history.Items = append([]agentruntime.HistoryItem{kickoff}, replies...)
	if _, err := f.service.syncTerminalHistory(context.Background(), session, true); err != nil {
		t.Fatal(err)
	}
	got := f.displayed(session.ID)
	var order []string
	for _, item := range got {
		order = append(order, item.ID)
	}
	if strings.Join(order, ",") != "kick,r1,tool1,end1,u2,r2" {
		t.Fatalf("display order = %v, want the kickoff before its replies", order)
	}
	if got[0].Via != "kickoff" || got[0].Text != kickoff.Text {
		t.Fatalf("kickoff = %#v", got[0])
	}
	for i, item := range got[1:4] {
		if item.Origin != "kickoff" || item.Sequence != before[i].Sequence {
			t.Fatalf("turn item %#v: want kickoff provenance at its original position %d", item, before[i].Sequence)
		}
	}
	if got[4].Origin != "user" || got[4].Via != "terminal" {
		t.Fatalf("later turn provenance changed: %#v", got[4])
	}
}
