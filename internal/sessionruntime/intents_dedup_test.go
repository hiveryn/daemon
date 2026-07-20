package sessionruntime

import (
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func mustDedupKey(t *testing.T, sessionID string, typ domain.IntentType, payload any) string {
	t.Helper()
	k, err := intentDedupKey(sessionID, typ, payload)
	if err != nil {
		t.Fatalf("intentDedupKey: %v", err)
	}
	return k
}

// Go map iteration order is randomized, so this also guards against the key
// order of the payload map leaking into the hash.
func TestIntentDedupKeyStableAcrossKeyOrder(t *testing.T) {
	a := mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{
		"title": "Fix the thing", "repo": "daemon", "body": "details",
	})
	b := mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{
		"body": "details", "repo": "daemon", "title": "Fix the thing",
	})
	if a != b {
		t.Fatalf("key order changed the dedup key:\n a = %s\n b = %s", a, b)
	}
}

func TestIntentDedupKeyStableAcrossWhitespace(t *testing.T) {
	a := mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "Fix the thing"})
	b := mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "  Fix the thing\n"})
	if a != b {
		t.Fatalf("surrounding whitespace changed the dedup key:\n a = %s\n b = %s", a, b)
	}
}

// An omitted field and an empty one are the same request.
func TestIntentDedupKeyTreatsEmptyAsAbsent(t *testing.T) {
	a := mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "T"})
	b := mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{
		"title": "T", "repo": "", "body": "   ", "references": []any{},
	})
	if a != b {
		t.Fatalf("empty fields changed the dedup key:\n a = %s\n b = %s", a, b)
	}
}

func TestIntentDedupKeyDiffersOnMeaningfulChange(t *testing.T) {
	base := mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "A"})

	cases := map[string]string{
		"different title":   mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "B"}),
		"different session": mustDedupKey(t, "sess-2", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "A"}),
		"different tool":    mustDedupKey(t, "sess-1", domain.IntentTypeConcludeSession, map[string]any{"title": "A"}),
		"extra field":       mustDedupKey(t, "sess-1", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "A", "repo": "daemon"}),
	}
	for name, k := range cases {
		if k == base {
			t.Errorf("%s produced the same dedup key as the base payload — retries would wrongly collapse", name)
		}
	}
}

// Element order in a list is meaningful; it must not be normalized away.
func TestIntentDedupKeyRespectsSliceOrder(t *testing.T) {
	a := mustDedupKey(t, "s", domain.IntentTypeCreateWorkTicket, map[string]any{"references": []any{"x", "y"}})
	b := mustDedupKey(t, "s", domain.IntentTypeCreateWorkTicket, map[string]any{"references": []any{"y", "x"}})
	if a == b {
		t.Fatal("slice order was normalized away; [x y] and [y x] are different requests")
	}
}

// The trap this whole mechanism dies on: a per-call value in the payload makes
// every retry hash differently, silently disabling dedup.
func TestIntentDedupKeyChangesWhenPayloadCarriesPerCallValue(t *testing.T) {
	a := mustDedupKey(t, "s", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "A", "now": "2026-07-17T10:00:00Z"})
	b := mustDedupKey(t, "s", domain.IntentTypeCreateWorkTicket, map[string]any{"title": "A", "now": "2026-07-17T10:00:01Z"})
	if a == b {
		t.Fatal("expected differing keys — this test documents WHY per-call values must stay out of the payload")
	}
}
