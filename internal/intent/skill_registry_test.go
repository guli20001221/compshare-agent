package intent

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/compshare-agent/internal/routing"
)

// TestSystemPrompt_MatchesBaselineSHA is the byte-identity guard now that the
// generated route registry is the sole deterministic route source: the FULL planner system
// prompt must hash to systemPromptSHA256Baseline. Any drift in a route's
// directive, example question, or confidence (or the fragment order) changes this
// SHA and fails here. (Replaces the deleted flag-gated SHA test from P3a-3.)
func TestRouteSource_SkillRegistryRoutesIdenticalDispatch(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	for i := range routingIntentSet() {
		if !IsRoutingIntent(i) {
			t.Errorf("IsRoutingIntent(%q) = false, want true", i)
		}
		req := HandlerRequest{Plan: IntentRoute{Intent: i}}
		result := h.DispatchRoute(context.Background(), req)
		if result.Status != HandlerStatusHandled {
			t.Errorf("DispatchRoute(%q) status = %q, want %q", i, result.Status, HandlerStatusHandled)
		}
		if want := skillRequiredTool(i); result.ToolAction != want {
			t.Errorf("DispatchRoute(%q) ToolAction = %q, want %q", i, result.ToolAction, want)
		}
	}
}

// TestRouteHandlerForKey_ResolvesEveryRouteSkill asserts every migrated
// route skill declares a handler_key that resolves to a non-nil handler, and
// that the count of route skills equals routingIntentOrder.
func TestRouteHandlerForKey_ResolvesEveryRouteSkill(t *testing.T) {
	count := 0
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == "" {
			continue
		}
		count++
		if route.HandlerKey == "" {
			t.Errorf("route %q declares no handler_key", route.Name)
			continue
		}
		if RouteHandlerForKey(route.HandlerKey) == nil {
			t.Errorf("route %q handler_key %q does not resolve", route.Name, route.HandlerKey)
		}
	}
	if count != len(routingIntentOrder) {
		t.Errorf("route skills (intent_label set) = %d, want %d (routingIntentOrder size)", count, len(routingIntentOrder))
	}
}

// TestRouteHandlerByKey_MatchesExpectedHandlers asserts the handler bound to
// each route skill's handler_key is the expected per-intent Go handler func
// (compared by func pointer). This pins the skill↔Go dispatch binding.
func TestRouteHandlerByKey_MatchesRegistry(t *testing.T) {
	expectedByIntent := map[Intent]routeHandlerFunc{
		IntentGPUSpecsQuery:         handleGPUSpecsQuery,
		IntentStockAvailability:     handleStockAvailability,
		IntentNetAcceleratorStatus:  handleNetAcceleratorStatus,
		IntentRefundEstimate:        handleRefundEstimate,
		IntentCFSInfo:               handleCFSInfo,
		IntentImageTagCatalog:       handleImageTagCatalog,
		IntentModelRepositoryBrowse: handleModelRepositoryBrowse,
		IntentImageList:             handleImageList,
		IntentPricingQuery:          handlePricingQuery,
	}
	keyByIntent := map[Intent]string{}
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel != "" {
			keyByIntent[Intent(route.IntentLabel)] = route.HandlerKey
		}
	}
	for _, i := range routingIntentOrder {
		key, ok := keyByIntent[i]
		if !ok {
			t.Errorf("intent %q has no route skill", i)
			continue
		}
		got := RouteHandlerForKey(key)
		if got == nil {
			t.Errorf("intent %q handler_key %q does not resolve", i, key)
			continue
		}
		want := expectedByIntent[i]
		if want == nil {
			t.Errorf("intent %q has no expected handler in the test table", i)
			continue
		}
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("intent %q: skill handler_key %q binds a different func than expected", i, key)
		}
	}
}

// TestRouteHandlerByKey_NoStaleEntries asserts the bridge map carries no key
// beyond those declared by the route skills (no dangling binding).
func TestRouteHandlerByKey_NoStaleEntries(t *testing.T) {
	declared := map[string]bool{}
	for _, route := range routing.GeneratedRoutes() {
		if route.HandlerKey != "" {
			declared[route.HandlerKey] = true
		}
	}
	for key := range routeHandlerByKey {
		if !declared[key] {
			t.Errorf("routeHandlerByKey has stale key %q not declared by any skill", key)
		}
	}
	if len(routeHandlerByKey) != len(declared) {
		t.Errorf("routeHandlerByKey size %d != declared handler_keys %d", len(routeHandlerByKey), len(declared))
	}
}

// TestRouteHandlerByKey_MatchesKnownHandlerKeys is the cross-package parity
// guard codegen.go documents: the intent-side handler binding (routeHandlerByKey)
// must cover EXACTLY the skills-side codegen allow-list (skills.KnownHandlerKeys()).
// The two sets are hand-maintained in different packages — without this assertion a
// key added to one but not the other drifts silently (codegen would accept a
// handler_key the bridge can't bind, or the bridge would carry a key codegen rejects).
func TestRouteHandlerByKey_MatchesKnownHandlerKeys(t *testing.T) {
	bindingKeys := map[string]bool{}
	for key := range routeHandlerByKey {
		bindingKeys[key] = true
	}
	allowList := routing.KnownHandlerKeys()
	if len(allowList) != len(bindingKeys) {
		t.Fatalf("set size mismatch: routing.KnownHandlerKeys()=%d, routeHandlerByKey=%d (%v vs %v)",
			len(allowList), len(bindingKeys), allowList, keysOf(bindingKeys))
	}
	for _, key := range allowList {
		if !bindingKeys[key] {
			t.Errorf("handler_key %q is in skills.KnownHandlerKeys() but not bound in routeHandlerByKey", key)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRouteSkills_ReactToolSubsetMatchesIntentToolSubset pins each migrated
// route skill's react_tool_subset to the live IntentToolSubset() value for
// its intent. They are equal today by hand; this guard keeps them equal so that
// when USE_SKILL_REGISTRY (P2 part 2) sources the ReAct tool window from the skill
// registry, the planner-visible tool set stays byte-identical to the legacy
// tool_subset.go source. Without it the two could silently diverge after the flip.
func TestRouteSkills_ReactToolSubsetMatchesIntentToolSubset(t *testing.T) {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == "" {
			continue
		}
		want := IntentToolSubset(Intent(route.IntentLabel))
		if !reflect.DeepEqual(route.ToolSubset, want) {
			t.Errorf("%s: tool_subset=%v but IntentToolSubset(%s)=%v", route.Name, route.ToolSubset, route.IntentLabel, want)
		}
	}
}

// TestSkillRegistryRouteMetadata_Shape asserts the skill-sourced metadata is
// ordered by routingIntentOrder, projects each route skill's required tool
// (RequiredTools[0]) into RequiredTool, and never sets required_citation
// (routes are NOT cited).
func TestSkillRegistryRouteMetadata_Shape(t *testing.T) {
	skillMeta := skillRegistryRouteMetadata()
	if len(skillMeta) != len(routingIntentOrder) {
		t.Fatalf("skill metadata count = %d, want %d", len(skillMeta), len(routingIntentOrder))
	}
	for i, want := range routingIntentOrder {
		got := skillMeta[i]
		if got.IntentLabel != string(want) {
			t.Errorf("[%d] intent order drift: skill=%q want=%q", i, got.IntentLabel, want)
		}
		if wantTool := skillRequiredTool(want); got.RequiredTool != wantTool {
			t.Errorf("[%d] %s: required_tool skill=%q want=%q", i, got.Name, got.RequiredTool, wantTool)
		}
		if got.RequiredCitation {
			t.Errorf("[%d] %s: required_citation must be false for routes", i, got.Name)
		}
	}
}
