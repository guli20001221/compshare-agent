package intent

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/skills"
)

// TestFastTierSkills_AreCapabilityDispatched locks the ROUTING half of the
// fast-tier determinism contract (P3a-1): a skill declaring applicable_tiers:[fast]
// is deterministically capability-dispatched — it binds a real handler_key and its
// intent is a capability intent (routed to DispatchCapability), so it NEVER enters
// the ReAct loop where an LLM picks tools. The render half (the handler's envelope
// bypasses the LLM renderer) is locked in the engine package.
func TestFastTierSkills_AreCapabilityDispatched(t *testing.T) {
	fast := 0
	for _, s := range skills.GeneratedSkills() {
		if !slices.Contains(s.ApplicableTiers, skills.TierFast) {
			continue
		}
		fast++
		assert.NotEmptyf(t, s.HandlerKey,
			"fast-tier skill %q must bind a capability handler_key — fast never enters ReAct/LLM tool-choice", s.Name)
		assert.Truef(t, CapabilityHandlerForKey(s.HandlerKey) != nil,
			"fast-tier skill %q handler_key %q resolves to no handler", s.Name, s.HandlerKey)
		assert.NotEmptyf(t, s.IntentLabel, "fast-tier skill %q must declare an intent_label", s.Name)
		assert.Truef(t, IsCapabilityIntent(Intent(s.IntentLabel)),
			"fast-tier skill %q intent %q must be a capability intent (routes to DispatchCapability, never ReAct)", s.Name, s.IntentLabel)
	}
	require.GreaterOrEqualf(t, fast, 6,
		"expected the 6 catalog capability skills to be fast-tier (got %d) — non-vacuity guard", fast)
}
