package intent

import (
	"reflect"
	"sort"
	"testing"

	"github.com/compshare-agent/internal/skills"
)

// TestSkillRegistryCapabilityFragments_ByteIdenticalToLegacy is the B2b P2
// byte-identity gate. The planner-prompt directives + examples built from the
// generated skill registry MUST be byte-for-byte identical to the legacy
// capabilityMetadata source. A failure means a migrated skill.md drifted from its
// capabilities/*.md origin (a directive string, an example question, or a
// confidence value) — which would change the planner system prompt SHA the
// moment USE_SKILL_REGISTRY is flipped on.
func TestSkillRegistryCapabilityFragments_ByteIdenticalToLegacy(t *testing.T) {
	legacyDir, legacyEx := capabilityPromptFragmentsFrom(capabilityMetadata)
	skillDir, skillEx := capabilityPromptFragmentsFrom(skillRegistryCapabilityMetadata())

	assertStringSlicesEqual(t, "directives", legacyDir, skillDir)
	assertStringSlicesEqual(t, "examples", legacyEx, skillEx)
}

func assertStringSlicesEqual(t *testing.T, label string, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: length differs: legacy=%d skill=%d", label, len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("%s[%d] differs:\n legacy: %q\n skill:  %q", label, i, want[i], got[i])
		}
	}
}

// TestCapabilityHandlerForKey_ResolvesEveryCapabilitySkill asserts every migrated
// capability skill declares a handler_key that resolves to a non-nil handler, and
// that the count of capability skills equals the capabilityRegistry size.
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
	if count != len(capabilityRegistry) {
		t.Errorf("capability skills (intent_label set) = %d, want %d (capabilityRegistry size)", count, len(capabilityRegistry))
	}
}

// TestCapabilityHandlerByKey_MatchesRegistry asserts the handler bound to each
// capability skill's handler_key is the SAME func capabilityRegistry dispatches
// for that intent (compared by func pointer). This pins the skill↔Go dispatch
// binding so the flag-on path routes identically to legacy.
func TestCapabilityHandlerByKey_MatchesRegistry(t *testing.T) {
	keyByIntent := map[Intent]string{}
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel != "" {
			keyByIntent[Intent(s.IntentLabel)] = s.HandlerKey
		}
	}
	for _, e := range capabilityRegistry {
		key, ok := keyByIntent[e.intent]
		if !ok {
			t.Errorf("intent %q has no capability skill", e.intent)
			continue
		}
		got := CapabilityHandlerForKey(key)
		if got == nil {
			t.Errorf("intent %q handler_key %q does not resolve", e.intent, key)
			continue
		}
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(e.handler).Pointer() {
			t.Errorf("intent %q: skill handler_key %q binds a different func than capabilityRegistry", e.intent, key)
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

// TestSkillRegistryCapabilityMetadata_MatchesLegacyShape asserts the skill-sourced
// metadata reproduces the legacy capabilityMetadata field-for-field (same order,
// names, required tool, citation flag) — the structural counterpart to the
// byte-identity prompt test.
func TestSkillRegistryCapabilityMetadata_MatchesLegacyShape(t *testing.T) {
	skillMeta := skillRegistryCapabilityMetadata()
	if len(skillMeta) != len(capabilityMetadata) {
		t.Fatalf("skill metadata count = %d, want %d", len(skillMeta), len(capabilityMetadata))
	}
	for i, legacy := range capabilityMetadata {
		got := skillMeta[i]
		if got.Name != legacy.Name || got.IntentLabel != legacy.IntentLabel {
			t.Errorf("[%d] name/intent drift: skill=%q/%q legacy=%q/%q", i, got.Name, got.IntentLabel, legacy.Name, legacy.IntentLabel)
		}
		if got.RequiredTool != legacy.RequiredTool {
			t.Errorf("[%d] %s: required_tool skill=%q legacy=%q", i, legacy.Name, got.RequiredTool, legacy.RequiredTool)
		}
		if got.RequiredCitation != legacy.RequiredCitation {
			t.Errorf("[%d] %s: required_citation skill=%v legacy=%v", i, legacy.Name, got.RequiredCitation, legacy.RequiredCitation)
		}
	}
}
