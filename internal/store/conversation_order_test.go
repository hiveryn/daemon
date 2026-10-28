package store

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// feedClient is a conversation reader the way clients consume the version
// feed: small pages bounded by a snapshot, upsert by greatest revision,
// display by sequence (ties by id).
type feedClient struct {
	items  map[string]domain.ConversationItem
	cursor int64
}

func (c *feedClient) poll(t *testing.T, repo *SessionStore) {
	t.Helper()
	if c.items == nil {
		c.items = map[string]domain.ConversationItem{}
	}
	ctx := context.Background()
	page, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{AfterRevision: c.cursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := page.SnapshotRevision
	for {
		for _, item := range page.Items {
			if existing, ok := c.items[item.ID]; !ok || existing.Revision < item.Revision {
				c.items[item.ID] = item
			}
		}
		if !page.HasMore {
			break
		}
		if page, err = repo.ReadConversation(ctx, "s", domain.ConversationQuery{AfterRevision: page.NextRevision, SnapshotRevision: snapshot, Limit: 2}); err != nil {
			t.Fatal(err)
		}
	}
	c.cursor = snapshot
}

func (c *feedClient) order() []domain.ConversationItem {
	out := make([]domain.ConversationItem, 0, len(c.items))
	for _, item := range c.items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sequence != out[j].Sequence {
			return out[i].Sequence < out[j].Sequence
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func ids(items []domain.ConversationItem) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = item.ID
	}
	return strings.Join(parts, ",")
}

// fresh is a new client (a page reload) reading the whole feed.
func fresh(t *testing.T, repo *SessionStore) []domain.ConversationItem {
	t.Helper()
	var c feedClient
	c.poll(t, repo)
	return c.order()
}

func orderRepo(t *testing.T) *SessionStore {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSessionStore(db)
}

func put(t *testing.T, repo *SessionStore, items ...domain.ConversationItem) {
	t.Helper()
	if err := repo.PutConversationItems(context.Background(), "s", items); err != nil {
		t.Fatal(err)
	}
}

func checkOrder(t *testing.T, label string, items []domain.ConversationItem, want string) {
	t.Helper()
	if got := ids(items); got != want {
		t.Fatalf("%s order = %s, want %s", label, got, want)
	}
	for i := 1; i < len(items); i++ {
		if items[i].Sequence <= items[i-1].Sequence {
			t.Fatalf("%s: sequences not strictly increasing: %#v", label, items)
		}
	}
}

// The live failure: a kickoff an earlier daemon hid was first persisted after
// its turn's replies and a later turn, so it sat at the end. The next native
// read moves it before its replies; replies keep their positions.
func TestLateKickoffIsPlacedBeforeItsReplies(t *testing.T) {
	repo := orderRepo(t)
	t0 := time.Date(2026, 10, 27, 4, 53, 0, 0, time.UTC)
	kick := domain.ConversationItem{ID: "kick", NativeTurnID: "T1", Role: "user", Origin: "kickoff", Via: "kickoff", Text: "launch", At: t0}
	r1 := domain.ConversationItem{ID: "r1", NativeTurnID: "T1", Role: "assistant", Origin: "user", Text: "on it", At: t0.Add(3 * time.Second)}
	tool := domain.ConversationItem{ID: "tool", NativeTurnID: "T1", Role: "tool", Origin: "user", Tool: "exec", Status: "started", At: t0.Add(4 * time.Second)}
	u2 := domain.ConversationItem{ID: "u2", NativeTurnID: "T2", Role: "user", Origin: "user", Via: "terminal", Text: "next", At: t0.Add(time.Minute)}
	a2 := domain.ConversationItem{ID: "a2", NativeTurnID: "T2", Role: "assistant", Origin: "user", Text: "done", At: t0.Add(time.Minute + time.Second)}

	// The kickoff-hiding daemon persisted the record without it.
	put(t, repo, r1, tool, u2, a2)
	var client feedClient
	client.poll(t, repo)
	checkOrder(t, "before", client.order(), "r1,tool,u2,a2")

	// A daemon that showed the kickoff but froze first-seen positions
	// appended it (and added provenance to its turn).
	put(t, repo, kick)
	client.poll(t, repo)
	checkOrder(t, "late", client.order(), "r1,tool,u2,a2,kick")
	positions := map[string]int64{}
	for _, item := range client.order() {
		positions[item.ID] = item.Sequence
	}

	// The native record in order, with the turn's provenance and a tool update.
	r1k, toolk := r1, tool
	r1k.Origin, toolk.Origin, toolk.Status, toolk.Text = "kickoff", "kickoff", "completed", "output"
	put(t, repo, kick, r1k, toolk, u2, a2)
	client.poll(t, repo)
	got := client.order()
	checkOrder(t, "incremental", got, "kick,r1,tool,u2,a2")
	checkOrder(t, "fresh", fresh(t, repo), "kick,r1,tool,u2,a2")
	for _, item := range got {
		if item.ID != "kick" && item.Sequence != positions[item.ID] {
			t.Fatalf("%s moved from %d to %d", item.ID, positions[item.ID], item.Sequence)
		}
		if item.NativeTurnID == "T1" && item.Origin != "kickoff" {
			t.Fatalf("%s lost kickoff provenance: %#v", item.ID, item)
		}
	}
	if got[2].Status != "completed" || got[2].Text != "output" || got[2].Tool != "exec" {
		t.Fatalf("tool update lost: %#v", got[2])
	}
	if got[0].Text != "launch" || !got[0].At.Equal(t0) {
		t.Fatalf("kickoff content changed: %#v", got[0])
	}

	// A repeated read of the same record changes nothing.
	snapshot := client.cursor
	put(t, repo, kick, r1k, toolk, u2, a2)
	page, err := repo.ReadConversation(context.Background(), "s", domain.ConversationQuery{AfterRevision: snapshot})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("idempotent put added revisions: %#v %v", page, err)
	}
}

// A late input in the middle is placed after its native predecessor (the end
// of the previous turn), so its own replies follow it and turns stay apart.
func TestLateInputJoinsItsTurnAtTheBoundary(t *testing.T) {
	repo := orderRepo(t)
	u1 := domain.ConversationItem{ID: "u1", NativeTurnID: "T1", Role: "user", Via: "terminal"}
	a1 := domain.ConversationItem{ID: "a1", NativeTurnID: "T1", Role: "assistant"}
	u2 := domain.ConversationItem{ID: "u2", NativeTurnID: "T2", Role: "user", Via: "notification", Origin: "notification"}
	a2 := domain.ConversationItem{ID: "a2", NativeTurnID: "T2", Role: "assistant"}
	u3 := domain.ConversationItem{ID: "u3", NativeTurnID: "T3", Role: "user", Via: "api"}
	a3 := domain.ConversationItem{ID: "a3", NativeTurnID: "T3", Role: "assistant"}
	put(t, repo, u1, a1, a2, u3, a3)
	var client feedClient
	client.poll(t, repo)
	put(t, repo, u1, a1, u2, a2, u3, a3)
	client.poll(t, repo)
	checkOrder(t, "incremental", client.order(), "u1,a1,u2,a2,u3,a3")
	checkOrder(t, "fresh", fresh(t, repo), "u1,a1,u2,a2,u3,a3")
}

// Timestamps never order the feed: equal, missing and out-of-order times all
// follow native record order.
func TestNativeOrderIgnoresTimestamps(t *testing.T) {
	repo := orderRepo(t)
	same := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	put(t, repo,
		domain.ConversationItem{ID: "z", Role: "user", At: same},
		domain.ConversationItem{ID: "y", Role: "assistant", At: same},
		domain.ConversationItem{ID: "x", Role: "tool"},
		domain.ConversationItem{ID: "w", Role: "assistant", At: same.Add(-time.Hour)},
	)
	checkOrder(t, "first", fresh(t, repo), "z,y,x,w")
	put(t, repo,
		domain.ConversationItem{ID: "v", Role: "user"},
		domain.ConversationItem{ID: "z", Role: "user", At: same},
		domain.ConversationItem{ID: "y", Role: "assistant", At: same},
		domain.ConversationItem{ID: "u", Role: "system", At: same},
		domain.ConversationItem{ID: "x", Role: "tool"},
		domain.ConversationItem{ID: "w", Role: "assistant", At: same.Add(-time.Hour)},
	)
	checkOrder(t, "later", fresh(t, repo), "v,z,y,u,x,w")
}

// Stored items outside a batch (another record, chat events) keep their
// positions; a batch with no stored neighbour is appended. A repeated id in a
// batch keeps its first position and its latest state.
func TestItemsOutsideTheBatchKeepTheirPositions(t *testing.T) {
	repo := orderRepo(t)
	put(t, repo, domain.ConversationItem{ID: "old1", Role: "user"}, domain.ConversationItem{ID: "old2", Role: "assistant"})
	put(t, repo, domain.ConversationItem{ID: "n1", Role: "user"}, domain.ConversationItem{ID: "n2", Role: "tool", Status: "started"},
		domain.ConversationItem{ID: "n2", Role: "tool", Status: "completed"})
	got := fresh(t, repo)
	checkOrder(t, "append", got, "old1,old2,n1,n2")
	if got[3].Status != "completed" {
		t.Fatalf("repeated id kept stale state: %#v", got[3])
	}
	put(t, repo, domain.ConversationItem{ID: "n0", Role: "user"}, domain.ConversationItem{ID: "n1", Role: "user"})
	checkOrder(t, "before neighbour", fresh(t, repo), "old1,old2,n0,n1,n2")
}

// When repeated insertions exhaust the gap, the session is renumbered in one
// transaction: every client still converges on one strictly ordered feed.
func TestExhaustedGapRenumbersConsistently(t *testing.T) {
	repo := orderRepo(t)
	first := domain.ConversationItem{ID: "first", Role: "user"}
	last := domain.ConversationItem{ID: "last", Role: "assistant"}
	put(t, repo, first, last)
	var client feedClient
	client.poll(t, repo)
	batch := []domain.ConversationItem{first}
	want := []string{"first"}
	for i := range 14 {
		id := string(rune('a' + i))
		batch = append(batch, domain.ConversationItem{ID: id, Role: "tool"})
		want = append(want, id)
		put(t, repo, append(append([]domain.ConversationItem{}, batch...), last)...)
		if i%3 == 0 {
			client.poll(t, repo)
		}
	}
	want = append(want, "last")
	client.poll(t, repo)
	checkOrder(t, "incremental", client.order(), strings.Join(want, ","))
	checkOrder(t, "fresh", fresh(t, repo), strings.Join(want, ","))
	// The newest item is always placed right after its predecessor, halving
	// the remaining gap, so 14 insertions must have renumbered at least once.
	if client.order()[0].Sequence == conversationGap && client.order()[len(want)-1].Sequence == 2*conversationGap {
		t.Fatal("expected a renumber")
	}
}

func TestNativeOrderRunKeepsTheLongestAgreeingRun(t *testing.T) {
	batch := []domain.ConversationItem{{ID: "k", Sequence: 90}, {ID: "a", Sequence: 10}, {ID: "new"}, {ID: "b", Sequence: 20}, {ID: "c", Sequence: 15}, {ID: "d", Sequence: 30}}
	fixed := nativeOrderRun(batch)
	var kept []string
	for i, ok := range fixed {
		if ok {
			kept = append(kept, batch[i].ID)
		}
	}
	if got := strings.Join(kept, ","); got != "a,b,d" && got != "a,c,d" {
		t.Fatalf("kept %s", got)
	}
}
