package sessionruntime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func testIntent(id, sessionID string) domain.Intent {
	return domain.Intent{
		ID:      id,
		Type:    domain.IntentTypeCreateWorkTicket,
		Summary: "test intent",
		Origin:  domain.IntentOrigin{SessionID: sessionID, ArchitectKey: "arch"},
	}
}

func noopExec(context.Context, domain.IntentInputValues) (any, error) { return "result", nil }

func TestIntentStoreBeginCreatesThenAttaches(t *testing.T) {
	s := newIntentStore()

	id, ch, _, d := s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)
	if d != intentCreated {
		t.Fatalf("first Begin disposition = %v, want intentCreated", d)
	}
	if id != "i-1" || ch == nil {
		t.Fatalf("first Begin returned id=%q ch=%v", id, ch)
	}

	// Same dedup key while still pending → attach to the SAME intent, not a new one.
	id2, ch2, _, d2 := s.Begin("key-1", testIntent("i-2", "sess-1"), noopExec)
	if d2 != intentAttached {
		t.Fatalf("second Begin disposition = %v, want intentAttached", d2)
	}
	if id2 != "i-1" {
		t.Fatalf("attached to intent %q, want the in-flight i-1", id2)
	}
	if ch2 == nil {
		t.Fatal("attached waiter got a nil channel")
	}
	if got := len(s.pending); got != 1 {
		t.Fatalf("pending count = %d, want 1 (attach must not create a second intent)", got)
	}

	// Both waiters must receive the broadcast.
	want := intentResult{Outcome: domain.IntentOutcomeApproved, Result: "ticket-1"}
	s.Finish("i-1", want)
	for i, c := range []<-chan intentResult{ch, ch2} {
		select {
		case got := <-c:
			if got.Result != "ticket-1" {
				t.Errorf("waiter %d result = %v, want ticket-1", i, got.Result)
			}
		default:
			t.Errorf("waiter %d received nothing; broadcast must reach every waiter", i)
		}
	}
}

func TestIntentStoreReplaysResolvedOutcome(t *testing.T) {
	s := newIntentStore()

	_, _, _, _ = s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)
	s.Finish("i-1", intentResult{Outcome: domain.IntentOutcomeApproved, Result: "ticket-1"})

	// A retry after resolution must replay, not block and not mint a new intent.
	_, ch, replayed, d := s.Begin("key-1", testIntent("i-2", "sess-1"), noopExec)
	if d != intentReplayed {
		t.Fatalf("disposition = %v, want intentReplayed", d)
	}
	if ch != nil {
		t.Error("replayed Begin must not hand back a channel to wait on")
	}
	if replayed.Result != "ticket-1" {
		t.Errorf("replayed result = %v, want the original ticket-1", replayed.Result)
	}
	if got := len(s.pending); got != 0 {
		t.Errorf("pending count = %d, want 0", got)
	}
}

func TestIntentStoreReplaysDenial(t *testing.T) {
	s := newIntentStore()

	_, _, _, _ = s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)
	s.Finish("i-1", intentResult{Outcome: domain.IntentOutcomeDeniedByUser, Reason: "nope"})

	_, _, replayed, d := s.Begin("key-1", testIntent("i-2", "sess-1"), noopExec)
	if d != intentReplayed {
		t.Fatalf("disposition = %v, want intentReplayed", d)
	}
	if replayed.Outcome != domain.IntentOutcomeDeniedByUser || replayed.Reason != "nope" {
		t.Errorf("replayed = %+v, want the original denial", replayed)
	}
}

func TestIntentStoreDedupKeyIsolatesSessionsAndTools(t *testing.T) {
	s := newIntentStore()

	_, _, _, d1 := s.Begin("key-a", testIntent("i-1", "sess-1"), noopExec)
	_, _, _, d2 := s.Begin("key-b", testIntent("i-2", "sess-2"), noopExec)
	if d1 != intentCreated || d2 != intentCreated {
		t.Fatalf("dispositions = %v/%v, want both intentCreated", d1, d2)
	}
	if got := len(s.pending); got != 2 {
		t.Fatalf("pending count = %d, want 2 (different dedup keys are different intents)", got)
	}
}

func TestIntentStoreClaimIsSingleWinner(t *testing.T) {
	s := newIntentStore()
	_, _, _, _ = s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)

	var wins int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.Claim("i-1"); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("Claim winners = %d, want exactly 1 (double-claim would double-execute the write)", wins)
	}
}

func TestIntentStoreClaimDoesNotUnregister(t *testing.T) {
	s := newIntentStore()
	_, _, _, _ = s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)

	if _, ok := s.Claim("i-1"); !ok {
		t.Fatal("Claim failed")
	}

	// This is the duplicate-ticket guard: a retry landing between Claim and
	// Finish must attach, not create.
	id, ch, _, d := s.Begin("key-1", testIntent("i-2", "sess-1"), noopExec)
	if d != intentAttached {
		t.Fatalf("disposition during exec = %v, want intentAttached", d)
	}
	if id != "i-1" {
		t.Fatalf("attached to %q, want i-1", id)
	}

	// And the late attacher still receives the broadcast.
	s.Finish("i-1", intentResult{Outcome: domain.IntentOutcomeApproved, Result: "ticket-1"})
	select {
	case got := <-ch:
		if got.Result != "ticket-1" {
			t.Errorf("late attacher result = %v, want ticket-1", got.Result)
		}
	default:
		t.Error("late attacher received nothing")
	}
}

func TestIntentStoreClaimForSessionRejectsForeignSession(t *testing.T) {
	s := newIntentStore()
	_, _, _, _ = s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)

	if _, ok := s.ClaimForSession("sess-2", "i-1"); ok {
		t.Fatal("ClaimForSession allowed a different session to resolve this intent")
	}
	if _, ok := s.ClaimForSession("sess-1", "i-1"); !ok {
		t.Fatal("ClaimForSession rejected the owning session")
	}
}

func TestIntentStoreDetachLeavesIntentAlive(t *testing.T) {
	s := newIntentStore()
	id, ch, _, _ := s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)

	s.Detach(id, ch)

	// The whole point: the requester walked away (its ctx died), but the intent
	// still resolves so the retry can replay it.
	if got := len(s.pending); got != 1 {
		t.Fatalf("pending count after Detach = %d, want 1", got)
	}
	s.Finish("i-1", intentResult{Outcome: domain.IntentOutcomeAutoApproved, Result: "ticket-1"})

	_, _, replayed, d := s.Begin("key-1", testIntent("i-2", "sess-1"), noopExec)
	if d != intentReplayed || replayed.Result != "ticket-1" {
		t.Fatalf("after detach+resolve: disposition=%v result=%v, want replay of ticket-1", d, replayed.Result)
	}
}

func TestIntentStoreReplaySweepsAfterTTL(t *testing.T) {
	s := newIntentStore()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	_, _, _, _ = s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)
	s.Finish("i-1", intentResult{Outcome: domain.IntentOutcomeApproved, Result: "ticket-1"})

	// Just inside the window → still replays.
	now = now.Add(intentReplayTTL - time.Minute)
	if _, _, _, d := s.Begin("key-1", testIntent("i-2", "sess-1"), noopExec); d != intentReplayed {
		t.Fatalf("inside TTL: disposition = %v, want intentReplayed", d)
	}

	// Past the window → the cache entry is swept and a fresh intent is created.
	now = now.Add(2 * intentReplayTTL)
	if _, _, _, d := s.Begin("key-1", testIntent("i-3", "sess-1"), noopExec); d != intentCreated {
		t.Fatalf("past TTL: disposition = %v, want intentCreated", d)
	}
	if _, ok := s.replay["key-1"]; ok {
		t.Error("expired replay entry was not swept")
	}
}

func TestIntentStoreReplayWindowIsPerTool(t *testing.T) {
	s := newIntentStore()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	action := testIntent("a-1", "sess-1")
	action.Type = domain.IntentTypeExecuteAction
	_, _, _ = s.BeginDeferred("action", action, noopExec, deferredHooks{})
	s.Finish("a-1", intentResult{Outcome: domain.IntentOutcomeDeniedByUser})
	_, _, _, _ = s.Begin("ticket", testIntent("t-1", "sess-1"), noopExec)
	s.Finish("t-1", intentResult{Outcome: domain.IntentOutcomeApproved})

	// The window is anchored at resolution: exactly ten minutes later still replays.
	now = now.Add(10 * time.Minute)
	if _, _, d := s.BeginDeferred("action", action, noopExec, deferredHooks{}); d != intentReplayed {
		t.Fatalf("executeAction at 10m: disposition = %v, want intentReplayed", d)
	}

	now = now.Add(time.Second)
	again := action
	again.ID = "a-2"
	if id, _, d := s.BeginDeferred("action", again, noopExec, deferredHooks{}); d != intentCreated || id != "a-2" {
		t.Fatalf("executeAction past 10m: id=%s disposition=%v, want a fresh a-2", id, d)
	}
	// Other tools keep the default hour.
	if _, _, _, d := s.Begin("ticket", testIntent("t-2", "sess-1"), noopExec); d != intentReplayed {
		t.Fatalf("createWorkTicket at 10m1s: disposition = %v, want intentReplayed", d)
	}
	now = now.Add(intentReplayTTL)
	if _, _, _, d := s.Begin("ticket", testIntent("t-3", "sess-1"), noopExec); d != intentCreated {
		t.Fatalf("createWorkTicket past 1h: disposition = %v, want intentCreated", d)
	}
}

func TestIntentStorePendingForSession(t *testing.T) {
	s := newIntentStore()
	_, _, _, _ = s.Begin("key-1", testIntent("i-1", "sess-1"), noopExec)
	_, _, _, _ = s.Begin("key-2", testIntent("i-2", "sess-1"), noopExec)
	_, _, _, _ = s.Begin("key-3", testIntent("i-3", "sess-2"), noopExec)

	got := s.PendingForSession("sess-1")
	if len(got) != 2 || got[0] != "i-1" || got[1] != "i-2" {
		t.Fatalf("PendingForSession(sess-1) = %v, want [i-1 i-2]", got)
	}

	// Finish must clear the session index, or teardown would fire dead intents.
	s.Finish("i-1", intentResult{Outcome: domain.IntentOutcomeApproved})
	s.Finish("i-2", intentResult{Outcome: domain.IntentOutcomeApproved})
	if got := s.PendingForSession("sess-1"); len(got) != 0 {
		t.Fatalf("PendingForSession after Finish = %v, want empty", got)
	}
	if _, ok := s.bySession["sess-1"]; ok {
		t.Error("empty session bucket was not removed")
	}
}

// N concurrent identical retries must collapse to exactly one intent — the
// core idempotency guarantee, under -race.
func TestIntentStoreConcurrentIdenticalBeginsCollapseToOne(t *testing.T) {
	s := newIntentStore()

	const n = 32
	var wg sync.WaitGroup
	created := make([]int, n)
	chans := make([]<-chan intentResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ch, _, d := s.Begin("key-1", testIntent(fmt.Sprintf("i-%d", i), "sess-1"), noopExec)
			if d == intentCreated {
				created[i] = 1
			}
			chans[i] = ch
		}(i)
	}
	wg.Wait()

	total := 0
	for _, c := range created {
		total += c
	}
	if total != 1 {
		t.Fatalf("intentCreated count = %d, want exactly 1 (concurrent retries must not duplicate)", total)
	}
	if got := len(s.pending); got != 1 {
		t.Fatalf("pending count = %d, want 1", got)
	}

	// Every caller, creator and attacher alike, gets the one result.
	var id string
	for k := range s.pending {
		id = k
	}
	s.Finish(id, intentResult{Outcome: domain.IntentOutcomeApproved, Result: "ticket-1"})
	for i, c := range chans {
		select {
		case got := <-c:
			if got.Result != "ticket-1" {
				t.Fatalf("waiter %d got %v, want ticket-1", i, got.Result)
			}
		default:
			t.Fatalf("waiter %d received nothing", i)
		}
	}
}
