package engine

// Shared-dep method-receiver leak audit (C8 / task #85, 2026-05-21).
//
// Why this exists
// ---------------
// Per PR9 (ProjectId cross-session leak) and the long-tail finding that
// motivated PR #135's TestSessionIsolation_NoProjectIdLeak: process-wide
// SharedDeps singletons that expose mutating methods are a multi-tenant
// data-leak vector. One session writes via a setter, the next session
// reads the changed state — that read crosses tenant boundaries.
//
// PR #135 closed one specific case (tools.ExternalExecutor.SetProjectId).
// This audit generalizes the guard: for every SharedDeps-attached concrete
// type, reject any exported method whose name matches a mutating-verb
// prefix (Set/Update/Reset/Configure). New mutating surface must justify
// itself in code review — silently re-introducing it would break the
// guard at unit-test time.
//
// What this catches
// -----------------
// - "I added SetCallback to LLMClient so I can swap the http client mid-run"
//   — would let session A redirect session B's LLM calls.
// - "I added UpdateCorpus to KnowledgeRetriever for hot-reload"
//   — would let session A poison session B's RAG corpus.
// - "I added ResetState to GroundedRenderer to fix flaky tests"
//   — would let session A wipe session B's renderer state mid-turn.
//
// What this does NOT catch
// ------------------------
// - Method-set drift inside dependencies-of-dependencies (e.g. an LLM
//   transport's hidden state) — covered by integration tests at the
//   leak surface, not here.
// - Mutating verbs that don't match the verb prefix list (e.g. "RotateKey",
//   "ApplyOverride"). These need allow-list maintenance when introduced.
// - Setters on per-session types (e.g. *Engine.SetIntentPlanner is legitimate
//   — *Engine is constructed per session in NewSession, so its setters
//   don't leak across tenants). These types are intentionally excluded.
//
// Cross-reference
// ---------------
// - PR #135 / engine_session_test.go:340 — TestSessionIsolation_NoProjectIdLeak
//   (concrete-type guard for tools.ExternalExecutor.SetProjectId)
// - SharedDeps doc comment + struct (engine.go:181-217) — explains the
//   process-singleton model and why setters here are dangerous, then
//   declares the 13 fields the audit reasons about.
// - Memory feedback_shared_struct_setter_leaks_across_sessions
//   (originSession c2083e21) — the principle.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/embedding"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	grounded "github.com/compshare-agent/internal/renderer"
)

// mutatingVerbPrefixes is the banned-verb list. Any exported method on a
// shared-dep concrete type that begins with one of these is rejected.
// Adding to this list tightens the guard; removing requires justification.
var mutatingVerbPrefixes = []string{
	"Set",
	"Update",
	"Reset",
	"Configure",
}

// allowedSharedDepMethods is the per-type allow-list. Entries are
// fully-qualified "<pkg>.<Type>.<Method>" strings. Add an entry ONLY when
// a method's name matches a banned prefix but its semantics are provably
// safe (e.g. only writes per-call local state, never shared instance
// state). Each addition must be justified inline.
//
// Empty by default — current production has zero exceptions.
var allowedSharedDepMethods = map[string]struct{}{}

// sharedDepConcreteTypes enumerates the concrete types that back
// SharedDeps's interface fields. Listed by pointer (matching how clients
// hold them) so reflect.TypeOf returns the value receiver method set
// AND the pointer receiver method set in one shot.
//
// When SharedDeps gains a new field, add the corresponding concrete type
// here. The list is a deliberate single-source-of-truth — automatic
// discovery (via reflect on SharedDeps fields) would only see the
// interface method set, missing concrete-only setters that an unsafe
// type-assertion could reach.
var sharedDepConcreteTypes = []reflect.Type{
	reflect.TypeOf((*llm.Client)(nil)),                 // SharedDeps.LLMClient
	reflect.TypeOf((*knowledge.Retriever)(nil)),        // SharedDeps.KnowledgeRetriever
	reflect.TypeOf((*grounded.GroundedRenderer)(nil)),  // SharedDeps.GroundedRenderer
	reflect.TypeOf((*governance.MemoryLimiter)(nil)),   // SharedDeps.RateLimiter
	reflect.TypeOf((*knowledge.EmbeddingSidecar)(nil)), // injected into knowledge.Retriever
	reflect.TypeOf((*embedding.Client)(nil)),           // upstream of knowledge.EmbeddingSidecar
}

// TestSharedDeps_NoMutatingSetterLeakage scans every method of every
// shared-dep concrete type and fails if any exported name starts with a
// banned verb prefix. See the file-level comment for the leak model.
func TestSharedDeps_NoMutatingSetterLeakage(t *testing.T) {
	for _, typ := range sharedDepConcreteTypes {
		typeName := typ.Elem().PkgPath() + "." + typ.Elem().Name()
		for i := 0; i < typ.NumMethod(); i++ {
			m := typ.Method(i)
			// Method.Name is the basename (no receiver / package).
			methodKey := typeName + "." + m.Name
			if _, ok := allowedSharedDepMethods[methodKey]; ok {
				continue
			}
			for _, prefix := range mutatingVerbPrefixes {
				if strings.HasPrefix(m.Name, prefix) {
					t.Errorf(
						"shared-dep leak: %s has exported mutating method %q "+
							"(prefix %q). Process-wide SharedDeps singletons must not "+
							"expose setters — see internal/engine/engine.go:181-217 + "+
							"PR #135 / engine_session_test.go:340 for the leak model. "+
							"If the method is genuinely safe (per-call only, never "+
							"shared instance state), add %q to allowedSharedDepMethods "+
							"with an inline justification. Otherwise refactor the value "+
							"to flow through cfg → constructor (PR9 pattern) or via a "+
							"per-session field on Engine, never a SharedDeps setter.",
						typeName, m.Name, prefix, methodKey,
					)
				}
			}
		}
	}
}

// TestSharedDeps_AuditCoversAllSharedDepFields ensures the audit list
// stays in sync with SharedDeps. When a new field is added to SharedDeps,
// either:
//   - add its concrete type to sharedDepConcreteTypes, OR
//   - add the field name to nonAuditableFields with a reason (data-only,
//     primitive, non-leak-bearing map, etc.)
//
// Without this counter-test the audit could silently drift when SharedDeps
// grows — a new mutable type could slip in unaudited.
var nonAuditableFields = map[string]string{
	"IntentPlannerModel":          "string — no methods",
	"IntentPlannerEnabledIntents": "map[intent.Intent]struct{} — set-shaped data, no methods of concern",
	"IntentCutoverIntents":        "map[intent.Intent]struct{} — set-shaped data, no methods of concern",
	"GroundedRendererModel":       "string — no methods",
	"FastTemplateRenderer":        "bool — no methods",
	"SupportsObjectToolChoice":    "bool — no methods",
	"MaxTokensPerTurn":            "int — no methods",
	"ExternalExecutor":            "tools.ToolExecutor — already covered by TestSessionIsolation_NoProjectIdLeak (PR #135)",
	"IntentPlanner":               "intent.IntentPlanner interface — concrete intent.Planner verified clean (single exported method Plan, see TODO below for promotion criteria)",
}

// TODO(future): promote intent.Planner into sharedDepConcreteTypes IF
// either (a) it gains a second exported method whose semantics aren't
// trivially obvious, or (b) production wiring starts handing out the
// concrete *Planner pointer rather than the IntentPlanner interface.
// Verified at this PR: intent.Planner only exports Plan(ctx,input)
// (internal/intent/planner.go:101) — non-mutating, no setter. Behind
// the IntentPlanner interface (engine.go:111), a setter would also
// have to widen the interface or require an unsafe type assertion to
// reach, both of which would be caught by code review. Not an
// acceptance gap for this PR.

func TestSharedDeps_AuditCoversAllSharedDepFields(t *testing.T) {
	sharedDepsType := reflect.TypeOf(SharedDeps{})
	// Build the audited-type set keyed by "<pkg>.<TypeName>" for matching.
	audited := map[string]struct{}{}
	for _, typ := range sharedDepConcreteTypes {
		elem := typ.Elem()
		audited[elem.PkgPath()+"."+elem.Name()] = struct{}{}
	}
	for i := 0; i < sharedDepsType.NumField(); i++ {
		field := sharedDepsType.Field(i)
		if _, ok := nonAuditableFields[field.Name]; ok {
			continue
		}
		// For interface fields, the audit lookup is keyed by the concrete
		// impl's type name. We can't derive that automatically — the
		// sharedDepConcreteTypes list is the source of truth. The check
		// here is: every SharedDeps field must EITHER appear in
		// nonAuditableFields OR have a concrete impl audited via
		// sharedDepConcreteTypes.
		//
		// The match heuristic is "the audited list contains AT LEAST ONE
		// concrete type intended to back this field". We document the
		// mapping in sharedDepConcreteTypes comments rather than enforce
		// it programmatically — interface↔concrete mapping is not
		// statically derivable in Go reflection.
		switch field.Name {
		case "LLMClient":
			requireAudited(t, audited, "github.com/compshare-agent/internal/llm.Client", field.Name)
		case "AgentLLMClient":
			// Same concrete type as LLMClient (router.For(TierAgent) → *llm.Client),
			// already audited via sharedDepConcreteTypes — the mutating-setter scan
			// covers it once. Listed explicitly so the field stays classified.
			requireAudited(t, audited, "github.com/compshare-agent/internal/llm.Client", field.Name)
		case "KnowledgeRetriever":
			requireAudited(t, audited, "github.com/compshare-agent/internal/knowledge.Retriever", field.Name)
		case "GroundedRenderer":
			requireAudited(t, audited, "github.com/compshare-agent/internal/renderer.GroundedRenderer", field.Name)
		case "RateLimiter":
			requireAudited(t, audited, "github.com/compshare-agent/internal/governance.MemoryLimiter", field.Name)
		default:
			t.Errorf(
				"SharedDeps field %q is neither in nonAuditableFields nor "+
					"in the explicit interface→concrete map in this test. "+
					"Add it to nonAuditableFields with a reason, or extend "+
					"the switch with its concrete impl in sharedDepConcreteTypes.",
				field.Name,
			)
		}
	}
}

func requireAudited(t *testing.T, audited map[string]struct{}, key, fieldName string) {
	t.Helper()
	if _, ok := audited[key]; !ok {
		t.Errorf(
			"SharedDeps field %q backed by concrete type %q is NOT in "+
				"sharedDepConcreteTypes — the mutating-verb audit will not "+
				"catch a setter added to that type. Add it to the list.",
			fieldName, key,
		)
	}
}

// fixtureLeakyDep mirrors the leak pattern PR9 caught (SetX on a
// process-singleton type). It exists ONLY for the negative-fixture
// test below — it proves the audit's matcher actually fires on a real
// setter, so future refactors can't make the audit trivially-passing
// by changing the matching logic.
type fixtureLeakyDep struct{}

func (*fixtureLeakyDep) SetProjectId(string) {}
func (*fixtureLeakyDep) UpdateState(int)     {}
func (*fixtureLeakyDep) ResetCache()         {}
func (*fixtureLeakyDep) ConfigureLimits()    {}

// safeMethod has a non-banned verb prefix. The negative-fixture test
// asserts the matcher does NOT fire on this — guarding against
// over-broad matching that would force false-positive allow-list
// entries for legitimate APIs.
func (*fixtureLeakyDep) Allow(int) bool  { return true }
func (*fixtureLeakyDep) Chat(int) string { return "" }

// TestSharedDeps_AuditMatcherActuallyFiresOnSetters is the negative
// fixture: run the same matcher used by TestSharedDeps_NoMutatingSetterLeakage
// against a hand-crafted type with known setters and confirm each is flagged.
// This is the difference between "the audit passes because production is
// clean" and "the audit passes because it doesn't actually check anything".
func TestSharedDeps_AuditMatcherActuallyFiresOnSetters(t *testing.T) {
	typ := reflect.TypeOf((*fixtureLeakyDep)(nil))
	expectedFlags := map[string]string{
		"SetProjectId":    "Set",
		"UpdateState":     "Update",
		"ResetCache":      "Reset",
		"ConfigureLimits": "Configure",
	}
	expectedSafe := []string{"Allow", "Chat"}

	flagged := map[string]string{} // method name → matching prefix
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		for _, prefix := range mutatingVerbPrefixes {
			if strings.HasPrefix(m.Name, prefix) {
				flagged[m.Name] = prefix
				break
			}
		}
	}
	for name, wantPrefix := range expectedFlags {
		gotPrefix, ok := flagged[name]
		if !ok {
			t.Errorf("matcher failed to flag %q on fixtureLeakyDep — "+
				"the production audit would also miss this. Check "+
				"mutatingVerbPrefixes is wired correctly.", name)
			continue
		}
		if gotPrefix != wantPrefix {
			t.Errorf("matcher flagged %q with prefix %q, want %q", name, gotPrefix, wantPrefix)
		}
	}
	for _, name := range expectedSafe {
		if prefix, ok := flagged[name]; ok {
			t.Errorf("matcher false-positive on safe method %q "+
				"(prefix %q). Over-broad matching forces false-positive "+
				"allow-list entries. Tighten the prefix list.", name, prefix)
		}
	}
}
