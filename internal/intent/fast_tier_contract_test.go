package intent

import (
	"testing"

	"github.com/compshare-agent/internal/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRoutingRoutes_AreDeterministicallyDispatched locks the routing half of
// the deterministic read-only contract: every generated route binds a real Go
// handler and never enters ReAct/LLM tool choice.
func TestRoutingRoutes_AreDeterministicallyDispatched(t *testing.T) {
	count := 0
	for _, route := range routing.GeneratedRoutes() {
		count++
		assert.NotEmptyf(t, route.HandlerKey,
			"route %q must bind a handler_key", route.Name)
		assert.Truef(t, CapabilityHandlerForKey(route.HandlerKey) != nil,
			"route %q handler_key %q resolves to no handler", route.Name, route.HandlerKey)
		assert.NotEmptyf(t, route.IntentLabel, "route %q must declare an intent_label", route.Name)
		assert.Truef(t, IsCapabilityIntent(Intent(route.IntentLabel)),
			"route %q intent %q must be deterministically dispatched", route.Name, route.IntentLabel)
	}
	require.GreaterOrEqualf(t, count, 7,
		"expected the catalog/status routes to exist (got %d) - non-vacuity guard", count)
}
