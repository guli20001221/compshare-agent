package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/tools"

	openai "github.com/sashabaranov/go-openai"
)

type sessionActionJournal struct {
	calls int
	err   error
}

func (j *sessionActionJournal) Execute(ctx context.Context, action string, args map[string]any, call tools.ActionCall) (map[string]any, error) {
	j.calls++
	return call(ctx, action, args)
}

func (j *sessionActionJournal) Err() error { return j.err }

// newTwoSessions constructs two Engines from the same SharedDeps. Used by the
// P0 isolation tests below. mockLLM / mockExecutor live in engine_test.go (same
// package) so we reuse them rather than declaring a parallel stub.
func newTwoSessions(t *testing.T) (engA, engB *Engine, deps *SharedDeps) {
	t.Helper()
	deps = &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewInMemoryRateLimiter(governance.DefaultLimits()),
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
		RateLimiter:      governance.NewInMemoryRateLimiter(governance.DefaultLimits()),
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

func TestSessionIsolation_ActionJournalIsInjectedPerTurn(t *testing.T) {
	inner := &mockExecutor{results: map[string]map[string]any{"StopCompShareInstance": {"RetCode": 0}}}
	deps := &SharedDeps{
		LLMClient: &mockLLM{}, RateLimiter: governance.NewInMemoryRateLimiter(governance.DefaultLimits()), ExternalExecutor: inner,
	}
	journalA := &sessionActionJournal{}
	confirm := func(string, map[string]any) bool { return true }
	engA := NewSession(deps, SessionOptions{
		Subject: "subj-A", ConfirmFn: confirm, MutatingToolsEnabled: true,
		ActionJournal: journalA, RequireActionJournal: true,
	})
	engB := NewSession(deps, SessionOptions{
		Subject: "subj-B", ConfirmFn: confirm, MutatingToolsEnabled: true,
		RequireActionJournal: true,
	})

	_, err := engA.safeExecutor.Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "uhost-a"})
	if err != nil {
		t.Fatalf("session A journaled mutation: %v", err)
	}
	if journalA.calls != 1 {
		t.Fatalf("session A journal calls=%d, want 1", journalA.calls)
	}
	_, err = engB.safeExecutor.Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "uhost-b"})
	if !errors.Is(err, tools.ErrActionJournalRequired) {
		t.Fatalf("session B mutation error=%v, want missing journal", err)
	}
	if journalA.calls != 1 {
		t.Fatalf("session B leaked into session A journal; calls=%d", journalA.calls)
	}
}

func TestEngine_ActionJournalErrorIsCommitBarrier(t *testing.T) {
	deps := &SharedDeps{ExternalExecutor: &mockExecutor{results: map[string]map[string]any{}}}
	poisoned := &sessionActionJournal{err: tools.ErrActionOutcomeUncertain}
	eng := NewSession(deps, SessionOptions{ActionJournal: poisoned, RequireActionJournal: true})
	if !errors.Is(eng.ActionJournalError(), tools.ErrActionOutcomeUncertain) {
		t.Fatalf("coordinator-visible journal error=%v", eng.ActionJournalError())
	}
	required := NewSession(deps, SessionOptions{RequireActionJournal: true})
	if !errors.Is(required.ActionJournalError(), tools.ErrActionJournalRequired) {
		t.Fatalf("missing required journal error=%v", required.ActionJournalError())
	}
	legacy := NewSession(deps, SessionOptions{})
	if err := legacy.ActionJournalError(); err != nil {
		t.Fatalf("legacy session unexpectedly requires journal: %v", err)
	}
}

func TestEngine_UncertainActionReplyRequiresReconciliation(t *testing.T) {
	eng := &Engine{safeExecutor: &tools.SafeToolExecutor{}}
	poisoned := &sessionActionJournal{err: tools.ErrActionOutcomeUncertain}
	eng.safeExecutor = newSafeToolExecutor(&mockExecutor{}, nil, poisoned, true)

	assert := func(condition bool, message string) {
		t.Helper()
		if !condition {
			t.Fatal(message)
		}
	}
	assert(errors.Is(eng.ActionJournalError(), tools.ErrActionOutcomeUncertain), "the engine must expose the poisoned journal")
	assert(actionOutcomeUncertainReply != "", "the reconciliation reply must not be empty")
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

	// InMemoryRateLimiter default LLMQPS = 5; burn 5 to drain session A's bucket
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
// Whitelist totals: 9 shared + 83 per-session = 92 fields. Any drift
// requires updating both this test AND plan §3.
func TestSessionIsolation_AllEngineFieldsClassified(t *testing.T) {
	sharedFields := map[string]bool{
		"llmClient": true,
		// agentLLMClient is the TierAgent (strong-model) client — shared like
		// llmClient (a stateless client wrapper, pointer-equal across sessions).
		// Used by the B8 deploy_model image-matching handler.
		"agentLLMClient":           true,
		"knowledgeRetriever":       true,
		"rateLimiter":              true,
		"supportsObjectToolChoice": true,
		"maxTokensPerTurn":         true,
		// externalExecutor is the RAW shared tool executor (same instance as the
		// one safeExecutor wraps) — pointer-equal across sessions, used only for
		// read-only L0 catalog calls. Shared like llmClient.
		"externalExecutor": true,
		// zoneCatalog is the process-wide zone-display-name cache (or nil →
		// zones.Default()). A shared read-mostly cache, not per-session state.
		"zoneCatalog": true,
	}
	perSessionFields := map[string]bool{
		"safeExecutor":                     true,
		"confirmFn":                        true,
		"confirmEditsFn":                   true,
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
		"lastConfirmationAcceptedThisCall": true,
		"deferTaskCarryThisTurn":           true,
		// Per-turn agentic SearchKnowledge state (P3): whether the tool ran this
		// turn and the hits it returned, used by the final-answer no-raw-leak
		// guard. Per-session by design — sharing would validate one tenant's
		// answer against another tenant's retrieved evidence. Reset every turn.
		"searchKnowledgeRanThisTurn":  true,
		"searchKnowledgeHitsThisTurn": true,
		// Per-turn SearchKnowledge call counter feeding the agent-loop search cap.
		// Per-session/per-turn by design — a shared counter would let one tenant's
		// searches withdraw the tool from another's turn. Reset every turn.
		"searchKnowledgeCallsThisTurn": true,
		// Per-turn ChunkID-keyed evidence ledger (#126), the union of this turn's
		// SearchKnowledge items, consumed by the grounded-answer cite validator.
		// Per-session by design — same cross-tenant-leak reasoning as the hits
		// above. Reset every turn.
		"searchKnowledgeLedgerThisTurn": true,
		// Stable standalone question formulated from the full conversation. It is
		// turn-local and must never cross tenants or turns.
		"resolvedKnowledgeQuestionThisTurn": true,
		// Per-turn reference-ledger observability state: tracks SearchKnowledge
		// activity ids and which chunks came from each activity. Sharing would
		// cross-link one tenant's citations to another tenant's retrieval trace.
		// Reset every turn.
		"searchKnowledgeActivitiesThisTurn":   true,
		"searchKnowledgeActivityIDsByChunkID": true,
		// Per-turn knowledge_qa route marker.
		// Per-session by design — it carries the turn-scoped cite-or-refuse coupling and
		// the runtime-form projection; sharing it would cross one tenant's route decision
		// into another's. Reset every turn.
		"knowledgeQAAgentLoopThisTurn": true,
		// Optional deploy preference extractor injection + its per-turn result.
		// Kept per-session so test doubles / future stateful wrappers cannot
		// leak calls or extracted preferences across users.
		// One cached context judgment per turn. These are reset at the start
		// of ChatWithOptions and must remain session-local: sharing them would
		// apply one user's continue/clear decision to another user's task.
		"turnTokensConsumed": true,
		// Per-turn ReAct loop counters feeding the trace's react_rounds field and
		// the budget terminus. Per-session/per-turn by design — a shared counter
		// would attribute one tenant's loop depth to another's turn. Reset every turn.
		"reactRoundsThisTurn":           true,
		"reactCeilingHitThisTurn":       true,
		"turnModelCallsThisTurn":        true,
		"turnCompletionClassHint":       true,
		"turnCompletionReasonHint":      true,
		"turnCompletionEmittedThisTurn": true,
		"hardBlockStandingThisTurn":     true,
		"hardBlockTraceThisTurn":        true,
		// Per-turn instance-binding observables (#3 StateTrace). Per-session/
		// per-turn by design — sharing would attribute one tenant's bound
		// instance / fact-cache age to another's turn. Reset every turn.
		"selectedInstanceIDAtTurnStart":     true,
		"instanceResolutionSourceThisTurn":  true,
		"factCacheOldestAgeSecondsThisTurn": true,
		"rendererTraceObserver":             true,
		// Per-session: whether this session's history was ever trimmed/compacted.
		// Leaking it across sessions would report a fresh session as already-trimmed.
		"historyTrimmedThisSession":  true,
		"retrievalTraceObserver":     true,
		"freshnessTraceObserver":     true,
		"diagnosisTraceObserver":     true,
		"outcomeTraceObserver":       true,
		"authorizationTraceObserver": true,
		"tokenUsageObserver":         true,
		"rateLimitObserver":          true,
		"hardBlockObserver":          true,
		"turnCompletionObserver":     true,
		// Runtime lifecycle evidence is turn/session-local. Sharing either the
		// event buffer or its observer would mix two tenants' reasoning traces.
		"agentRuntimeEventsThisTurn": true,
		"agentRuntimeObserver":       true,
		"currentCtx":                 true,
		// guidedCreate is a per-turn HTTP capability gate; sharing it would let
		// one client's opt-in change another client's create workflow shape.
		"guidedCreate": true,
		// M1 SessionState fields — per-session by design. SessionState is
		// the JSON-serializable per-session dialog state envelope; mixing
		// it across sessions would be exactly the cross-user leak this
		// test was created to prevent.
		"sessionState":                        true,
		"sessionStateVersion":                 true,
		"sessionStateHydrated":                true,
		"continuityAdvisories":                true,
		"turnContextViewThisTurn":             true,
		"turnContextViewReady":                true,
		"promptSectionIDsThisTurn":            true,
		"memoryUpdateSourceThisTurn":          true,
		"groundingOutcomeThisTurn":            true,
		"promptMessagesRawPeakThisTurn":       true,
		"promptMessagesAssembledPeakThisTurn": true,
		"promptMessagesCapAppliedThisTurn":    true,
		"sessionFactContextEnabled":           true,
		"reactResultProjectionEnabled":        true,
		"reactHistoryCompactionEnabled":       true,
		"verifiedInstanceEvidenceThisTurn":    true,
		"readResponseEvidenceThisTurn":        true,
		"toolResultsByCallThisTurn":           true,
		"actionProposalRanThisTurn":           true,
		"actionProposalDispositionThisTurn":   true,
		"imageContextThisTurn":                true,
		"secretInputsThisTurn":                true,
		"baseUserContext":                     true,
		"displayedResourceSelectionThisTurn":  true,
	}

	if want, got := 8, len(sharedFields); want != got {
		t.Fatalf("shared whitelist count drift: expected %d, got %d", want, got)
	}
	if want, got := 79, len(perSessionFields); want != got {
		t.Fatalf("per-session whitelist count drift: expected %d, got %d", want, got)
	}

	typ := reflect.TypeOf(Engine{})
	if want, got := 87, typ.NumField(); want != got {
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
		LLMClient:                  llm,
		ExternalExecutor:           exec,
		SupportsObjectToolChoice:   true,
		SupportsRequiredToolChoice: true,
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
