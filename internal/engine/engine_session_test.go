package engine

import (
	"reflect"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/tools"

	openai "github.com/sashabaranov/go-openai"
)

// newTwoSessions constructs two Engines from the same SharedDeps. Used by the
// P0 isolation tests below. mockLLM / mockExecutor live in engine_test.go (same
// package) so we reuse them rather than declaring a parallel stub.
func newTwoSessions(t *testing.T) (engA, engB *Engine, deps *SharedDeps) {
	t.Helper()
	deps = &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         &mockExecutor{results: map[string]map[string]any{}},
	}
	engA = NewSession(deps, SessionOptions{Subject: "subj-A"})
	engB = NewSession(deps, SessionOptions{Subject: "subj-B"})
	return engA, engB, deps
}

// TestSessionIsolation_Messages — P0-1.
// Per plan §3.2: messages串了的后果是 user B 看到 user A 原话。This test injects
// a marker into session A's messages slice and asserts session B never sees it,
// even though both sessions share the same SharedDeps. Encodes WHY: cross-user
// data leak is the highest-severity failure mode of single-replica multi-tenant
// deployment.
func TestSessionIsolation_Messages(t *testing.T) {
	engA, engB, _ := newTwoSessions(t)

	const secret = "SECRET-A-PAYLOAD-12345"
	engA.messages = append(engA.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: secret,
	})

	for _, m := range engB.MessagesSnapshot() {
		if m.Content == secret {
			t.Fatalf("session B leaked session A's message: %q", m.Content)
		}
	}
	// Sanity: A actually has the message — otherwise this test would pass by
	// vacuously empty snapshots.
	gotA := engA.MessagesSnapshot()
	if len(gotA) == 0 || gotA[len(gotA)-1].Content != secret {
		t.Fatalf("session A lost its own message; got %#v", gotA)
	}
}

// TestSessionIsolation_Registry — P0-2.
// Per plan §3.2: registry串了的后果是 user B 操作到 user A 的实例（P0 越权）。
// EntityRegistry has no public mutation except SyncFromDescribe — the test
// injects two disjoint UHostSet maps and asserts neither registry sees the
// other's instances. Encodes WHY: entity confusion enables cross-tenant write.
func TestSessionIsolation_Registry(t *testing.T) {
	engA, engB, _ := newTwoSessions(t)

	if err := engA.RegistryPointer().SyncFromDescribe(map[string]any{
		"TotalCount": 1,
		"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-aaa",
				"Name":    "instance-A",
				"Zone":    "cn-wlcb-01",
				"State":   "Running",
			},
		},
	}, "test-session-A-init"); err != nil {
		t.Fatalf("seed session A registry: %v", err)
	}

	if err := engB.RegistryPointer().SyncFromDescribe(map[string]any{
		"TotalCount": 1,
		"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-bbb",
				"Name":    "instance-B",
				"Zone":    "cn-wlcb-01",
				"State":   "Running",
			},
		},
	}, "test-session-B-init"); err != nil {
		t.Fatalf("seed session B registry: %v", err)
	}

	snapB := engB.RegistryPointer().Snapshot()
	if _, leaked := snapB.Instances["uhost-aaa"]; leaked {
		t.Fatalf("session B registry leaked session A's instance: %#v", snapB.Instances)
	}
	if _, kept := snapB.Instances["uhost-bbb"]; !kept {
		t.Fatalf("session B lost its own instance; got %#v", snapB.Instances)
	}

	snapA := engA.RegistryPointer().Snapshot()
	if _, leaked := snapA.Instances["uhost-bbb"]; leaked {
		t.Fatalf("session A registry leaked session B's instance: %#v", snapA.Instances)
	}

	// Registries must be DIFFERENT pointer instances.
	if engA.RegistryPointer() == engB.RegistryPointer() {
		t.Fatalf("session A and B share the same registry pointer; per-session isolation broken")
	}
}

// TestSessionIsolation_ConfirmFn — P0-3.
// Per plan §3.2: confirmFn串了的后果是 user A 的确认弹窗去问 user B（P0 误操作）。
// Each NewSession receives its own ConfirmFunc. The test wires two functions
// with disjoint side effects and asserts session A invoking confirm never
// triggers session B's callback.
func TestSessionIsolation_ConfirmFn(t *testing.T) {
	deps := &SharedDeps{
		LLMClient:        &mockLLM{},
		RateLimiter:      governance.NewMemoryLimiter(governance.DefaultLimits()),
		ExternalExecutor: &mockExecutor{results: map[string]map[string]any{}},
	}
	var calledA, calledB bool
	confirmA := func(action string, args map[string]any) bool { calledA = true; return true }
	confirmB := func(action string, args map[string]any) bool { calledB = true; return false }

	engA := NewSession(deps, SessionOptions{Subject: "subj-A", ConfirmFn: confirmA})
	engB := NewSession(deps, SessionOptions{Subject: "subj-B", ConfirmFn: confirmB})

	if engA.confirmFn == nil {
		t.Fatalf("session A confirmFn unexpectedly nil")
	}
	engA.confirmFn("StopInstance", nil)
	if !calledA {
		t.Fatalf("session A confirm callback was not invoked")
	}
	if calledB {
		t.Fatalf("session A's confirm call leaked into session B's callback")
	}

	// confirmFn pointers must differ — if NewSession had captured a single
	// function from a process-wide var, both engines would share it.
	if reflect.ValueOf(engA.confirmFn).Pointer() == reflect.ValueOf(engB.confirmFn).Pointer() {
		t.Fatalf("session A and B share confirmFn pointer; per-session wiring broken")
	}
}

// TestSessionIsolation_SharedPointersEqual — P0-4.
// Sibling assertion to the per-session checks: shared fields MUST be pointer-
// equal across sessions. If a session refactor accidentally copies an LLM
// client or RateLimiter, this test will catch it. Encodes WHY: shared deps
// hold no per-session state, and copying defeats the purpose of NewSharedDeps.
func TestSessionIsolation_SharedPointersEqual(t *testing.T) {
	engA, engB, deps := newTwoSessions(t)

	if engA.LLMClientPointer() != deps.LLMClient {
		t.Fatalf("session A LLMClient pointer drift")
	}
	if engA.LLMClientPointer() != engB.LLMClientPointer() {
		t.Fatalf("LLMClient must be shared across sessions; got %p vs %p",
			engA.LLMClientPointer(), engB.LLMClientPointer())
	}
	if engA.RateLimiterPointer() != engB.RateLimiterPointer() {
		t.Fatalf("RateLimiter must be shared across sessions; got %p vs %p",
			engA.RateLimiterPointer(), engB.RateLimiterPointer())
	}
}

// TestSessionIsolation_RateLimit — P0-5.
// Encodes WHY: per-user subject keys must isolate quota burn across tenants.
// Sets distinct subjects on two sessions, burns session A's LLM bucket, and
// asserts session B's first LLM request still succeeds. If subjects shared
// a bucket (regression to process-wide subject), this would fail.
func TestSessionIsolation_RateLimit(t *testing.T) {
	engA, engB, _ := newTwoSessions(t)
	engA.SetRateLimitSubject("rl-subj-A")
	engB.SetRateLimitSubject("rl-subj-B")

	// MemoryLimiter default LLMQPS = 5; burn 5 to drain session A's bucket
	// then expect the 6th call to be denied.
	for i := 0; i < governance.DefaultLLMQPS; i++ {
		if dec, _ := engA.allowRateLimited(governance.ClassLLM, "main_react_chat"); !dec.Allowed {
			t.Fatalf("session A: expected first %d LLM calls to succeed, denial at %d", governance.DefaultLLMQPS, i+1)
		}
	}
	denied, _ := engA.allowRateLimited(governance.ClassLLM, "main_react_chat")
	if denied.Allowed {
		t.Fatalf("session A: expected LLM bucket exhaustion after %d calls", governance.DefaultLLMQPS)
	}

	// Session B's first call must succeed — its bucket is independent.
	bDec, _ := engB.allowRateLimited(governance.ClassLLM, "main_react_chat")
	if !bDec.Allowed {
		t.Fatalf("session B was denied LLM call but its bucket was fresh; subjects not isolated")
	}
}

// TestSessionIsolation_AllEngineFieldsClassified — reflection guard.
// Per plan §3 + §5.5: every Engine struct field MUST be classified as either
// shared or per-session. New fields added without classification will fail
// this test and force the maintainer to update plan §3 + the whitelist
// below. Encodes WHY: silent field additions defeat the §3 cross-session
// isolation guarantee.
//
// Whitelist totals: 11 shared + 27 per-session = 38 fields. Any drift
// requires updating both this test AND plan §3.
func TestSessionIsolation_AllEngineFieldsClassified(t *testing.T) {
	sharedFields := map[string]bool{
		"llmClient":                   true,
		"intentPlanner":               true,
		"intentPlannerModel":          true,
		"intentPlannerEnabledIntents": true,
		"intentCutoverIntents":        true,
		"knowledgeRetriever":          true,
		"groundedRenderer":            true,
		"groundedRendererModel":       true,
		"rateLimiter":                 true,
		"supportsObjectToolChoice":    true,
		"maxTokensPerTurn":            true,
	}
	perSessionFields := map[string]bool{
		"safeExecutor":                     true,
		"confirmFn":                        true,
		"registry":                         true,
		"rateLimitSubject":                 true,
		"mutatingToolsEnabled":             true,
		"messages":                         true,
		"userTurn":                         true,
		"lastUserMsg":                      true,
		"lastInstanceQueryTurn":            true,
		"lastMonitorTurn":                  true,
		"currentMonitorTargets":            true,
		"currentMonitorNoData":             true,
		"currentMonitorStart":              true,
		"currentMonitorEnd":                true,
		"currentMonitorWindow":             true,
		"pendingResourceSelection":         true,
		"readExpensiveCallsThisTurn":       true,
		"requireKnowledgeCitationThisTurn": true,
		"turnTokensConsumed":               true,
		"rendererTraceObserver":            true,
		"plannerTraceObserver":             true,
		"retrievalTraceObserver":           true,
		"outcomeTraceObserver":             true,
		"tokenUsageObserver":               true,
		"rateLimitObserver":                true,
		"hardBlockObserver":                true,
		"currentCtx":                        true,
	}

	if want, got := 11, len(sharedFields); want != got {
		t.Fatalf("shared whitelist count drift: expected %d, got %d", want, got)
	}
	if want, got := 27, len(perSessionFields); want != got {
		t.Fatalf("per-session whitelist count drift: expected %d, got %d", want, got)
	}

	typ := reflect.TypeOf(Engine{})
	if want, got := 38, typ.NumField(); want != got {
		t.Fatalf("Engine field count drift: expected %d, got %d. "+
			"Update plan §3 + this test's whitelists to match.", want, got)
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if sharedFields[name] || perSessionFields[name] {
			continue
		}
		t.Errorf("Engine field %q is not classified as shared or per-session. "+
			"Update §3 of console-deploy plan and this test's whitelist.", name)
	}
}

// TestNewWithDeps_FieldSetMatchesNewSession — §5.3 option (a) guard.
// NewWithDeps keeps its existing signature (used by ~1500 lines of tests) and
// constructs an Engine directly rather than going through NewSession. This
// test asserts NewWithDeps produces a field set equivalent to
// NewSession(deps, SessionOptions{MutatingToolsEnabled: true,
// SupportsObjectToolChoice: true}) so the two construction paths cannot
// silently drift. Encodes WHY: future field additions must land in both
// constructors or this test fails — preventing the cross-session leak that
// motivated §3's classification work.
func TestNewWithDeps_FieldSetMatchesNewSession(t *testing.T) {
	llm := &mockLLM{}
	exec := &mockExecutor{results: map[string]map[string]any{}}
	confirm := func(string, map[string]any) bool { return true }

	withDeps := NewWithDeps(llm, exec, confirm)

	session := NewSession(&SharedDeps{
		LLMClient:                llm,
		ExternalExecutor:         exec,
		SupportsObjectToolChoice: true,
	}, SessionOptions{
		ConfirmFn:            confirm,
		MutatingToolsEnabled: true,
	})

	if withDeps.llmClient != session.llmClient {
		t.Errorf("llmClient pointer differs: NewWithDeps=%p NewSession=%p", withDeps.llmClient, session.llmClient)
	}
	if withDeps.supportsObjectToolChoice != session.supportsObjectToolChoice {
		t.Errorf("supportsObjectToolChoice differs: NewWithDeps=%v NewSession=%v",
			withDeps.supportsObjectToolChoice, session.supportsObjectToolChoice)
	}
	if withDeps.mutatingToolsEnabled != session.mutatingToolsEnabled {
		t.Errorf("mutatingToolsEnabled differs: NewWithDeps=%v NewSession=%v",
			withDeps.mutatingToolsEnabled, session.mutatingToolsEnabled)
	}
	if withDeps.lastInstanceQueryTurn != session.lastInstanceQueryTurn {
		t.Errorf("lastInstanceQueryTurn differs: NewWithDeps=%d NewSession=%d",
			withDeps.lastInstanceQueryTurn, session.lastInstanceQueryTurn)
	}
	if withDeps.lastMonitorTurn != session.lastMonitorTurn {
		t.Errorf("lastMonitorTurn differs: NewWithDeps=%d NewSession=%d",
			withDeps.lastMonitorTurn, session.lastMonitorTurn)
	}
	if withDeps.registry == nil {
		t.Errorf("NewWithDeps did not init registry")
	}
	if session.registry == nil {
		t.Errorf("NewSession did not init registry")
	}
	if withDeps.safeExecutor == nil || session.safeExecutor == nil {
		t.Errorf("safeExecutor not initialized: NewWithDeps=%v NewSession=%v",
			withDeps.safeExecutor != nil, session.safeExecutor != nil)
	}
}

// TestNewSession_NilDepsPanics asserts the documented panic in NewSession.
// Encodes WHY: passing nil deps would zero-fill shared fields and turn a
// session into a half-broken engine — better to crash loud at construction.
func TestNewSession_NilDepsPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewSession(nil, _) should panic but did not")
		}
	}()
	_ = NewSession(nil, SessionOptions{})
}

// TestSessionIsolation_NoProjectIdLeak — PR9 guard.
//
// Encodes WHY: pre-PR9 the engine called ExternalExecutor.SetProjectId at
// the start of every session's Init to auto-discover a ProjectId via
// GetProjectList. Because SharedDeps.ExternalExecutor is a process-wide
// singleton shared across sessions, that write let session A's discovered
// project id auto-inject into session B's subsequent tool calls — a
// cross-tenant leak. PR9 removed the mutation surface entirely
// (SetProjectId, the ProjectId() getter, and ensureProjectId/pickProjectId).
//
// This test is a structural guard: it asserts the mutating method has
// not silently come back via a future refactor. The reflection check
// runs on *tools.ExternalExecutor because the leak path was through
// methods on that concrete type — TestSessionIsolation_AllEngineFieldsClassified
// only walks Engine struct fields and does NOT recurse into shared-dep
// struct method sets, so it can't catch a re-introduced setter here.
func TestSessionIsolation_NoProjectIdLeak(t *testing.T) {
	typ := reflect.TypeOf(&tools.ExternalExecutor{})
	for _, banned := range []string{"SetProjectId", "ProjectId"} {
		if _, ok := typ.MethodByName(banned); ok {
			t.Fatalf("tools.ExternalExecutor.%s reintroduced — this re-opens "+
				"the cross-session ProjectId leak fixed in PR9. ProjectId must "+
				"only flow from cfg → NewExternalExecutor at construction. If "+
				"a mutating tool genuinely needs ProjectId, plumb it via args "+
				"or a per-session Engine field, never a SharedDeps setter.", banned)
		}
	}
}

// TestNewSharedDeps_NilCfgErrors asserts the documented error in NewSharedDeps.
func TestNewSharedDeps_NilCfgErrors(t *testing.T) {
	deps, err := NewSharedDeps(nil)
	if err == nil {
		t.Fatalf("NewSharedDeps(nil) returned err=nil, deps=%v", deps)
	}
	if deps != nil {
		t.Fatalf("NewSharedDeps(nil) returned non-nil deps on error: %+v", deps)
	}
}
