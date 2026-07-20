package sessionruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
)

// intentDedupKey identifies "the same tool call again": same session, same
// tool, same normalized args. It is the defense against an agent-runtime
// tool-call timeout causing the agent to retry and mint a duplicate ticket.
//
// The payload must contain only request-derived values. Anything varying per
// call (a timestamp, a generated id) makes every retry hash differently and
// silently disables dedup entirely.
func intentDedupKey(sessionID string, t domain.IntentType, payload any) (string, error) {
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s payload for dedup: %w", t, err)
	}
	// NUL separators so ("a", "b|c") and ("a|b", "c") cannot collide.
	sum := sha256.Sum256([]byte(sessionID + "\x00" + string(t) + "\x00" + canonical))
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON renders a payload to a stable string: strings trimmed, empty
// values dropped, object keys ordered.
//
// Key ordering comes free — encoding/json sorts map[string]any keys on marshal
// — so the round-trip through `any` IS the stable ordering.
func canonicalJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	normalized, err := json.Marshal(normalizeForDedup(decoded))
	if err != nil {
		return "", fmt.Errorf("marshal normalized: %w", err)
	}
	return string(normalized), nil
}

// normalizeForDedup trims strings and drops empties recursively, so that
// {"repo": ""} and {} — or a title with stray trailing whitespace — hash the
// same. Slice element order is significant and is preserved.
func normalizeForDedup(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, raw := range t {
			n := normalizeForDedup(raw)
			if isEmptyForDedup(n) {
				continue
			}
			out[k] = n
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, raw := range t {
			n := normalizeForDedup(raw)
			if isEmptyForDedup(n) {
				continue
			}
			out = append(out, n)
		}
		return out
	case string:
		return strings.TrimSpace(t)
	default:
		return v
	}
}

func isEmptyForDedup(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	default:
		return false
	}
}
