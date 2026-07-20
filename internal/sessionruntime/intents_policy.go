package sessionruntime

import (
	"fmt"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// intentPolicies is the per-tool timeout policy. Hardcoded on purpose: there is
// no config layer for policy, so adding a tool or flipping its behavior is a
// one-line change here.
//
// The wait *window* is not in this table — it comes from config
// (intent_wait_timeout, default 20s) and is shared by every tool.
var intentPolicies = map[domain.IntentType]domain.IntentPolicy{
	// Preserves today's exact behavior: a conclusion nobody answers is applied.
	domain.IntentTypeConcludeSession:  domain.IntentPolicyWaitThenAllow,
	domain.IntentTypeCreateWorkTicket: domain.IntentPolicyWaitThenAllow,
}

// policyFor fails fast on an unregistered tool. A typo must never silently
// become auto-allow — that would hand an agent an unapproved write.
func policyFor(t domain.IntentType) (domain.IntentPolicy, error) {
	p, ok := intentPolicies[t]
	if !ok {
		return "", fmt.Errorf("no intent policy registered for tool %q (register it in intentPolicies)", t)
	}
	return p, nil
}

// intentWaitWindow is how long an intent waits for the user before its policy
// fires. It must stay safely under the smallest agent-runtime tool-call ceiling
// (~60s across Claude/codex/opencode); if a confirmed ceiling ever approaches
// this, shrink this window rather than raising the ceiling.
func (s *Service) intentWaitWindow() time.Duration {
	return time.Duration(s.cfg.IntentWaitTimeout) * time.Second
}
