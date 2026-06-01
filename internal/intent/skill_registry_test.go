package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"testing"

	"github.com/compshare-agent/internal/skills"
)

// TestSystemPrompt_MatchesBaselineSHA is the byte-identity guard now that the
// generated skill registry is the sole capability source: the FULL planner system
// prompt must hash to systemPromptSHA256Baseline. Any drift in a capability skill's
// directive, example question, or confidence (or the fragment order) changes this
// SHA and fails here. (Replaces the deleted flag-gated SHA test from P3a-3.)
func TestSystemPrompt_MatchesBaselineSHA(t *testing.T) {
	sum := sha256.Sum256([]byte(buildSystemPrompt()))
	got := hex.EncodeToString(sum[:])
	if got != systemPromptSHA256Baseline {
		t.Errorf("system prompt drifted from baseline.\n"+
			"  baseline: %s\n"+
			"  got:      %s\n"+
			"The skill registry is the sole capability source; a directive/example/order"+
			" change broke the pinned SHA.",
			systemPromptSHA256Baseline, got)
	}
}

// TestCapabilitySource_SkillRegistryRoutesIdenticalDispatch asserts every
// capability intent dispatches to a handler that returns Handled with
// ToolAction == requiredTool (func-pointer identity is separately pinned by
// TestCapabilityHandlerByKey_MatchesRegistry).
func TestCapabilitySource_SkillRegistryRoutesIdenticalDispatch(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	for i := range capabilityIntentSet() {
		if !IsCapabilityIntent(i) {
			t.Errorf("IsCapabilityIntent(%q) = false, want true", i)
		}
		req := HandlerRequest{Plan: Plan{Intent: i}}
		result := h.DispatchCapability(context.Background(), req)
		if result.Status != HandlerStatusHandled {
			t.Errorf("DispatchCapability(%q) status = %q, want %q", i, result.Status, HandlerStatusHandled)
		}
		if want := skillRequiredTool(i); result.ToolAction != want {
			t.Errorf("DispatchCapability(%q) ToolAction = %q, want %q", i, result.ToolAction, want)
		}
	}
}

// TestCapabilityHandlerForKey_ResolvesEveryCapabilitySkill asserts every migrated
// capability skill declares a handler_key that resolves to a non-nil handler, and
// that the count of capability skills equals capabilityIntentOrder.
func TestCapabilityHandlerForKey_ResolvesEveryCapabilitySkill(t *testing.T) {
	count := 0
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel == "" {
			continue
		}
		count++
		if s.HandlerKey == "" {
			t.Errorf("capability skill %q declares no handler_key", s.Name)
			continue
		}
		if CapabilityHandlerForKey(s.HandlerKey) == nil {
			t.Errorf("skill %q handler_key %q does not resolve", s.Name, s.HandlerKey)
		}
	}
	if count != len(capabilityIntentOrder) {
		t.Errorf("capability skills (intent_label set) = %d, want %d (capabilityIntentOrder size)", count, len(capabilityIntentOrder))
	}
}

// TestCapabilityHandlerByKey_MatchesExpectedHandlers asserts the handler bound to
// each capability skill's handler_key is the expected per-intent Go handler func
// (compared by func pointer). This pins the skill↔Go dispatch binding.
func TestCapabilityHandlerByKey_MatchesRegistry(t *testing.T) {
	expectedByIntent := map[Intent]capabilityHandlerFunc{
		IntentGPUSpecsQuery:      handleGPUSpecsQuery,
		IntentStockAvailability:  handleStockAvailability,
		IntentPlatformImageList:  handlePlatformImageList,
		IntentCustomImageList:    handleCustomImageList,
		IntentCommunityImageList: handleCommunityImageList,
		IntentPricingQuery:       handlePricingQuery,
	}
	keyByIntent := map[Intent]string{}
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel != "" {
			keyByIntent[Intent(s.IntentLabel)] = s.HandlerKey
		}
	}
	for _, i := range capabilityIntentOrder {
		key, ok := keyByIntent[i]
		if !ok {
			t.Errorf("intent %q has no capability skill", i)
			continue
		}
		got := CapabilityHandlerForKey(key)
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

// TestCapabilityHandlerByKey_NoStaleEntries asserts the bridge map carries no key
// beyond those declared by the capability skills (no dangling binding).
func TestCapabilityHandlerByKey_NoStaleEntries(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range skills.GeneratedSkills() {
		if s.HandlerKey != "" {
			declared[s.HandlerKey] = true
		}
	}
	for key := range capabilityHandlerByKey {
		if !declared[key] {
			t.Errorf("capabilityHandlerByKey has stale key %q not declared by any skill", key)
		}
	}
	if len(capabilityHandlerByKey) != len(declared) {
		t.Errorf("capabilityHandlerByKey size %d != declared handler_keys %d", len(capabilityHandlerByKey), len(declared))
	}
}

// TestCapabilityHandlerByKey_MatchesKnownHandlerKeys is the cross-package parity
// guard codegen.go documents: the intent-side handler binding (capabilityHandlerByKey)
// must cover EXACTLY the skills-side codegen allow-list (skills.KnownHandlerKeys()).
// The two sets are hand-maintained in different packages — without this assertion a
// key added to one but not the other drifts silently (codegen would accept a
// handler_key the bridge can't bind, or the bridge would carry a key codegen rejects).
func TestCapabilityHandlerByKey_MatchesKnownHandlerKeys(t *testing.T) {
	bindingKeys := map[string]bool{}
	for key := range capabilityHandlerByKey {
		bindingKeys[key] = true
	}
	allowList := skills.KnownHandlerKeys()
	if len(allowList) != len(bindingKeys) {
		t.Fatalf("set size mismatch: skills.KnownHandlerKeys()=%d, capabilityHandlerByKey=%d (%v vs %v)",
			len(allowList), len(bindingKeys), allowList, keysOf(bindingKeys))
	}
	for _, key := range allowList {
		if !bindingKeys[key] {
			t.Errorf("handler_key %q is in skills.KnownHandlerKeys() but not bound in capabilityHandlerByKey", key)
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

// TestCapabilitySkills_ReactToolSubsetMatchesIntentToolSubset pins each migrated
// capability skill's react_tool_subset to the live IntentToolSubset() value for
// its intent. They are equal today by hand; this guard keeps them equal so that
// when USE_SKILL_REGISTRY (P2 part 2) sources the ReAct tool window from the skill
// registry, the planner-visible tool set stays byte-identical to the legacy
// tool_subset.go source. Without it the two could silently diverge after the flip.
func TestCapabilitySkills_ReactToolSubsetMatchesIntentToolSubset(t *testing.T) {
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel == "" {
			continue
		}
		want := IntentToolSubset(Intent(s.IntentLabel))
		if !reflect.DeepEqual(s.ReactToolSubset, want) {
			t.Errorf("%s: react_tool_subset=%v but IntentToolSubset(%s)=%v", s.Name, s.ReactToolSubset, s.IntentLabel, want)
		}
	}
}

// TestSkillRegistryCapabilityMetadata_Shape asserts the skill-sourced metadata is
// ordered by capabilityIntentOrder, projects each capability skill's required tool
// (RequiredTools[0]) into RequiredTool, and never sets required_citation
// (capabilities are NOT cited).
func TestSkillRegistryCapabilityMetadata_Shape(t *testing.T) {
	skillMeta := skillRegistryCapabilityMetadata()
	if len(skillMeta) != len(capabilityIntentOrder) {
		t.Fatalf("skill metadata count = %d, want %d", len(skillMeta), len(capabilityIntentOrder))
	}
	for i, want := range capabilityIntentOrder {
		got := skillMeta[i]
		if got.IntentLabel != string(want) {
			t.Errorf("[%d] intent order drift: skill=%q want=%q", i, got.IntentLabel, want)
		}
		if wantTool := skillRequiredTool(want); got.RequiredTool != wantTool {
			t.Errorf("[%d] %s: required_tool skill=%q want=%q", i, got.Name, got.RequiredTool, wantTool)
		}
		if got.RequiredCitation {
			t.Errorf("[%d] %s: required_citation must be false for capabilities", i, got.Name)
		}
	}
}
