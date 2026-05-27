package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/refusal"
	grounded "github.com/compshare-agent/internal/renderer"
	"github.com/compshare-agent/internal/textutil"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"

	openai "github.com/sashabaranov/go-openai"
)

const (
	maxReActRounds = 10
	// maxHistoryMessages is the maximum number of non-system messages to keep.
	// With ~7K system prompt tokens and ~1K per message pair, 40 messages ≈ 27K tokens
	// which fits well within a 32K context window.
	maxHistoryMessages = 40
	// maxPlannerPriorMessages bounds the user/assistant history copied into
	// shadow-planner input. Tool and system messages are intentionally omitted.
	maxPlannerPriorMessages      = 8
	maxPlannerPriorTextRunes     = 2000
	maxKnowledgeHistoryRunes     = 4000
	maxReadExpensiveCallsPerTurn = 20
)

const knowledgeHistoryClipMarker = "\n\n[knowledge answer clipped from conversation history]"
const mutatingToolsDisabledMessage = "当前阶段不直接执行开机、关机、重启、重置密码、创建实例等变更操作。我可以告诉你在控制台怎么操作，具体执行请到控制台完成。"

// monitor_history / account_billing / resource_shortage refusal text moved
// to internal/refusal/templates.go in the C2 hard-block 归一 refactor.
// Call sites import refusal directly; this file no longer declares them.

const (
	rateLimitQPSMessage   = "请求过于频繁，请稍后再试。"
	rateLimitDailyMessage = "今日额度已用完，请明天再试。"
	// tokenBudgetExceededMessage is returned when a single user turn
	// consumed more LLM tokens than maxTokensPerTurn. Surfaces to the
	// user as a normal assistant message (status="blocked" downstream);
	// the partial reply prior to the budget hit is discarded — the loop
	// breaks at iteration boundary, so any tool_call already issued has
	// its tool_result on the wire before this frame.
	tokenBudgetExceededMessage = "本次问题消耗的算力已超过单次上限，请简化问题或拆分提问。"
)

const (
	toolCapExceededMessage         = "本次最多支持查询 20 台实例，请缩小范围后重试。"
	historyWindowExceededMessage   = "历史监控时间窗最多支持 24 小时，请缩短时间范围后重试。"
	readExpensiveTurnBudgetMessage = "本轮读取类查询次数已达上限，请缩小问题范围后重试。"
)

// Force-tool / hard-block priority chain (highest first):
//
//  1. isAccountBillingUnsupported    -> canned reply, no LLM call (hard-block)
//  2. isResourceShortageQuestion     -> canned reply, no LLM call (hard-block; error 226604)
//  3. isUnsupportedHistoricalMonitorQuestion -> canned reply, no LLM call
//  4. shouldForceMonitorRecall       -> tool_choice=GetCompShareInstanceMonitor
//                                       (BRIDGE T-001.f1, capability-gated)
//  5. (future) f3a resource info follow-up (BRIDGE T-001.f3a, if implemented)
//
// Capability gating: force-tool paths that emit object tool_choice MUST
// short-circuit when supportsObjectToolChoice=false. ds v4 flash in thinking
// mode 400s on object tool_choice; emitting it would break the request entirely
// rather than degrade to soft routing.
//
// shouldForceBillingDiagnosis was removed 2026-05-08: ds v4 flash returns 400
// on object tool_choice in thinking mode, and auto-routing achieves the same
// success rate as required (5/6). See eval/capability/2026-05-08-ds-v4-flash-
// tool-choice-probe.md.
//
// Each force step is short-circuited by a higher one. When adding a new
// force-tool path, update this comment and extract a single pickForcedTool()
// decision point when the priority chain grows beyond this narrow bridge set.

var (
	beijingZone          = time.FixedZone("CST", 8*3600)
	isoDateRE            = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	clockRangeRE         = regexp.MustCompile(`(?:\b\d{1,2}:\d{2}\b|\d{1,2}点(?:\d{1,2}分)?)\s*(?:~|-|到|至)\s*(?:\b\d{1,2}:\d{2}\b|\d{1,2}点(?:\d{1,2}分)?)`)
	historicalDurationRE = regexp.MustCompile(`(?i)(?:过去|近|最近|last|past|previous|recent)\s*(?:\d+\s*)?(?:分钟|小时|天|周|月|hour|hours|day|days|week|weeks|month|months|h|d)`)
	percentValueRE       = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*%`)
)

// ConfirmFunc asks the user to confirm an L1 operation. Returns true if confirmed.
type ConfirmFunc func(action string, args map[string]any) bool

// LLMClient abstracts the LLM chat interface for testability.
type LLMClient interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

type IntentPlanner interface {
	Plan(ctx context.Context, input intent.PlannerInput) (intent.PlannerResult, error)
}

type KnowledgeRetriever interface {
	Retrieve(question, productArea string) knowledge.RetrievalResult
}

// HistoryMessage is a simplified turn for rehydrating a conversation from
// persistent storage (e.g. MySQL). Only user and assistant roles are accepted;
// all other roles and empty content are silently skipped.
type HistoryMessage struct {
	Role    string
	Content string
}

// ChatOptions configure optional callbacks for ChatWithOptions. Callbacks are
// invoked synchronously on the caller's goroutine. OnTextDelta receives the
// final assistant reply, replayed in chunk order when the LLM's raw content
// is returned verbatim, or as a single override chunk when engine guards
// rewrite the reply.
type ChatOptions struct {
	// OnTextDelta, if non-nil, is called once per text token in order, but
	// only for the final LLM reply (not for intermediate ReAct tool-call rounds).
	// Canned-reply branches (account_billing_unsupported, monitor_history) skip
	// the LLM entirely and therefore never call this.
	OnTextDelta func(string)
	// OnUsage, if non-nil, is called once after the final LLM call returns its
	// usage data.
	OnUsage func(llm.TokenUsage)
	// ImageContext, if non-empty, is a structured caption extracted from a
	// user-uploaded image. It is added to the LLM-facing message after
	// pre-block keyword checks so screenshot UI labels (e.g. "运维监控",
	// "最近访问") do not trigger false-positive hard blocks.
	ImageContext string
	// ConfirmFunc, if non-nil, overrides the engine's stored ConfirmFunc for
	// this turn only. Used by the HTTP path to inject an SSE-backed confirm
	// that blocks on a channel instead of stdin.
	ConfirmFunc func(action string, args map[string]any) bool
}

type IntentPlannerOptions struct {
	EnabledIntents []intent.Intent
	Model          string
}

// Engine runs the ReAct loop: User → LLM → Tool → LLM → ... → Reply.
type Engine struct {
	llmClient                        LLMClient
	safeExecutor                     *tools.SafeToolExecutor
	registry                         *entity.EntityRegistry
	intentPlanner                    IntentPlanner
	intentPlannerModel               string
	intentPlannerEnabledIntents      map[intent.Intent]struct{}
	intentCutoverIntents             map[intent.Intent]struct{}
	knowledgeRetriever               KnowledgeRetriever
	groundedRenderer                 grounded.Renderer
	groundedRendererModel            string
	rendererTraceObserver            func(observability.RendererTrace)
	plannerTraceObserver             func(observability.PlannerTrace)
	retrievalTraceObserver           func(observability.RetrievalTrace)
	outcomeTraceObserver             func(observability.OutcomeTrace)
	tokenUsageObserver               func(llm.TokenUsage)
	rateLimiter                      governance.RateLimiter
	rateLimitSubject                 string
	rateLimitObserver                func(governance.Decision)
	readExpensiveCallsThisTurn       int
	requireKnowledgeCitationThisTurn bool
	// maxTokensPerTurn caps total LLM tokens (prompt + completion) per
	// user turn. 0 = disabled. Copied from SharedDeps in NewSession.
	maxTokensPerTurn int
	// turnTokensConsumed accumulates tokenUsageTotal(usage) across every
	// LLM call within the current Chat() invocation. Reset at the top of
	// Chat. Read at ReAct loop iteration boundaries to enforce
	// maxTokensPerTurn — never mid tool_call / tool_result pair.
	turnTokensConsumed       int
	hardBlockObserver        func(observability.EngineHardBlockTrace)
	confirmFn                ConfirmFunc
	messages                 []openai.ChatCompletionMessage // conversation history
	userTurn                 int                            // incremented at start of each Chat() call
	lastInstanceQueryTurn    int                            // set to userTurn on successful DescribeCompShareInstance
	lastMonitorTurn          int                            // set to userTurn on successful GetCompShareInstanceMonitor
	currentMonitorTargets    []string                       // historical monitor targets queried in the current turn
	currentMonitorNoData     []string                       // current-turn historical monitor targets with no data samples
	currentMonitorStart      int64                          // start of the current historical monitor window, if any
	currentMonitorEnd        int64                          // end of the current historical monitor window, if any
	currentMonitorWindow     bool                           // true when currentMonitorStart/End are known
	pendingResourceSelection *pendingResourceSelection
	// supportsObjectToolChoice gates force-tool guards (e.g. shouldForceMonitorRecall)
	// from sending object tool_choice on models that don't support it (notably
	// deepseek-v4-flash in thinking mode, which 400s). When false, guards still
	// run their detection logic but fall through to LLM auto routing.
	supportsObjectToolChoice bool
	// mutatingToolsEnabled controls whether instance-changing workflows and
	// L1 mutating API actions are exposed and executable. Production defaults
	// to read-only until these operations are product-ready.
	mutatingToolsEnabled bool
	// Raw user message for the current turn. Set at the start of Chat().
	// Read by executeDiagnosis guards for signal matching. Never mutated
	// mid-turn.
	lastUserMsg string
	// Turn-scoped planner intent. Set by tryPlannerDispatch when the planner
	// classifies but the handler falls back to ReAct. Consumed by the ReAct
	// loop to scope the tool list via intent.IntentToolSubset. Reset per turn.
	lastPlannerIntentThisTurn intent.Intent
	imageContextThisTurn      string
	baseUserContext           string
	// currentCtx holds the context for the current ChatWithOptions call.
	// Set at the start of ChatWithOptions and cleared (nil) on return.
	currentCtx context.Context
	// sessionState is the JSON-serializable per-session state injected by
	// SetSessionState before each Chat turn and read back via
	// SessionStateSnapshot after the turn. See session_state.go.
	// M1 contract: this field is only mutated by SetSessionState /
	// ClearSessionState; M2 will wire ToolFactExtractor to also update
	// it from inside the turn.
	sessionState         SessionState
	sessionStateVersion  int
	sessionStateHydrated bool
}

// SharedDeps groups Engine fields that are safe to share across sessions.
// All fields here are either stateless wrappers (LLM/Planner/Renderer
// clients), read-only data (knowledge corpus), or internally-locked state
// (RateLimiter has its own mutex). See plan §3.1 / §5 for the full
// classification rationale.
//
// IntentPlanner / KnowledgeRetriever / GroundedRenderer are exported so the
// server bootstrap (A3) can assign them directly on a SharedDeps assembled
// by NewSharedDeps. CLI keeps populating them via Engine.SetIntentPlanner /
// SetKnowledgeRetriever / SetGroundedRenderer on the per-process Engine
// returned by engine.New; that path stays valid because NewSession copies
// these fields into the Engine and the setters then overwrite them with
// the same instance. ApplySharedDepsFromEnv (planned for A3, see plan §5.6)
// will unify CLI/server env-driven setup; for A2 it is deferred.
//
// Do NOT add a builder pattern (`WithIntentPlanner(...)`). SharedDeps is
// frozen as soon as the first NewSession is called; later runtime mutation
// would race against in-flight sessions reading these fields.
type SharedDeps struct {
	LLMClient                   LLMClient
	IntentPlanner               IntentPlanner
	IntentPlannerModel          string
	IntentPlannerEnabledIntents map[intent.Intent]struct{}
	IntentCutoverIntents        map[intent.Intent]struct{}
	KnowledgeRetriever          KnowledgeRetriever
	GroundedRenderer            grounded.Renderer
	GroundedRendererModel       string
	RateLimiter                 governance.RateLimiter
	SupportsObjectToolChoice    bool
	// MaxTokensPerTurn caps total LLM tokens summed across one user turn.
	// 0 = disabled. Process-wide constant; copied into every NewSession.
	MaxTokensPerTurn int
	// ExternalExecutor is the underlying tool executor shared across sessions
	// (holds AK/SK + HTTP client). Each NewSession wraps it in a fresh
	// SafeToolExecutor so per-session confirmFn stays isolated.
	ExternalExecutor tools.ToolExecutor
}

// SessionOptions configures a per-session Engine. Server passes a freshly
// derived Subject + per-connection ConfirmFn; CLI passes a process-wide
// Subject and a terminal-stdin-based ConfirmFn.
type SessionOptions struct {
	Subject              string
	ConfirmFn            ConfirmFunc
	MutatingToolsEnabled bool
}

// NewSharedDeps assembles the always-shared engine dependencies from config.
// Call once at process startup; share the result across every NewSession.
// Planner / KnowledgeRetriever / GroundedRenderer are NOT populated here —
// they are env-driven and the caller assigns them on the returned struct
// (server) or via Engine setters post-NewSession (CLI).
func NewSharedDeps(cfg *config.Config) (*SharedDeps, error) {
	if cfg == nil {
		return nil, errors.New("engine.NewSharedDeps: cfg is nil")
	}
	cap := llm.LookupCapability(cfg.Agent.LLM.BaseURL, cfg.Agent.LLM.Model)
	return &SharedDeps{
		LLMClient: llm.NewClient(cfg.Agent.LLM),
		// MemoryLimiter is process-local and suitable for local demo or
		// single-instance deployment only. Multi-replica production needs a
		// centralized limiter such as Redis or an API gateway.
		RateLimiter:              governance.NewMemoryLimiter(cfg.Agent.RateLimit.Limits()),
		SupportsObjectToolChoice: cap.SupportsObjectToolChoice,
		MaxTokensPerTurn:         cfg.Agent.RateLimit.MaxTokensPerTurn,
		ExternalExecutor:         tools.NewExternalExecutor(cfg.Agent),
	}, nil
}

// NewSession constructs a per-connection Engine from shared dependencies and
// per-session options. Each Engine owns its own conversation history,
// entity registry, monitor-window cursors, and turn counters; nothing
// per-conversation is shared with sibling sessions.
//
// SECURITY: deps.RateLimiter is shared so cross-session quota fairness is
// preserved (subject keys keep tenants in separate buckets — see A1).
// Engine.messages / Engine.registry / Engine.safeExecutor are per-session
// so user A's chat history and entity registry cannot leak to user B.
func NewSession(deps *SharedDeps, opts SessionOptions) *Engine {
	if deps == nil {
		panic("engine.NewSession: deps is nil")
	}
	eng := &Engine{
		// ── shared (pointer-equal across sessions) ──
		llmClient:                   deps.LLMClient,
		intentPlanner:               deps.IntentPlanner,
		intentPlannerModel:          deps.IntentPlannerModel,
		intentPlannerEnabledIntents: deps.IntentPlannerEnabledIntents,
		intentCutoverIntents:        deps.IntentCutoverIntents,
		knowledgeRetriever:          deps.KnowledgeRetriever,
		groundedRenderer:            deps.GroundedRenderer,
		groundedRendererModel:       deps.GroundedRendererModel,
		rateLimiter:                 deps.RateLimiter,
		supportsObjectToolChoice:    deps.SupportsObjectToolChoice,
		maxTokensPerTurn:            deps.MaxTokensPerTurn,

		// ── per-session (fresh instance every call) ──
		confirmFn:             opts.ConfirmFn,
		registry:              entity.NewRegistry(),
		rateLimitSubject:      opts.Subject,
		mutatingToolsEnabled:  opts.MutatingToolsEnabled,
		lastInstanceQueryTurn: -1,
		lastMonitorTurn:       -1,
		// messages, userTurn, lastUserMsg, currentMonitor*, pendingResourceSelection,
		// readExpensiveCallsThisTurn, requireKnowledgeCitationThisTurn,
		// *Observer fields all start at zero values which is correct.
	}
	eng.safeExecutor = newSafeToolExecutor(deps.ExternalExecutor, opts.ConfirmFn)
	eng.safeExecutor.SetMutatingToolsEnabled(opts.MutatingToolsEnabled)
	return eng
}

// New is the legacy CLI constructor. It assembles SharedDeps from cfg, derives
// the rate-limit subject from the public key (process-wide, since CLI has
// only one identity), and returns a single Engine. Server path MUST NOT use
// this — it must call NewSharedDeps once and NewSession per connection so
// each tenant gets its own session.
func New(cfg *config.Config, confirmFn ConfirmFunc) *Engine {
	deps, err := NewSharedDeps(cfg)
	if err != nil {
		// Preserve original New() error-free contract for CLI callers.
		panic(fmt.Sprintf("engine.New: %v", err))
	}
	subject, ok := governance.SubjectKeyFromPublicKey(cfg.Agent.PublicKey)
	if !ok {
		fmt.Fprintln(os.Stderr, "warning: rate limiter using anonymous subject (public key missing)")
	}
	return NewSession(deps, SessionOptions{
		Subject:              subject,
		ConfirmFn:            confirmFn,
		MutatingToolsEnabled: false,
	})
}

// NewWithDeps creates an Engine with injected dependencies (for testing).
// Defaults supportsObjectToolChoice to true so existing tests that exercise
// force-tool guards continue to assert the forced ToolChoice. Tests that
// need the capability-gated path can flip the field via setter.
func NewWithDeps(client LLMClient, executor tools.ToolExecutor, confirmFn ConfirmFunc) *Engine {
	eng := &Engine{
		llmClient:                client,
		confirmFn:                confirmFn,
		registry:                 entity.NewRegistry(),
		rateLimitSubject:         governance.AnonymousSubjectKey,
		lastInstanceQueryTurn:    -1,
		lastMonitorTurn:          -1,
		supportsObjectToolChoice: true,
		mutatingToolsEnabled:     true,
	}
	eng.safeExecutor = newSafeToolExecutor(executor, confirmFn)
	return eng
}

// setSupportsObjectToolChoice is an internal helper for tests that need to
// exercise capability-gated force-tool behavior. Production code sets this
// via LookupCapability in New().
func (e *Engine) setSupportsObjectToolChoice(v bool) {
	e.supportsObjectToolChoice = v
}

// SetMutatingToolsEnabled explicitly enables or disables instance-changing
// workflows and L1 mutating API actions. The CLI leaves this disabled unless
// COMPSHARE_ENABLE_MUTATING_TOOLS=1 is set.
func (e *Engine) SetMutatingToolsEnabled(v bool) {
	e.mutatingToolsEnabled = v
	if e.safeExecutor != nil {
		e.safeExecutor.SetMutatingToolsEnabled(v)
	}
}

func (e *Engine) SetIntentPlanner(planner IntentPlanner, opts IntentPlannerOptions) {
	e.intentPlanner = planner
	e.intentPlannerModel = opts.Model
	e.intentPlannerEnabledIntents, e.intentCutoverIntents = BuildIntentPlannerMaps(opts.EnabledIntents)
}

// BuildIntentPlannerMaps converts the configured EnabledIntents slice into the
// two derived sets the engine consults during planning. Extracted so both
// Engine.SetIntentPlanner (CLI path) and a future ApplySharedDepsFromEnv
// helper (A3, server path) build the same maps.
func BuildIntentPlannerMaps(enabled []intent.Intent) (enabledMap, cutoverMap map[intent.Intent]struct{}) {
	enabledMap = map[intent.Intent]struct{}{}
	cutoverMap = map[intent.Intent]struct{}{}
	for _, e := range enabled {
		if e == intent.IntentResourceInfo ||
			e == intent.IntentMonitorQuery ||
			e == intent.IntentDiagnosis ||
			e == intent.IntentVagueFailure ||
			intent.IsCapabilityIntent(e) {
			enabledMap[e] = struct{}{}
		}
		switch e {
		case intent.IntentResourceInfo, intent.IntentMonitorQuery:
			cutoverMap[e] = struct{}{}
		default:
			// Capability Registry v1: any registered capability intent is
			// admissible to the cutover set without per-case wiring here.
			if intent.IsCapabilityIntent(e) {
				cutoverMap[e] = struct{}{}
			}
		}
	}
	return enabledMap, cutoverMap
}

func (e *Engine) SetPlannerTraceObserver(observer func(observability.PlannerTrace)) {
	e.plannerTraceObserver = observer
}

func (e *Engine) SetKnowledgeRetriever(retriever KnowledgeRetriever) {
	// Engine treats a non-nil retriever as the Stage 2B retrieval gate. CLI
	// code owns env parsing and only calls this after USE_KNOWLEDGE_RETRIEVAL
	// and corpus loading succeed.
	e.knowledgeRetriever = retriever
}

func (e *Engine) SetGroundedRenderer(r grounded.Renderer, model string) {
	e.groundedRenderer = r
	e.groundedRendererModel = model
}

func (e *Engine) SetRendererTraceObserver(observer func(observability.RendererTrace)) {
	e.rendererTraceObserver = observer
}

func (e *Engine) SetRetrievalTraceObserver(observer func(observability.RetrievalTrace)) {
	e.retrievalTraceObserver = observer
}

func (e *Engine) SetOutcomeTraceObserver(observer func(observability.OutcomeTrace)) {
	e.outcomeTraceObserver = observer
}

func (e *Engine) SetTokenUsageObserver(observer func(llm.TokenUsage)) {
	e.tokenUsageObserver = observer
}

func (e *Engine) SetRateLimitObserver(observer func(governance.Decision)) {
	e.rateLimitObserver = observer
}

func (e *Engine) RateLimitSubjectKey() string {
	return e.rateLimitSubject
}

// SetRateLimitSubject overrides the subject derived at Engine.New so the
// server path can swap to the per-WS-connection tenant identity right after
// engine.NewSession (A2). Returns the previous subject for tests that need
// to assert the swap actually happened.
func (e *Engine) SetRateLimitSubject(subject string) string {
	prev := e.rateLimitSubject
	e.rateLimitSubject = subject
	return prev
}

func (e *Engine) RateLimitDecision(req governance.Request) governance.Decision {
	decision, _ := e.allowRateLimited(req.Class, req.Action)
	return decision
}

func (e *Engine) SetHardBlockObserver(observer func(observability.EngineHardBlockTrace)) {
	e.hardBlockObserver = observer
}

// ── Snapshot accessors (tests only) ──
//
// The following methods exist to let cross-session isolation tests assert
// pointer identity on shared fields and pointer non-identity on per-session
// state. Production code MUST NOT depend on them.

// MessagesSnapshot returns a copy of the current conversation history. Used
// by tests to assert per-session message isolation without exposing the
// internal slice. Production code must read messages through Chat/Init.
func (e *Engine) MessagesSnapshot() []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, len(e.messages))
	copy(out, e.messages)
	return out
}

// LLMClientPointer returns the underlying LLMClient interface value so
// session-isolation tests can call require.Same to assert sessions share
// one instance. Test-only.
func (e *Engine) LLMClientPointer() LLMClient { return e.llmClient }

// KnowledgeRetrieverPointer returns the underlying KnowledgeRetriever for
// session-isolation tests. Test-only.
func (e *Engine) KnowledgeRetrieverPointer() KnowledgeRetriever { return e.knowledgeRetriever }

// IntentPlannerPointer returns the underlying IntentPlanner for
// session-isolation tests. Test-only.
func (e *Engine) IntentPlannerPointer() IntentPlanner { return e.intentPlanner }

// RateLimiterPointer returns the underlying RateLimiter for
// session-isolation tests. Test-only.
func (e *Engine) RateLimiterPointer() governance.RateLimiter { return e.rateLimiter }

// RegistryPointer returns the per-session EntityRegistry pointer so tests
// can assert that two sessions hold DIFFERENT registries. Test-only.
func (e *Engine) RegistryPointer() *entity.EntityRegistry { return e.registry }

func newSafeToolExecutor(executor tools.ToolExecutor, confirmFn ConfirmFunc) *tools.SafeToolExecutor {
	var safeConfirm tools.ConfirmFunc
	if confirmFn != nil {
		safeConfirm = tools.ConfirmFunc(confirmFn)
	}
	return tools.NewSafeToolExecutor(executor, tools.WithConfirmFunc(safeConfirm))
}

// Init performs first-turn context injection:
// calls DescribeCompShareInstance and builds the system prompt.
// Returns opening suggestions.
func (e *Engine) Init(ctx context.Context) ([]prompt.Suggestion, error) {
	// PR9: removed automatic ProjectId discovery (was: e.ensureProjectId(ctx)).
	// Discovery mutated a SharedDeps singleton and leaked across sessions.
	// ProjectId now flows from cfg → ExternalExecutor at construction only.
	// When mutating tools that need ProjectId (e.g. UpdateCompShareStopScheduler)
	// open up, plumb a per-session value through args, not via a setter.

	// Auto-inject user instance context
	userCtx := "暂无用户信息"
	result, err := e.refreshRegistry(ctx, entity.RefreshReasonInit)
	if err != nil {
		if msg, ok := friendlyToolErrorMessage(err); ok {
			fmt.Fprintln(os.Stderr, msg)
		}
		// Context injection is best-effort; continue with default context.
		_ = err
	} else {
		userCtx = prompt.FormatInstanceContext(result)
	}

	e.baseUserContext = userCtx
	systemPrompt := prompt.BuildSystemWithOptions(userCtx, prompt.BuildOptions{MutatingToolsEnabled: e.mutatingToolsEnabled})
	e.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}

	// Determine suggestions based on user state
	stage := prompt.NewUser
	if err == nil {
		stage = prompt.ClassifyUser(result)
	}
	return prompt.GetSuggestions(stage), nil
}

func (e *Engine) refreshRegistry(ctx context.Context, reason entity.RefreshReason) (map[string]any, error) {
	if e.registry == nil {
		return e.executeRawTool(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100}, tools.OriginDirectLLM)
	}
	result, err := e.registry.RefreshResult(ctx, e.toolExecutorFor(tools.OriginDirectLLM), reason)
	if err == nil {
		e.lastInstanceQueryTurn = e.userTurn
	}
	return result, err
}

func (e *Engine) singleRegistryInstance() (id, name string) {
	if e.registry == nil {
		return "", ""
	}
	if e.registry.NeedsRefresh(time.Now()) {
		return "", ""
	}
	snap := e.registry.Snapshot()
	if snap.TotalCount != 1 || snap.Truncated || len(snap.Instances) != 1 {
		return "", ""
	}
	for uid, inst := range snap.Instances {
		return uid, inst.Name
	}
	return "", ""
}

// RegistryTraceState returns the immutable registry fields reserved by trace.
// It does not expose the registry object, maps, or lock to callers.
func (e *Engine) RegistryTraceState(now time.Time) observability.EntityRegistryTrace {
	if e == nil || e.registry == nil {
		return observability.EntityRegistryTrace{SyncEvent: "unavailable"}
	}
	state := e.registry.TraceState(now)
	return observability.EntityRegistryTrace{
		SnapshotID: state.SnapshotID,
		AgeSeconds: state.AgeSeconds,
		SyncEvent:  state.SyncEvent,
	}
}

// RegistrySnapshot returns an immutable entity snapshot for shadow planner
// validation. It does not expose the registry object, maps, or lock to callers.
func (e *Engine) RegistrySnapshot() entity.RegistrySnapshot {
	if e == nil || e.registry == nil {
		return entity.RegistrySnapshot{SyncEvent: string(entity.SyncEventUnavailable)}
	}
	return e.registry.Snapshot()
}

// PlannerPriorTextSnapshot returns a bounded, read-only text projection of
// prior user/assistant turns for shadow-planner provenance checks. It excludes
// system prompts and tool-result JSON so shadow mode does not expand the data
// surface beyond conversational text.
func (e *Engine) PlannerPriorTextSnapshot() string {
	if e == nil || len(e.messages) == 0 {
		return ""
	}
	lines := make([]string, 0, maxPlannerPriorMessages)
	for i := len(e.messages) - 1; i >= 0 && len(lines) < maxPlannerPriorMessages; i-- {
		msg := e.messages[i]
		role := ""
		switch msg.Role {
		case openai.ChatMessageRoleUser:
			role = "user"
		case openai.ChatMessageRoleAssistant:
			role = "assistant"
		default:
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		lines = append(lines, role+": "+content+"\n")
	}
	var b strings.Builder
	included := make([]string, 0, len(lines))
	budget := maxPlannerPriorTextRunes
	for _, line := range lines {
		runes := []rune(line)
		if len(runes) > budget {
			if len(included) == 0 && budget > 0 {
				included = append(included, string(runes[:budget]))
			}
			break
		}
		included = append(included, line)
		budget -= len(runes)
	}
	for i := len(included) - 1; i >= 0; i-- {
		b.WriteString(included[i])
	}
	return strings.TrimSpace(b.String())
}

// InitWithContext performs context injection with a pre-built user context string,
// bypassing the DescribeCompShareInstance API call. Used for testing.
func (e *Engine) InitWithContext(userCtx string) {
	e.baseUserContext = userCtx
	systemPrompt := prompt.BuildSystemWithOptions(userCtx, prompt.BuildOptions{MutatingToolsEnabled: e.mutatingToolsEnabled})
	e.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}
}

// RehydrateHistory rebuilds the message history from a prior session stored in
// persistent storage. It replaces any existing history with a fresh system
// prompt followed by the supplied user/assistant turns. Empty content and
// non-user/non-assistant roles are silently skipped.
func (e *Engine) RehydrateHistory(msgs []HistoryMessage) {
	e.baseUserContext = ""
	systemPrompt := prompt.BuildSystemWithOptions("", prompt.BuildOptions{MutatingToolsEnabled: e.mutatingToolsEnabled})
	e.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: systemPrompt}}
	for _, msg := range msgs {
		if msg.Content == "" {
			continue
		}
		switch msg.Role {
		case openai.ChatMessageRoleUser, openai.ChatMessageRoleAssistant:
			e.messages = append(e.messages, openai.ChatCompletionMessage{Role: msg.Role, Content: msg.Content})
		}
	}
}

// SetSessionState installs the prior persisted SessionState and the
// context_version that was read together with it. Must be called BEFORE
// ChatWithOptions. Safe to call once per Lease — caller (handleChat) is
// already serialized via agentpool.Lease.
//
// The state is treated as immutable input for the current turn; mutations
// during the turn produce a new state visible via SessionStateSnapshot.
//
// M2 (2026-05-24) added version-aware merge: when this engine already
// has hydrated state with a higher-or-equal version, the incoming state
// is treated as STALE — its RecentFacts are merged in via
// mergeFactsByProducedAt, but the scalar fields (SelectedInstance{ID,Name},
// LastIntent) keep the in-memory values. This is the M1 forward-note
// (docs/agent/plan/m1-session-state-cas.md:429) implementation.
//
// When does the merge path fire?
//
//   - Cross-replica race (future multi-replica deploy): replica A wrote
//     facts at v=N, then replica B's next-turn hydrate sees v=N from a
//     stale read, but B's in-memory state already advanced past v=N.
//     Without the guard, B would clobber its own newer state with the
//     stale read.
//   - Defense-in-depth: handlers_chat.go always calls ClearSessionState
//     before SetSessionState, so the merge path is rarely triggered in
//     single-replica today. But a future buggy caller skipping the clear
//     would step exactly on the cached-Engine reuse bug M1 prevented;
//     this guard is the secondary defense.
//
// Single-replica behavior unchanged: ClearSessionState resets hydrated
// to false, so SetSessionState always takes the !hydrated branch and
// fully overwrites — exactly the M1 contract.
func (e *Engine) SetSessionState(state SessionState, version int) {
	if e.sessionStateHydrated && version <= e.sessionStateVersion {
		e.sessionState.RecentFacts = mergeFactsByProducedAt(e.sessionState.RecentFacts, state.RecentFacts)
		// SelectedInstance{ID,Name} / LastIntent / SchemaVersion: keep
		// the in-memory value. The local engine has not yet persisted,
		// so its scalars are at-or-newer than the incoming row.
		return
	}
	e.sessionState = state
	e.sessionStateVersion = version
	e.sessionStateHydrated = true
}

// ClearSessionState resets the per-turn SessionState to its zero value
// and marks the engine as un-hydrated. Callers (handleChat) MUST invoke
// this immediately after Lease, BEFORE attempting ParsePersistedContext +
// SetSessionState. Reason: agentpool.Pool reuses the same *engine.Engine
// across turns (LRU 200 / 30min), so without an explicit clear, a parse
// failure on turn N+1 would leave hydrated=true sticky from turn N and
// cause the persist-on-success path to overwrite the row using stale
// state. M1 has no in-engine writer so the immediate impact is small,
// but M2 would step directly on this — clear from the start.
func (e *Engine) ClearSessionState() {
	e.sessionState = SessionState{}
	e.sessionStateVersion = 0
	e.sessionStateHydrated = false
}

// SessionStateSnapshot returns the current SessionState plus the version
// that should be passed back to SessionStore.UpdateContext as the CAS
// expectedVersion, and a hydrated flag indicating whether SetSessionState
// was successfully called this turn. Callers MUST check hydrated before
// persisting — persisting an un-hydrated zero state would overwrite the
// row, which is exactly the bug we want to avoid on parse-failure paths.
func (e *Engine) SessionStateSnapshot() (state SessionState, version int, hydrated bool) {
	return e.sessionState, e.sessionStateVersion, e.sessionStateHydrated
}

// refreshSystemPrompt rebuilds e.messages[0] with the current SessionState
// injected into the user context section. Called per-turn at the start of
// ChatWithOptions, AFTER SetSessionState has been called (HTTP handler
// serializes: ClearSessionState → SetSessionState → ChatWithOptions).
// This solves the HTTP timing issue: RehydrateHistory builds the initial
// system prompt with empty userContext because SessionState isn't
// available yet; refreshSystemPrompt patches it once state is hydrated.
// CLI path (hydrated=false): rebuilds from baseUserContext without
// appending instance info, so the result is identical to the Init prompt.
func (e *Engine) refreshSystemPrompt() {
	if len(e.messages) == 0 || e.messages[0].Role != openai.ChatMessageRoleSystem {
		return
	}
	ctx := e.baseUserContext
	if ctx == "" {
		ctx = "暂无用户信息"
	}
	if e.sessionStateHydrated && e.sessionState.SelectedInstanceID != "" {
		if e.sessionState.SelectedInstanceName != "" {
			ctx += "\n\n当前会话已选实例：" + e.sessionState.SelectedInstanceName + "（" + e.sessionState.SelectedInstanceID + "）"
		} else {
			ctx += "\n\n当前会话已选实例：" + e.sessionState.SelectedInstanceID
		}
	}
	if id, name := e.singleRegistryInstance(); id != "" {
		if name != "" {
			ctx += "\n\n当前账户只有 1 个实例：" + name + "（" + id + "），操作时可直接使用，无需追问。"
		} else {
			ctx += "\n\n当前账户只有 1 个实例：" + id + "，操作时可直接使用，无需追问。"
		}
	}
	e.messages[0].Content = prompt.BuildSystemWithOptions(ctx, prompt.BuildOptions{MutatingToolsEnabled: e.mutatingToolsEnabled})
}

// Chat processes one user message through the ReAct loop and returns the final text reply.
// The callback is invoked for each intermediate step (tool calls, thinking, etc.).
// It delegates to ChatWithOptions with empty options (no streaming callbacks).
func (e *Engine) Chat(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, error) {
	return e.ChatWithOptions(ctx, userMsg, onStep, ChatOptions{})
}

// ChatWithOptions is like Chat but accepts streaming callbacks via opts.
// OnTextDelta is buffered per-round and only replayed on the final text branch
// (never on intermediate tool-call rounds). OnUsage is called once after the
// final LLM reply. Canned-reply branches (account_billing_unsupported,
// monitor_history_unsupported) skip the LLM and therefore never fire callbacks.
func (e *Engine) ChatWithOptions(ctx context.Context, userMsg string, onStep func(StepEvent), opts ChatOptions) (string, error) {
	e.userTurn++
	e.currentCtx = ctx
	defer func() { e.currentCtx = nil }()
	if u, ok := tools.UserFrom(ctx); ok {
		if subject, ok := governance.SubjectKeyFromOrganization(u.TopOrganizationID, u.OrganizationID); ok {
			e.rateLimitSubject = subject
		}
	}
	// Per-turn ConfirmFunc override (HTTP path injects SSE-backed confirm).
	if opts.ConfirmFunc != nil {
		origConfirm := e.confirmFn
		e.confirmFn = ConfirmFunc(opts.ConfirmFunc)
		e.safeExecutor.SetConfirmFunc(tools.ConfirmFunc(opts.ConfirmFunc))
		defer func() {
			e.confirmFn = origConfirm
			e.safeExecutor.SetConfirmFunc(tools.ConfirmFunc(origConfirm))
		}()
	}

	e.lastUserMsg = userMsg
	e.imageContextThisTurn = opts.ImageContext
	e.readExpensiveCallsThisTurn = 0
	e.requireKnowledgeCitationThisTurn = false
	e.turnTokensConsumed = 0
	e.lastPlannerIntentThisTurn = ""
	e.refreshSystemPrompt()

	// Trim before appending to guarantee the new user message is never dropped.
	e.trimHistory()
	priorText := e.PlannerPriorTextSnapshot()

	// Pre-LLM hard-block chain — runs on raw userMsg only, BEFORE OCR
	// image context is prepended. This prevents screenshot UI labels
	// (e.g. "运维监控", "最近访问") from triggering false-positive blocks.
	if decision := enginePreBlock.Decide(userMsg); decision.Matched {
		e.pendingResourceSelection = nil
		if e.hardBlockObserver != nil {
			e.hardBlockObserver(observability.EngineHardBlockTrace{
				Hit:         true,
				Category:    decision.Category,
				TriggeredBy: observability.HardBlockTriggerKeyword,
			})
		}
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: userMsg,
		})
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: decision.Reply,
		})
		return decision.Reply, nil
	}

	// Build LLM-facing message: raw userMsg + optional image context.
	// userMsg stays immutable for all keyword/regex routing below;
	// llmUserMsg carries image evidence into conversation history so the
	// ReAct LLM can reference it.
	llmUserMsg := userMsg
	if opts.ImageContext != "" {
		llmUserMsg = "用户上传了一张截图，系统自动识别到以下内容：\n" + opts.ImageContext + "\n\n" + userMsg
	}

	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: llmUserMsg,
	})

	e.currentMonitorTargets = nil
	e.currentMonitorNoData = nil
	e.currentMonitorStart = 0
	e.currentMonitorEnd = 0
	e.currentMonitorWindow = false

	if reply, handled := e.tryResumeResourceSelection(ctx, userMsg, onStep); handled {
		return reply, nil
	}

	forceMonitorRecall := e.shouldForceMonitorRecall(userMsg)
	if reply, handled := e.tryPlannerDispatch(ctx, userMsg, priorText, onStep, opts.OnTextDelta); handled {
		return reply, nil
	}

	for round := 0; round < maxReActRounds; round++ {
		// Per-turn token budget gate. Placed at the TOP of the loop so
		// any tool_call → tool_result pair emitted in the previous
		// iteration has already completed and been appended to history
		// before we stop. This preserves the WS protocol invariant that
		// every tool_call is followed by a tool_result on the wire —
		// breaking mid-pair would leave the client with an orphan
		// tool_call frame. First iteration (round 0) sees consumed
		// pre-loaded with any planner LLM call (accumulateTokenUsage
		// in callPlannerOnce) and triggers if that already blew budget.
		if e.tokenBudgetExceeded() {
			e.emitTokenBudgetExceededHardBlock()
			e.messages = append(e.messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: tokenBudgetExceededMessage,
			})
			return tokenBudgetExceededMessage, nil
		}
		req := llm.ChatRequest{
			Messages: e.buildMessagesForLLM(),
			Tools:    tools.VisibleRegistryForSubset(intent.IntentToolSubset(e.lastPlannerIntentThisTurn), e.mutatingToolsEnabled),
		}
		// BRIDGE T-001.f1: adjacent monitor follow-up must re-call
		// GetCompShareInstanceMonitor instead of reusing prior numbers.
		// Scope: first LLM call of this turn only. Capability-gated:
		// models without object tool_choice support (e.g. deepseek-v4-flash
		// in thinking mode) fall through to LLM auto routing instead of
		// 400ing on a forced ToolChoice. Stale-reuse is then unmitigated
		// on those models — see eval/capability/2026-05-08-ds-v4-flash-
		// tool-choice-probe.md and the pending monitor stale-reuse probe.
		if round == 0 && forceMonitorRecall && e.supportsObjectToolChoice {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "GetCompShareInstanceMonitor"},
			}
		}
		if decision, ok := e.allowRateLimited(governance.ClassLLM, "main_react_chat"); !ok {
			content := rateLimitMessage(decision.Reason)
			e.messages = append(e.messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: content,
			})
			return content, nil
		}
		// Stream text deltas live to opts.OnTextDelta unless a downstream
		// guard might rewrite the final content this round. When a guard could
		// fire, buffer per-round so we can either replay the raw deltas (when
		// content == rawContent) or emit the override as a single chunk.
		// Intermediate tool-call rounds emit no content deltas in practice, so
		// live mode does not leak partial tool args.
		guardMayRewrite := e.currentMonitorWindow ||
			(round == 0 && e.requireKnowledgeCitationThisTurn && e.knowledgeRetriever != nil)
		liveStream := opts.OnTextDelta != nil && !guardMayRewrite
		var streamedDeltas []string
		if liveStream {
			req.OnTextDelta = opts.OnTextDelta
		} else if opts.OnTextDelta != nil {
			req.OnTextDelta = func(s string) {
				streamedDeltas = append(streamedDeltas, s)
			}
		}
		resp, err := e.llmClient.Chat(ctx, req)
		if err != nil {
			return "", fmt.Errorf("LLM 调用失败: %w", err)
		}

		e.emitTokenUsage(resp.Usage)
		if opts.OnUsage != nil {
			opts.OnUsage(resp.Usage)
		}

		// Post-call budget check: emitTokenUsage just accumulated this
		// call's usage. If the single call already blew the cap, gate
		// here so the user gets the canned reply instead of an answer
		// that already exceeded budget. Without this, a one-shot 60k-
		// token final answer would still flow through to the user
		// despite max_tokens_per_turn=50000. Returning canned makes the
		// cap meaningful end-to-end. (c) invariant still holds: this
		// branch has NO tool_calls in flight — len(resp.ToolCalls)==0
		// is the condition just below — so no orphan pair.
		if len(resp.ToolCalls) == 0 && e.tokenBudgetExceeded() {
			e.emitTokenBudgetExceededHardBlock()
			e.messages = append(e.messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: tokenBudgetExceededMessage,
			})
			return tokenBudgetExceededMessage, nil
		}

		// No tool calls → final text reply
		if len(resp.ToolCalls) == 0 {
			rawContent := resp.Content
			content := e.guardMonitorTemporalFinalReply(rawContent)
			// PR-RAG-PLANNER-INTENT-AUDIT (2026-05-17): cited contract invariant.
			// Keep the hard gate for planner-classified knowledge questions that
			// fall back to a pure LLM answer, but do not apply it to diagnosis,
			// price/tool, or other non-knowledge intents.
			if round == 0 && e.requireKnowledgeCitationThisTurn && e.knowledgeRetriever != nil &&
				!isKnowledgeRefusal(content) && !hasNumberedCitation(content) {
				if e.hardBlockObserver != nil {
					e.hardBlockObserver(observability.EngineHardBlockTrace{
						Hit:         true,
						Category:    "cited_contract_violation",
						TriggeredBy: observability.HardBlockTriggerPostLLM,
					})
				}
				content = ragNoEvidenceReply
			}
			// Replay buffered streaming deltas when the LLM content was returned
			// verbatim. If an engine guard overwrote content, emit the canonical
			// override as a single chunk so the SSE stream matches the persisted
			// final reply — do not replay stale raw deltas in that case.
			// liveStream rounds have already streamed deltas as they arrived;
			// nothing to replay.
			if opts.OnTextDelta != nil && !liveStream {
				if content == rawContent {
					for _, delta := range streamedDeltas {
						opts.OnTextDelta(delta)
					}
				} else {
					opts.OnTextDelta(content)
				}
			} else if opts.OnTextDelta != nil && liveStream && content != rawContent {
				// Live mode reached a guard rewrite (state changed mid-round,
				// e.g. currentMonitorWindow flipped). Emit a final corrective
				// chunk with the rewritten tail. Rare in practice; the
				// guardMayRewrite predicate is meant to keep us out of this
				// branch entirely.
				opts.OnTextDelta(content)
			}
			e.messages = append(e.messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: content,
			})
			return content, nil
		}

		// Has tool calls → execute each and feed results back
		assistantMsg := openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		e.messages = append(e.messages, assistantMsg)

		for idx, tc := range resp.ToolCalls {
			toolResult := e.executeTool(ctx, tc, onStep)

			// Deterministic final reply — return directly without LLM narration
			if finalMsg, ok := isFinalReply(toolResult); ok {
				// Append matching tool response for this tool call
				e.messages = append(e.messages, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					Content:    finalMsg,
					ToolCallID: tc.ID,
				})
				// Pad remaining unprocessed tool calls with synthetic responses
				// to keep the history well-formed (every tool_call needs a tool response)
				for _, remaining := range resp.ToolCalls[idx+1:] {
					e.messages = append(e.messages, openai.ChatCompletionMessage{
						Role:       openai.ChatMessageRoleTool,
						Content:    "skipped",
						ToolCallID: remaining.ID,
					})
				}
				// Append the final assistant message
				e.messages = append(e.messages, openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleAssistant,
					Content: finalMsg,
				})
				return finalMsg, nil
			}

			e.messages = append(e.messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    toolResult,
				ToolCallID: tc.ID,
			})
		}
	}

	return "抱歉，处理轮次超限，请重新描述您的需求。", nil
}

type plannerDispatchResult struct {
	result   intent.PlannerResult
	latency  time.Duration
	snapshot entity.RegistrySnapshot
}

func (e *Engine) tryPlannerDispatch(ctx context.Context, userMsg, priorText string, onStep func(StepEvent), onTextDelta func(string)) (string, bool) {
	if !e.plannerDispatchEnabled() {
		return "", false
	}

	dispatch := e.callPlannerOnce(ctx, userMsg, priorText)
	if status, ok := e.commonPlannerCandidateStatus(dispatch.result); !ok {
		if dispatch.result.Plan.Intent == intent.IntentKnowledgeQA {
			e.requireKnowledgeCitationThisTurn = true
		}
		e.lastPlannerIntentThisTurn = dispatch.result.Plan.Intent
		e.emitPlannerTrace(dispatch.result, status, dispatch.latency)
		return "", false
	}

	// Record the planner intent for all subsequent branches. If any branch
	// falls back to ReAct (return "", false), the ReAct loop uses this to
	// scope the tool list via intent.IntentToolSubset.
	e.lastPlannerIntentThisTurn = dispatch.result.Plan.Intent

	// Token budget gate. callPlannerOnce already added planner usage to
	// the per-turn counter; if that alone blew the cap, return the
	// canned reply BEFORE any further LLM call (cutover handler,
	// answerWithRetrievedEvidence, grounded renderer). Without this
	// every planner-handled path could spend an extra answerer call's
	// worth of tokens past the cap — the C1 finding from 2026-05-21
	// review. Returning handled=true short-circuits Chat() so it does
	// NOT fall through to the ReAct loop (which would re-trip the gate
	// but waste a frame). No tool_call/tool_result pair is in flight
	// here (planner-handled paths don't emit ReAct tool events), so
	// the (c) protocol invariant is naturally satisfied.
	if e.tokenBudgetExceeded() {
		e.emitTokenBudgetExceededHardBlock()
		e.emitPlannerTrace(dispatch.result, intent.CutoverStatusFallbackIneligible, dispatch.latency)
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: tokenBudgetExceededMessage,
		})
		return tokenBudgetExceededMessage, true
	}

	if dispatch.result.Plan.Intent == intent.IntentMonitorHistory {
		// Code-level guardrail: when image context is present, the planner
		// may misclassify screenshot UI labels ("运维监控", "最近访问") as
		// a historical monitor question. Only honor the refusal if the raw
		// user message independently satisfies the keyword pattern.
		if e.imageContextThisTurn != "" && !isUnsupportedHistoricalMonitorQuestion(userMsg) {
			e.emitPlannerTrace(dispatch.result, intent.CutoverStatusFallbackIneligible, dispatch.latency)
			return "", false
		}
		e.emitPlannerTrace(dispatch.result, intent.CutoverStatusFallbackTimeWindow, dispatch.latency)
		return e.emitMonitorHistoryHardBlock(), true
	}
	if reply, handled := e.tryPlannerDiagnosisClarification(dispatch); handled {
		return reply, true
	}
	if dispatch.result.Plan.Intent == intent.IntentResourceInfo || dispatch.result.Plan.Intent == intent.IntentMonitorQuery || intent.IsCapabilityIntent(dispatch.result.Plan.Intent) {
		return e.tryPhase1Cutover(ctx, dispatch, userMsg, onStep)
	}
	if reply, handled := e.tryStage2BRetrieval(ctx, dispatch, userMsg, onStep, onTextDelta); handled {
		return reply, true
	}
	if dispatch.result.Plan.Intent == intent.IntentKnowledgeQA {
		e.requireKnowledgeCitationThisTurn = true
		return "", false
	}

	e.emitPlannerTrace(dispatch.result, intent.CutoverStatusFallbackIneligible, dispatch.latency)
	return "", false
}

func (e *Engine) tryPlannerDiagnosisClarification(dispatch plannerDispatchResult) (string, bool) {
	if !e.plannerIntentEnabled(dispatch.result.Plan.Intent) {
		return "", false
	}
	switch dispatch.result.Plan.Intent {
	case intent.IntentVagueFailure:
		reply := diagnosisVagueFailureClarificationReply
		e.emitPlannerTrace(dispatch.result, intent.CutoverStatusFallbackUnresolvedTarget, dispatch.latency)
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: reply,
		})
		return reply, true
	case intent.IntentDiagnosis:
		if len(dispatch.result.Plan.Slots.TargetRefs) > 0 || countPlannerSnapshotInstances(dispatch.snapshot) <= 1 {
			return "", false
		}
		reply := diagnosisMissingTargetClarificationReply
		e.emitPlannerTrace(dispatch.result, intent.CutoverStatusFallbackUnresolvedTarget, dispatch.latency)
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: reply,
		})
		return reply, true
	default:
		return "", false
	}
}

func (e *Engine) plannerDispatchEnabled() bool {
	return e != nil && e.intentPlanner != nil &&
		(len(e.intentPlannerEnabledIntents) > 0 || e.knowledgeRetriever != nil)
}

func (e *Engine) plannerIntentEnabled(intentValue intent.Intent) bool {
	if e == nil || e.intentPlannerEnabledIntents == nil {
		return false
	}
	_, ok := e.intentPlannerEnabledIntents[intentValue]
	return ok
}

func (e *Engine) callPlannerOnce(ctx context.Context, userMsg, priorText string) plannerDispatchResult {
	start := time.Now()
	result := engineFallbackPlannerResult()
	snapshot := e.RegistrySnapshot()
	if _, ok := e.allowRateLimited(governance.ClassLLM, "intent_planner"); ok {
		planned, err := e.intentPlanner.Plan(ctx, intent.PlannerInput{
			UserText:     userMsg,
			ImageContext: e.imageContextThisTurn,
			LastIntent:   e.sessionState.LastIntent,
			PriorText:    priorText,
			Resolver:     snapshot,
		})
		if err == nil {
			result = planned
		}
	} else {
		// Planner quota denial is observable through trace.rate_limit. The
		// cutover status intentionally collapses this into fallback_invalid
		// because trace currently has no dedicated planner-denied enum.
	}
	latency := time.Since(start)

	// Add planner LLM tokens to the per-turn budget. Planner usage is
	// surfaced via PlannerTrace (not emitTokenUsage), so without this
	// accumulation a knowledge-QA turn that resolves entirely through
	// the planner-handled path would never count its planner cost
	// against maxTokensPerTurn — defeating the "total tokens per turn"
	// promise of the cap. Tests:
	// TestChat_TokenBudget_PlannerHandledPath_GateFires.
	e.accumulateTokenUsage(result.Usage)

	return plannerDispatchResult{result: result, latency: latency, snapshot: snapshot}
}

func (e *Engine) tryPhase1Cutover(ctx context.Context, dispatch plannerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	result := dispatch.result
	result.Plan = planWithUserTextMonitorMetrics(result.Plan, userMsg)
	if result.Plan.Intent != intent.IntentResourceInfo && result.Plan.Intent != intent.IntentMonitorQuery && !intent.IsCapabilityIntent(result.Plan.Intent) {
		return "", false
	}
	if status, ok := e.phase1CutoverCandidateStatus(result); !ok {
		e.emitPlannerTrace(result, status, dispatch.latency)
		return "", false
	}

	handler := intent.NewDemoHandler(plannerHandlerExecutor{engine: e, onStep: onStep})
	req := intent.HandlerRequest{
		Plan:     result.Plan,
		Resolver: dispatch.snapshot,
		UserText: userMsg,
	}
	if e.sessionStateHydrated && e.sessionState.SelectedInstanceID != "" {
		req.FallbackInstanceID = e.sessionState.SelectedInstanceID
	}
	var handled intent.HandlerResult
	switch result.Plan.Intent {
	case intent.IntentResourceInfo:
		handled = handler.HandleResourceInfo(ctx, req)
	case intent.IntentMonitorQuery:
		handled = handler.HandleMonitorQuery(ctx, req)
	default:
		// Capability Registry v1: any registered capability intent dispatches
		// through the registry. Engine.go does not need per-case wiring as new
		// capabilities are added — see internal/intent/capability_registry.go.
		if intent.IsCapabilityIntent(result.Plan.Intent) {
			handled = handler.DispatchCapability(ctx, req)
		} else {
			e.emitPlannerTrace(result, intent.CutoverStatusFallbackIneligible, dispatch.latency)
			return "", false
		}
	}

	if handled.Status == intent.HandlerStatusFallbackBeforeTool {
		if isResourceSelectionFallbackReason(handled.FallbackReason) {
			if selection, ok, err := e.buildResourceSelectionForPlan(ctx, result, dispatch.snapshot, onStep); err != nil {
				reply := intent.FriendlyToolFailureReply
				if msg, friendly := friendlyToolErrorMessage(err); friendly {
					reply = msg
				}
				e.pendingResourceSelection = nil
				e.emitPlannerTrace(result, intent.CutoverStatusFailureAfterTool, dispatch.latency)
				e.messages = append(e.messages, openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleAssistant,
					Content: reply,
				})
				return reply, true
			} else if ok {
				if len(selection.candidates) == 1 {
					resumed := result
					resumed.Plan = planWithSelectedResource(result.Plan, selection.candidates[0].UHostId)
					req := intent.HandlerRequest{
						Plan:     resumed.Plan,
						Resolver: selection.snapshot,
						UserText: userMsg,
					}
					handled = handler.HandleMonitorQuery(ctx, req)
					e.emitPlannerTrace(resumed, handled.CutoverStatus, dispatch.latency)
					e.annotateHandlerResultForUserQuestion(&handled, resumed.Plan, e.lastUserMsg)
					reply := handled.Reply
					if handled.Status == intent.HandlerStatusHandled {
						reply = e.renderGroundedHandlerResult(ctx, handled)
						e.recordSelectedInstanceFromEnvelope(handled.Envelope)
						e.recordLastIntentFromPlan(resumed.Plan)
					}
					e.messages = append(e.messages, openai.ChatCompletionMessage{
						Role:    openai.ChatMessageRoleAssistant,
						Content: reply,
					})
					return reply, true
				}
				e.pendingResourceSelection = selection
				reply := renderResourceSelectionPrompt(*selection)
				e.emitPlannerTrace(result, intent.CutoverStatusSelectionRequired, dispatch.latency)
				e.messages = append(e.messages, openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleAssistant,
					Content: reply,
				})
				return reply, true
			}
		}
		if handled.FallbackReason == intent.FallbackTimeWindow {
			e.emitPlannerTrace(result, handled.CutoverStatus, dispatch.latency)
			return e.emitMonitorHistoryHardBlock(), true
		}
		e.emitPlannerTrace(result, handled.CutoverStatus, dispatch.latency)
		return "", false
	}

	e.emitPlannerTrace(result, handled.CutoverStatus, dispatch.latency)
	e.annotateHandlerResultForUserQuestion(&handled, result.Plan, e.lastUserMsg)
	reply := handled.Reply
	if handled.Status == intent.HandlerStatusHandled {
		reply = e.renderGroundedHandlerResult(ctx, handled)
		e.recordSelectedInstanceFromEnvelope(handled.Envelope)
		e.recordLastIntentFromPlan(result.Plan)
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	})
	return reply, true
}

func (e *Engine) tryResumeResourceSelection(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, bool) {
	pending := e.pendingResourceSelection
	if pending == nil {
		return "", false
	}
	if isResourceSelectionExpired(e.userTurn, *pending) {
		e.pendingResourceSelection = nil
		return "", false
	}

	match := matchResourceSelection(userMsg, *pending)
	if !match.ok {
		if e.userTurn >= pending.createdTurn+2 {
			e.pendingResourceSelection = nil
			return "", false
		}
		pending.invalidAttempts++
		reply := renderResourceSelectionPrompt(*pending)
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: reply,
		})
		return reply, true
	}

	e.pendingResourceSelection = nil
	if pending.plan.Intent != intent.IntentMonitorQuery {
		return "", false
	}

	resumedPlan := planWithSelectedResource(pending.plan, match.instance.UHostId)
	resumedPlan = planWithUserTextMonitorMetrics(resumedPlan, pending.originalUserMsg)
	handler := intent.NewDemoHandler(plannerHandlerExecutor{engine: e, onStep: onStep})
	handled := handler.HandleMonitorQuery(ctx, intent.HandlerRequest{
		Plan:     resumedPlan,
		Resolver: pending.snapshot,
		UserText: pending.originalUserMsg,
	})
	e.emitPlannerTrace(intent.PlannerResult{Plan: resumedPlan}, handled.CutoverStatus, 0)
	e.annotateHandlerResultForUserQuestion(&handled, resumedPlan, pending.originalUserMsg)

	reply := handled.Reply
	if handled.Status == intent.HandlerStatusHandled {
		reply = e.renderGroundedHandlerResult(ctx, handled)
		e.recordSelectedInstanceFromEnvelope(handled.Envelope)
		e.recordLastIntentFromPlan(resumedPlan)
	}
	if reply == "" {
		reply = intent.FriendlyToolFailureReply
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	})
	return reply, true
}

func (e *Engine) renderGroundedHandlerResult(ctx context.Context, handled intent.HandlerResult) string {
	if e.groundedRenderer == nil || handled.Envelope == nil {
		return handled.Reply
	}
	trace := observability.RendererTrace{
		Enabled:             true,
		Status:              "fallback",
		EnvelopeKind:        string(handled.Envelope.Kind),
		InputEnvelopeHashes: append([]string(nil), handled.RendererInputEnvelopeHashes...),
		InputToolArgHashes:  append([]string(nil), handled.RendererInputToolArgHashes...),
		FallbackUsed:        true,
		FallbackReason:      grounded.FallbackRateLimited,
		Model:               e.groundedRendererModel,
		AttributionMode:     grounded.AttributionEnvelope,
	}
	if _, ok := e.allowRateLimited(governance.ClassLLM, "grounded_renderer"); !ok {
		e.emitRendererTrace(trace)
		return handled.Reply
	}
	// Token budget gate before issuing the renderer LLM call. Returning
	// the canned message keeps the contract consistent with the other
	// gate sites: hard_block fires → message_recorder marks the row
	// status="blocked" → user MUST see the budget message, not a
	// normal-looking handled.Reply. Pre-fix this path returned
	// handled.Reply while still firing hard_block, which made the user
	// view ("normal answer") disagree with the DB view ("blocked") —
	// the C3 finding from the user's 2026-05-21 self-review.
	if e.tokenBudgetExceeded() {
		e.emitTokenBudgetExceededHardBlock()
		e.emitRendererTrace(trace)
		return tokenBudgetExceededMessage
	}
	result := e.groundedRenderer.Render(ctx, grounded.RenderRequest{
		Envelope: *handled.Envelope,
		Fallback: handled.Reply,
		Model:    e.groundedRendererModel,
	})
	e.emitTokenUsage(result.Usage)
	status := "fallback"
	if !result.FallbackUsed {
		status = "rendered"
	}
	trace.Status = status
	trace.FallbackUsed = result.FallbackUsed
	trace.FallbackReason = result.FallbackReason
	trace.Model = result.Model
	trace.LatencyMS = result.LatencyMS
	trace.AttributionMode = result.AttributionMode
	if len(trace.InputEnvelopeHashes) == 0 && result.EnvelopeHash != "" {
		trace.InputEnvelopeHashes = []string{result.EnvelopeHash}
	}
	e.emitRendererTrace(trace)
	return result.Text
}

func planWithUserTextMonitorMetrics(plan intent.Plan, userText string) intent.Plan {
	if plan.Intent != intent.IntentMonitorQuery {
		return plan
	}
	lower := strings.ToLower(userText)
	mentionsVRAM := strings.Contains(userText, "显存") ||
		strings.Contains(lower, "vram") ||
		strings.Contains(lower, "gpu memory")
	if !mentionsVRAM {
		return plan
	}
	mentionsGPUUtil := strings.Contains(lower, "gpu") || strings.Contains(userText, "显卡")
	metrics := append([]intent.Metric(nil), plan.Slots.Metrics...)
	if !mentionsGPUUtil {
		metrics = removeMonitorMetric(metrics, intent.MetricGPU)
	}
	plan.Slots.Metrics = appendMonitorMetricIfMissing(metrics, intent.MetricVRAM)
	return plan
}

func appendMonitorMetricIfMissing(metrics []intent.Metric, metric intent.Metric) []intent.Metric {
	for _, existing := range metrics {
		if existing == metric {
			return metrics
		}
	}
	return append(metrics, metric)
}

func removeMonitorMetric(metrics []intent.Metric, metric intent.Metric) []intent.Metric {
	out := metrics[:0]
	for _, existing := range metrics {
		if existing != metric {
			out = append(out, existing)
		}
	}
	return out
}

func (e *Engine) annotateHandlerResultForUserQuestion(result *intent.HandlerResult, plan intent.Plan, userMsg string) {
	if result == nil || result.Envelope == nil || plan.Intent != intent.IntentMonitorQuery {
		return
	}
	if isMonitorTroubleshootingQuestion(userMsg) {
		result.Envelope.Computed = append(result.Envelope.Computed, envelope.Fact{
			Key:    "answer_mode",
			Label:  "Answer mode",
			Value:  "troubleshooting",
			Source: envelope.FactSourceComputed,
		})
		for _, metric := range plan.Slots.Metrics {
			if metric == intent.MetricCPU {
				result.Envelope.Computed = append(result.Envelope.Computed, envelope.Fact{
					Key:    "issue_metric",
					Label:  "Issue metric",
					Value:  "cpu",
					Source: envelope.FactSourceComputed,
				})
				result.Reply = monitorTroubleshootingFallbackReply(result.Reply)
				if hash, err := envelope.Hash(*result.Envelope); err == nil {
					result.RendererInputEnvelopeHashes = []string{hash}
				}
				return
			}
		}
		result.Reply = monitorTroubleshootingFallbackReply(result.Reply)
		if hash, err := envelope.Hash(*result.Envelope); err == nil {
			result.RendererInputEnvelopeHashes = []string{hash}
		}
		return
	}
	if !isMonitorLoadAssessmentQuestion(userMsg) {
		return
	}
	result.Envelope.Computed = append(result.Envelope.Computed, envelope.Fact{
		Key:    "answer_mode",
		Label:  "Answer mode",
		Value:  "load_assessment",
		Source: envelope.FactSourceComputed,
	})
	result.Reply = monitorLoadAssessmentFallbackReply(result.Reply)
	if hash, err := envelope.Hash(*result.Envelope); err == nil {
		result.RendererInputEnvelopeHashes = []string{hash}
	}
}

func monitorTroubleshootingFallbackReply(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = "当前云侧监控没有返回可用指标。"
	}
	return summary + "\n\n当前这一次采样只能说明当前时刻的云侧监控状态，不能排除之前或间歇性的历史波动。建议在控制台查看该实例最近一段时间的对应指标趋势，并同时对照 CPU、内存、GPU 和系统负载等监控指标。"
}

func monitorLoadAssessmentFallbackReply(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "当前云侧监控没有返回可用指标，暂时无法判断这台实例是否忙。"
	}
	if monitorSummaryLooksLowLoad(summary) {
		return "从当前实时采样看，这台实例现在不算忙：" + summary + "。这只代表当前时刻，不能说明过去一段时间是否有过高峰。"
	}
	return "当前实时采样如下：" + summary + "。是否忙需要结合业务预期和历史趋势判断；我目前只能基于当前采样给出判断。"
}

func monitorSummaryLooksLowLoad(summary string) bool {
	parts := strings.FieldsFunc(summary, func(r rune) bool {
		return r == ';' || r == '；' || r == '\n' || r == '|' || r == ','
	})
	seenLoadMetric := false
	for _, part := range parts {
		if !isLoadAssessmentMetric(part) {
			continue
		}
		match := percentValueRE.FindStringSubmatch(part)
		if len(match) < 2 {
			continue
		}
		seenLoadMetric = true
		value, err := strconv.ParseFloat(match[1], 64)
		if err == nil && value > 10 {
			return false
		}
	}
	return seenLoadMetric
}

func isLoadAssessmentMetric(text string) bool {
	normalized := strings.ToLower(text)
	if strings.Contains(normalized, "磁盘") || strings.Contains(normalized, "系统盘") ||
		strings.Contains(normalized, "数据盘") || strings.Contains(normalized, "disk") {
		return false
	}
	return strings.Contains(normalized, "cpu") ||
		strings.Contains(normalized, "gpu") ||
		strings.Contains(normalized, "内存") ||
		strings.Contains(normalized, "显存") ||
		strings.Contains(normalized, "vram") ||
		strings.Contains(normalized, "memory")
}

func isMonitorTroubleshootingQuestion(userMsg string) bool {
	normalized := strings.ToLower(userMsg)
	explicitTroubleshooting := []string{
		"怎么办", "怎么处理", "如何处理", "怎么解决", "如何解决", "排查", "异常",
		"卡顿", "很卡", "太卡", "卡住", "卡死", "无响应", "变慢", "很慢",
	}
	for _, word := range explicitTroubleshooting {
		if strings.Contains(normalized, strings.ToLower(word)) {
			return true
		}
	}
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(normalized)
	cpuIssuePhrases := []string{
		"cpu高", "cpu过高", "cpu太高", "cpu很高", "cpu负载高", "cpu占用高", "cpu使用率高",
		"cpu飙高", "cpu打满", "cpu满了", "highcpu",
	}
	for _, phrase := range cpuIssuePhrases {
		if strings.Contains(compact, phrase) {
			return true
		}
	}
	return false
}

func isMonitorLoadAssessmentQuestion(userMsg string) bool {
	normalized := strings.ToLower(userMsg)
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(normalized)
	phrases := []string{
		"忙不忙", "空闲吗", "空不空闲", "闲置吗", "闲不闲", "负载怎么样", "负载如何",
		"gpu忙吗", "gpu忙不忙", "显卡忙吗", "显卡忙不忙",
	}
	for _, phrase := range phrases {
		if strings.Contains(compact, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

func (e *Engine) tryStage2BRetrieval(ctx context.Context, dispatch plannerDispatchResult, userMsg string, onStep func(StepEvent), onTextDelta func(string)) (string, bool) {
	result := dispatch.result
	if result.Plan.Intent != intent.IntentKnowledgeQA {
		return "", false
	}
	if e.knowledgeRetriever == nil {
		e.emitRetrievalTrace(observability.RetrievalTrace{})
		e.emitPlannerTrace(result, intent.CutoverStatusFallbackRetrievalDisabled, dispatch.latency)
		return "", false
	}

	onStep(StepEvent{Type: StepToolCall, Action: "SearchKnowledge", Source: "retrieval", Message: "正在搜索知识库"})
	retrieved := e.knowledgeRetriever.Retrieve(userMsg, inferKnowledgeProductArea(userMsg))
	hitItems := retrieved.HitItems
	trace := observability.RetrievalTrace{
		Enabled:                retrieved.Enabled,
		KBVersion:              retrieved.KBVersion,
		QueryRaw:               userMsg,
		QueryNormalized:        retrieved.QueryNormalized,
		QueryExpansions:        []string{},
		Hits:                   len(retrieved.Hits),
		HybridMode:             retrieved.HybridMode,
		HybridFallbackReason:   retrieved.HybridFallbackReason,
		EmbeddingLatencyMS:     retrieved.EmbeddingLatencyMS,
		EmbeddingModel:         retrieved.EmbeddingModel,
		RerankerMode:           retrieved.RerankerMode,
		RerankerLatencyMS:      retrieved.RerankerLatencyMS,
		RerankerFallbackReason: retrieved.RerankerFallbackReason,
	}
	if trace.QueryNormalized == "" {
		trace.QueryNormalized = knowledge.NormalizeQuery(userMsg)
	}
	evidences, evidenceErr := evidencesFromRetrievalHits(hitItems, trace.QueryNormalized)
	trace.HitItems = projectEvidenceTraceHits(evidences, hitItems)
	onStep(StepEvent{Type: StepToolResult, Action: "SearchKnowledge", Source: "retrieval", Message: "搜索完成"})
	if retrieved.Empty || len(retrieved.Hits) == 0 || len(evidences) == 0 || evidenceErr != nil {
		trace.RefusedReason = "no_evidence"
		trace.RankingErrorCandidate = true
		e.emitRetrievalTrace(trace)
		e.emitPlannerTrace(result, intent.CutoverStatusFallbackRetrievalMiss, dispatch.latency)
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: ragNoEvidenceReply,
		})
		return ragNoEvidenceReply, true
	}

	weak := isWeakEvidence(hitItems, retrieved.HybridMode)
	if weak {
		trace.WeakEvidence = true
	}
	if weak || isRankingAmbiguous(hitItems, retrieved.HybridMode) {
		trace.RankingErrorCandidate = true
	}
	// Buffer LLM deltas so we can decide whether to replay them after
	// post-processing. answerWithRetrievedEvidence may discard the LLM
	// output (token budget, refusal, retry-no-cite) and return a canned
	// string instead. Replaying raw deltas in those cases would leave the
	// SSE stream inconsistent with done.Content.
	var bufferedDeltas []string
	var bufferDelta func(string)
	if onTextDelta != nil {
		bufferDelta = func(s string) { bufferedDeltas = append(bufferedDeltas, s) }
	}
	reply, outcome, refusedReason, rankingCandidate, err := e.answerWithRetrievedEvidence(ctx, userMsg, evidences, weak, bufferDelta)
	if err != nil {
		trace.RefusedReason = "llm_error"
		trace.RankingErrorCandidate = true
		e.emitRetrievalTrace(trace)
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: ragNoEvidenceReply,
		})
		return ragNoEvidenceReply, true
	}
	if refusedReason != "" {
		trace.RefusedReason = refusedReason
	}
	if rankingCandidate {
		trace.RankingErrorCandidate = true
	}
	trace.CitedChunkIDs = extractCitedChunkIDs(reply, hitItems)
	displayReply := stripCitationMarkers(reply)

	// Replay buffered deltas only when the LLM's first-call output was
	// accepted (reply == resp.Content path). Refusal / budget / retry
	// paths return a different string, so we skip replay and let the
	// handler's done.Content carry the final text instead.
	if onTextDelta != nil && len(bufferedDeltas) > 0 && refusedReason == "" {
		for _, d := range bufferedDeltas {
			onTextDelta(d)
		}
	}

	e.emitRetrievalTrace(trace)
	e.emitOutcomeTrace(outcome)
	e.emitPlannerTrace(result, intent.CutoverStatusDispatchedRetrieval, dispatch.latency)
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: clipKnowledgeHistoryContent(displayReply),
	})
	return displayReply, true
}

func (e *Engine) answerWithRetrievedEvidence(ctx context.Context, userMsg string, evidences []envelope.Evidence, weak bool, onTextDelta func(string)) (string, observability.OutcomeTrace, string, bool, error) {
	outcome := observability.OutcomeTrace{}
	req := llm.ChatRequest{
		Messages:    prompt.BuildRAGMessages(userMsg, ragReferencesFromEvidence(evidences), weak, false),
		OnTextDelta: onTextDelta,
	}
	resp, err := e.llmClient.Chat(ctx, req)
	if err != nil {
		return "", outcome, "", false, fmt.Errorf("LLM 调用失败: %w", err)
	}
	e.emitTokenUsage(resp.Usage)

	// Post-call budget gate. The first answerer LLM call has been
	// accounted for; if cumulative tokens are over cap, return the
	// canned message instead of the LLM content. WHY: pre-fix, an
	// answer that itself blew budget was still delivered to the user
	// — the cap stopped only FURTHER calls. That made the cap
	// non-strict on RAG paths. EscapedHallucinatedCount stays 0 here
	// (no hallucination was attempted — there's no answer being
	// scored), distinct from organic retry_no_cite which sets it to 1.
	if e.tokenBudgetExceeded() {
		e.emitTokenBudgetExceededHardBlock()
		return tokenBudgetExceededMessage, outcome, "token_budget", false, nil
	}

	answer := strings.TrimSpace(resp.Content)
	if isKnowledgeRefusal(answer) {
		return answer, outcome, refusedReasonForRefusal(weak), false, nil
	}
	if hasNumberedCitation(answer) {
		return answer, outcome, "", false, nil
	}

	outcome.AttemptedHallucinatedCount = 1
	retryReq := llm.ChatRequest{
		Messages: prompt.BuildRAGMessages(userMsg, ragReferencesFromEvidence(evidences), weak, true),
	}
	retryResp, err := e.llmClient.Chat(ctx, retryReq)
	if err != nil {
		return "", outcome, "", false, fmt.Errorf("LLM 调用失败: %w", err)
	}
	e.emitTokenUsage(retryResp.Usage)

	// Post-retry budget gate. Symmetric to the post-first-call gate
	// above. If the retry pushed us over cap, deliver canned instead
	// of the retry's answer — even if the retry produced a cited
	// answer, the cap is binding.
	if e.tokenBudgetExceeded() {
		e.emitTokenBudgetExceededHardBlock()
		return tokenBudgetExceededMessage, outcome, "token_budget", false, nil
	}
	retryAnswer := strings.TrimSpace(retryResp.Content)
	if isKnowledgeRefusal(retryAnswer) {
		return retryAnswer, outcome, refusedReasonForRefusal(weak), false, nil
	}
	if hasNumberedCitation(retryAnswer) {
		return retryAnswer, outcome, "", false, nil
	}
	outcome.EscapedHallucinatedCount = 1
	return ragNoEvidenceReply, outcome, "retry_no_cite", true, nil
}

func refusedReasonForRefusal(weak bool) string {
	if weak {
		return "weak_evidence"
	}
	return "refusal"
}

func evidencesFromRetrievalHits(items []knowledge.RetrievalHit, queryNormalized string) ([]envelope.Evidence, error) {
	evidences := make([]envelope.Evidence, 0, len(items))
	producedAt := time.Now().UTC()
	for _, item := range items {
		score := item.Score
		var surfaceURL *string
		if strings.TrimSpace(item.Chunk.SourceURL) != "" {
			url := strings.TrimSpace(item.Chunk.SourceURL)
			surfaceURL = &url
		}
		evidence, err := envelope.NewEvidence(envelope.EvidenceInput{
			SourceTitle:     item.Chunk.Title,
			Snippet:         item.Chunk.Content,
			SurfaceURL:      surfaceURL,
			EvidenceKind:    envelope.EvidenceKindKnowledge,
			ChunkID:         item.Chunk.ChunkID,
			KBVersion:       item.Chunk.KBVersion,
			RetrievalScore:  &score,
			QueryNormalized: queryNormalized,
			ProducedAt:      producedAt,
		})
		if err != nil {
			return nil, err
		}
		evidences = append(evidences, evidence)
	}
	return evidences, nil
}

func projectEvidenceTraceHits(evidences []envelope.Evidence, items []knowledge.RetrievalHit) []observability.RetrievalHit {
	hits := make([]observability.RetrievalHit, 0, len(evidences))
	for index, evidence := range evidences {
		view := evidence.ForTrace()
		kept := true
		var item knowledge.RetrievalHit
		if index < len(items) {
			item = items[index]
			kept = item.Kept
		}
		hits = append(hits, observability.RetrievalHit{
			ChunkID: view.ChunkID,
			Score:   view.RetrievalScore,
			Kept:    kept,
			// RRF trace fields. Zero values omitted via json omitempty
			// for non-qwen3_rrf modes; populated when knowledge.Retriever
			// ran the qwen3_rrf branch.
			BM25Rank:    item.BM25Rank,
			DenseRank:   item.DenseRank,
			FusionRank:  item.FusionRank,
			FusionScore: item.FusionScore,
		})
	}
	return hits
}

func ragReferencesFromEvidence(evidences []envelope.Evidence) []prompt.RAGReference {
	refs := make([]prompt.RAGReference, 0, len(evidences))
	for i, evidence := range evidences {
		view := evidence.ForLLM()
		refs = append(refs, prompt.RAGReference{
			Number:  i + 1,
			Title:   view.SourceTitle,
			Content: view.Snippet,
		})
	}
	return refs
}

// isWeakEvidence reports whether the top hit's score is below the weak-evidence
// threshold for the retrieval path that produced it. hybridMode comes from
// knowledge.RetrievalResult.HybridMode and tracks the actual scoring path used
// (including bm25_fallback when a hybrid mode degraded to BM25 mid-flight),
// not the user-configured RAG_RETRIEVAL_MODE. Treat unknown / empty values as
// BM25 — that preserves pre-mode-aware test fixtures whose mock RetrievalResult
// leaves HybridMode unset.
func isWeakEvidence(items []knowledge.RetrievalHit, hybridMode string) bool {
	if len(items) == 0 {
		return false
	}
	return items[0].Score < weakEvidenceThresholdFor(hybridMode)
}

// isRankingAmbiguous reports whether the top two hits are close enough on the
// scoring scale that ranking is essentially a tie. Only feeds telemetry
// (trace.RankingErrorCandidate); does NOT influence the RAG prompt or refusal
// path. Mode-aware so the spread threshold matches the score scale in use.
func isRankingAmbiguous(items []knowledge.RetrievalHit, hybridMode string) bool {
	if len(items) < 2 {
		return false
	}
	return items[0].Score-items[1].Score < rankingAmbiguousSpreadFor(hybridMode)
}

// weakEvidenceThresholdFor maps a knowledge.RetrievalResult.HybridMode value to
// the appropriate weak-evidence floor. See cited_guard.go for the rationale
// behind each scale. The empty string and any unrecognized value default to
// the BM25 threshold so existing tests with mock RetrievalResult{} keep their
// fixture-pinned behavior.
func weakEvidenceThresholdFor(hybridMode string) float64 {
	switch hybridMode {
	case "hybrid_cosine", "hybrid_rerank", "qwen3_full", "qwen3_rrf":
		// qwen3_rrf's final Score is qwen3-reranker-8b relevance score
		// (same reranker as qwen3_full), so same [0,1] semantic threshold
		// applies. Without this case the default branch would pick the
		// BM25 threshold (designed for 0..N BM25 raw scores) and
		// false-refuse on perfectly cited cross-encoder evidence.
		return weakEvidenceSemanticThreshold
	default:
		// "bm25_only", "bm25_fallback", "", or any unrecognized value.
		return weakEvidenceBM25Threshold
	}
}

// rankingAmbiguousSpreadFor maps HybridMode to the spread threshold under which
// the top two hits are considered tied. Same default-to-BM25 rule as above.
func rankingAmbiguousSpreadFor(hybridMode string) float64 {
	switch hybridMode {
	case "hybrid_cosine", "hybrid_rerank", "qwen3_full", "qwen3_rrf":
		return rankingAmbiguousSemanticSpread
	default:
		return rankingAmbiguousBM25Spread
	}
}

func clipKnowledgeHistoryContent(content string) string {
	runes := []rune(content)
	if len(runes) <= maxKnowledgeHistoryRunes {
		return content
	}
	return string(runes[:maxKnowledgeHistoryRunes]) + knowledgeHistoryClipMarker
}

func (e *Engine) commonPlannerCandidateStatus(result intent.PlannerResult) (intent.CutoverStatus, bool) {
	if result.Fallback || result.LastValidationCode != "" ||
		result.Plan.SchemaVersion != intent.SchemaVersion || result.Plan.Intent == "" {
		return intent.CutoverStatusFallbackInvalid, false
	}
	// PR #61 (2026-05-21): planner's HardBlockHint is advisory only and no
	// longer participates in cutover routing — it ships to trace via
	// PlannerTrace.HardBlockHint for downstream join with engine_hard_block
	// (observability). Deterministic refusal comes from the keyword
	// PreBlock (router.go) and the planner-classified IntentMonitorHistory
	// path (emitMonitorHistoryHardBlock), both of which run AFTER this
	// candidate-status check.
	if result.Plan.Confidence < 0.60 {
		return intent.CutoverStatusFallbackLowConfidence, false
	}
	return intent.CutoverStatusDispatched, true
}

func (e *Engine) phase1CutoverCandidateStatus(result intent.PlannerResult) (intent.CutoverStatus, bool) {
	if result.Plan.Retrieval.Enabled {
		return intent.CutoverStatusFallbackIneligible, false
	}
	if result.Plan.Intent != intent.IntentResourceInfo && result.Plan.Intent != intent.IntentMonitorQuery && !intent.IsCapabilityIntent(result.Plan.Intent) {
		return intent.CutoverStatusFallbackIneligible, false
	}
	if _, ok := e.intentCutoverIntents[result.Plan.Intent]; !ok {
		return intent.CutoverStatusFallbackIneligible, false
	}
	return intent.CutoverStatusDispatched, true
}

const (
	diagnosisMissingTargetClarificationReply = "请问是哪台实例出了问题？请提供实例 ID 或实例名称后我再继续排查。"
	diagnosisVagueFailureClarificationReply  = "请问是哪台实例出了问题？也请描述一下具体是什么现象，例如 SSH 断了、GPU 报错、服务崩了或初始化卡住。"
)

func countPlannerSnapshotInstances(snapshot entity.RegistrySnapshot) int {
	if snapshot.TotalCount > 0 {
		return snapshot.TotalCount
	}
	return len(snapshot.Instances)
}

func (e *Engine) emitPlannerTrace(result intent.PlannerResult, status intent.CutoverStatus, latency time.Duration) {
	if e.plannerTraceObserver == nil {
		return
	}
	trace := intent.ProjectPlannerTrace(result, intent.PlannerTraceOptions{
		Enabled: true,
		Model:   e.intentPlannerModel,
		Latency: latency,
	})
	trace.CutoverStatus = string(status)
	e.plannerTraceObserver(trace)
}

func (e *Engine) emitRetrievalTrace(trace observability.RetrievalTrace) {
	if e.retrievalTraceObserver == nil {
		return
	}
	e.retrievalTraceObserver(trace)
}

func (e *Engine) emitOutcomeTrace(trace observability.OutcomeTrace) {
	if e.outcomeTraceObserver == nil || !traceOutcomeObserved(trace) {
		return
	}
	e.outcomeTraceObserver(trace)
}

func traceOutcomeObserved(trace observability.OutcomeTrace) bool {
	return trace.AttemptedHallucinatedCount != 0 ||
		trace.EscapedHallucinatedCount != 0 ||
		trace.KBConflictCount != 0
}

func (e *Engine) emitTokenUsage(usage llm.TokenUsage) {
	total := tokenUsageTotal(usage)
	if total > 0 {
		// Track regardless of observer wiring so the per-turn budget
		// check sees every LLM call's usage, not just turns that happen
		// to have an observer attached. Planner LLM calls are not
		// routed through emitTokenUsage (they're observed via
		// emitPlannerTrace) and add to the same counter via
		// accumulateTokenUsage below.
		e.turnTokensConsumed += total
	}
	if e.tokenUsageObserver == nil || total == 0 {
		return
	}
	e.tokenUsageObserver(usage)
}

// accumulateTokenUsage adds usage to the per-turn budget counter without
// going through the observer. Used for LLM calls (notably the planner)
// whose usage is surfaced via a different trace path but still needs to
// count against maxTokensPerTurn — otherwise a planner-handled turn
// could bypass the cap entirely.
func (e *Engine) accumulateTokenUsage(usage llm.TokenUsage) {
	total := tokenUsageTotal(usage)
	if total > 0 {
		e.turnTokensConsumed += total
	}
}

// tokenBudgetExceeded reports whether this turn has already consumed
// maxTokensPerTurn or more LLM tokens. Read-only — call emitTokenBudget
// ExceededHardBlock + append the canned assistant reply when this trips.
func (e *Engine) tokenBudgetExceeded() bool {
	return e.maxTokensPerTurn > 0 && e.turnTokensConsumed >= e.maxTokensPerTurn
}

// emitTokenBudgetExceededHardBlock fires the trace observer for a turn
// that ran over budget. Separate from message-append so each call site
// can keep its own assistant-message conventions (cutover handlers
// already manage their history slot; the ReAct loop appends inline).
func (e *Engine) emitTokenBudgetExceededHardBlock() {
	if e.hardBlockObserver != nil {
		e.hardBlockObserver(observability.EngineHardBlockTrace{
			Hit:         true,
			Category:    "token_budget_exceeded",
			TriggeredBy: observability.HardBlockTriggerTokenBudget,
		})
	}
}

func tokenUsageTotal(usage llm.TokenUsage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.PromptTokens + usage.CompletionTokens
}

func (e *Engine) emitRendererTrace(trace observability.RendererTrace) {
	if e.rendererTraceObserver == nil {
		return
	}
	e.rendererTraceObserver(trace)
}

func engineFallbackPlannerResult() intent.PlannerResult {
	return intent.PlannerResult{
		Fallback: true,
		Plan: intent.Plan{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Retrieval:     intent.Retrieval{Enabled: false},
		},
	}
}

type plannerHandlerExecutor struct {
	engine *Engine
	onStep func(StepEvent)
}

func (x plannerHandlerExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if x.engine == nil {
		return nil, fmt.Errorf("planner handler engine is nil")
	}
	result, err := x.engine.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: tools.OriginDirectLLM,
		Hooks: tools.SafeToolHooks{
			OnConfirmNeeded: func(action string, args map[string]any) {
				x.emit(StepEvent{Type: StepConfirmNeeded, Action: action, Source: observability.ToolSourcePlannerHandler, Args: x.engine.safeExecutor.RedactArgs(action, args), Message: "此操作需要您确认"})
			},
			OnBeforeCall: func(action string, args map[string]any) {
				x.emit(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourcePlannerHandler, Args: x.engine.safeExecutor.RedactArgs(action, args)})
			},
		},
	})
	if err != nil {
		if msg, ok := friendlyToolErrorMessage(err); ok {
			x.emit(blockedStepEvent(action, observability.ToolSourcePlannerHandler, x.engine.safeExecutor.RedactArgs(action, args), msg, err))
			return nil, friendlyEngineError{cause: err, message: msg}
		}
		x.emit(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourcePlannerHandler, Message: fmt.Sprintf("API 调用失败: %v", err)})
		return nil, err
	}
	event := StepEvent{
		Type:        StepToolResult,
		Action:      action,
		Source:      observability.ToolSourcePlannerHandler,
		Message:     "调用成功",
		TraceResult: result.TraceResult,
		Attempts:    result.Attempts,
	}
	if action == "GetCompShareInstanceMonitor" {
		event.RendererInputToolArgHashes = hashPlannerHandlerArgs(args)
	}
	x.emit(event)
	return result.RawResult, nil
}

func (x plannerHandlerExecutor) emit(ev StepEvent) {
	if x.onStep != nil {
		x.onStep(ev)
	}
}

func hashPlannerHandlerArgs(args map[string]any) []string {
	hash, err := observability.HashTracePayload(args)
	if err != nil {
		return nil
	}
	return []string{hash}
}

func (e *Engine) allowRateLimited(class governance.Class, action string) (governance.Decision, bool) {
	if e.rateLimiter == nil {
		return governance.Decision{Allowed: true, Class: class, Action: action}, true
	}
	subject := e.rateLimitSubject
	if subject == "" {
		subject = governance.AnonymousSubjectKey
	}
	decision := e.rateLimiter.Allow(governance.Request{
		SubjectKey: subject,
		Class:      class,
		Action:     action,
		Now:        time.Now(),
	})
	if e.rateLimitObserver != nil {
		e.rateLimitObserver(decision)
	}
	return decision, decision.Allowed
}

func rateLimitMessage(reason governance.Reason) string {
	if reason == governance.ReasonDailyExceeded {
		return rateLimitDailyMessage
	}
	return rateLimitQPSMessage
}

type friendlyEngineError struct {
	cause   error
	message string
}

func (e friendlyEngineError) Error() string {
	return e.message
}

func (e friendlyEngineError) Unwrap() error {
	return e.cause
}

func (e friendlyEngineError) UserMessage() string {
	return e.message
}

var friendlyActionNames = map[string]string{
	"CreateInstanceWorkflow":      "创建实例",
	"StopInstanceWorkflow":        "关机",
	"StartInstanceWorkflow":       "开机",
	"RebootInstanceWorkflow":      "重启",
	"RenameInstanceWorkflow":      "重命名",
	"ResetPasswordWorkflow":       "重置密码",
	"SetStopSchedulerWorkflow":    "设置定时关机",
	"CancelStopSchedulerWorkflow": "取消定时关机",
	"ResizeInstanceWorkflow":      "变配",
	"ReinstallInstanceWorkflow":   "重装系统",
	"CreateDiskWorkflow":          "创建数据盘",
}

func friendlyActionName(action string) string {
	if name, ok := friendlyActionNames[action]; ok {
		return name
	}
	return action
}

func friendlyToolErrorMessage(err error) (string, bool) {
	var friendly friendlyEngineError
	if errors.As(err, &friendly) {
		return friendly.message, true
	}
	switch {
	case errors.Is(err, tools.ErrHistoricalMonitorUnsupported):
		return refusal.MonitorHistoryUnsupported, true
	case errors.Is(err, tools.ErrHistoryWindowExceeded):
		return historyWindowExceededMessage, true
	case errors.Is(err, tools.ErrToolCapExceeded):
		return toolCapExceededMessage, true
	case errors.Is(err, governance.ErrRateLimited):
		return rateLimitQPSMessage, true
	case errors.Is(err, tools.ErrMutatingActionDisabled):
		return mutatingToolsDisabledMessage, true
	default:
		return "", false
	}
}

func friendlyToolResultJSON(message string) string {
	raw, err := json.Marshal(map[string]any{
		"success": false,
		"message": message,
	})
	if err != nil {
		return message
	}
	return string(raw)
}

func friendlyMessageFromText(text string) (string, bool) {
	for _, message := range []string{
		rateLimitQPSMessage,
		rateLimitDailyMessage,
		toolCapExceededMessage,
		historyWindowExceededMessage,
		readExpensiveTurnBudgetMessage,
		mutatingToolsDisabledMessage,
	} {
		if message != "" && strings.Contains(text, message) {
			return message, true
		}
	}
	return "", false
}

func cappedTraceForFriendlyError(err error, message string) (string, string) {
	if errors.Is(err, governance.ErrRateLimited) ||
		strings.Contains(message, rateLimitQPSMessage) ||
		strings.Contains(message, rateLimitDailyMessage) ||
		strings.Contains(message, readExpensiveTurnBudgetMessage) {
		return observability.ToolCappedRateLimit, message
	}
	if errors.Is(err, tools.ErrHistoryWindowExceeded) || strings.Contains(message, historyWindowExceededMessage) {
		return observability.ToolCappedWindow, message
	}
	if errors.Is(err, tools.ErrToolCapExceeded) || strings.Contains(message, toolCapExceededMessage) {
		return observability.ToolCappedTargets, message
	}
	return "", ""
}

func blockedStepEvent(action, source string, args map[string]any, message string, err error) StepEvent {
	capped, reason := cappedTraceForFriendlyError(err, message)
	return StepEvent{
		Type:      StepBlocked,
		Action:    action,
		Source:    source,
		Args:      args,
		Message:   message,
		Capped:    capped,
		CapReason: reason,
	}
}

// finalReplyPrefix marks a tool result as a deterministic final reply that
// should be returned directly to the user without LLM narration.
const finalReplyPrefix = "\x00FINAL:"

// isFinalReply checks if a tool result is a deterministic final reply.
func isFinalReply(result string) (string, bool) {
	if strings.HasPrefix(result, finalReplyPrefix) {
		return strings.TrimPrefix(result, finalReplyPrefix), true
	}
	return "", false
}

// executeTool handles security check + execution for one tool call.
func (e *Engine) executeTool(ctx context.Context, tc openai.ToolCall, onStep func(StepEvent)) string {
	action := tc.Function.Name

	// Parse args first (needed for all paths)
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		errMsg := fmt.Sprintf("parameter parse error: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: errMsg})
		return errMsg
	}

	// Knowledge tools execute locally — no API call, no security check needed
	if knowledge.IsKnowledgeTool(action) {
		args = e.safeExecutor.FilterArgs(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceKnowledgeLocal, Args: args})
		result, err := knowledge.ExecuteTool(action, args)
		if err != nil {
			errMsg := fmt.Sprintf("知识查询失败: %v", err)
			onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceKnowledgeLocal, Message: errMsg})
			return errMsg
		}
		onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceKnowledgeLocal, Message: "查询成功", TraceResult: result})
		return knowledge.ResultToJSON(result)
	}

	// Workflow meta-tools → delegate to workflow engine.
	// Security: LLM-provided args are filtered here before entering the workflow.
	// Workflow steps bypass per-tool L1 checks because step definitions are hardcoded
	// (not LLM-controlled) and each workflow has its own Confirm step for user approval.
	// Invariant: BuildArgs functions must only reference specific named keys from wfCtx.Params.
	if workflow.IsWorkflowTool(action) {
		if !e.mutatingToolsEnabled {
			args = e.safeExecutor.FilterArgs(action, args)
			msg := mutatingToolsDisabledMessage
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, tools.ErrMutatingActionDisabled))
			return friendlyToolResultJSON(msg)
		}
		args = e.safeExecutor.FilterArgs(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: e.safeExecutor.RedactArgs(action, args)})
		return e.executeWorkflow(ctx, action, args, onStep)
	}

	// Diagnosis meta-tools → delegate to diagnosis engine.
	if diagnosis.IsDiagnosisTool(action) {
		args = e.safeExecutor.FilterArgs(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: e.safeExecutor.RedactArgs(action, args)})
		return e.executeDiagnosis(ctx, action, args, onStep)
	}

	if decision, ok := e.allowMutatingTool(action); !ok {
		msg := rateLimitMessage(decision.Reason)
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, governance.ErrRateLimited))
		return finalReplyPrefix + msg
	}

	result, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: tools.OriginDirectLLM,
		Hooks: tools.SafeToolHooks{
			OnConfirmNeeded: func(action string, args map[string]any) {
				onStep(StepEvent{Type: StepConfirmNeeded, Action: action, Source: observability.ToolSourceMainReAct, Args: e.safeExecutor.RedactArgs(action, args), Message: "此操作需要您确认"})
			},
			OnBeforeCall: func(action string, args map[string]any) {
				onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: e.safeExecutor.RedactArgs(action, args)})
			},
		},
	})
	if err != nil {
		if errors.Is(err, tools.ErrHistoricalMonitorUnsupported) {
			msg := refusal.MonitorHistoryUnsupported
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, err))
			return finalReplyPrefix + msg
		}
		if msg, ok := friendlyToolErrorMessage(err); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, err))
			return friendlyToolResultJSON(msg)
		}
		if errors.Is(err, tools.ErrDestructiveAction) {
			msg := fmt.Sprintf("安全限制：%s 是破坏性操作（L2），已拒绝执行。请到控制台手动操作。", action)
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
			return finalReplyPrefix + msg
		}
		if errors.Is(err, tools.ErrUserDeclined) {
			msg := fmt.Sprintf("操作已取消：%s 未执行。", friendlyActionName(action))
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
			return finalReplyPrefix + msg
		}
		errMsg := fmt.Sprintf("API 调用失败: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: errMsg})
		return errMsg
	}

	var display string
	if result.Display.Kind == "JupyterToken" && result.Display.Value != "" {
		display = fmt.Sprintf("Jupyter Token: %s", result.Display.Value)
	}

	formatted := prompt.FormatToolResult(result.LLMResult)
	onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "调用成功", Display: display, TraceResult: result.TraceResult, Attempts: result.Attempts})
	return formatted
}

func (e *Engine) allowMutatingTool(action string) (governance.Decision, bool) {
	policy, ok := e.safeExecutor.PolicyForAction(action)
	// Read-expensive classes use their own budget in checkReadExpensiveBudget.
	// Destructive L2 actions are blocked by SafeToolExecutor before execution
	// and do not consume quota. Only ActionClassMutating uses this budget.
	if !ok || policy.Class != tools.ActionClassMutating {
		return governance.Decision{Allowed: true, Class: governance.ClassMutatingTool, Action: action}, true
	}
	return e.allowRateLimited(governance.ClassMutatingTool, action)
}

func (e *Engine) checkReadExpensiveBudget(action string, origin tools.ExecutionOrigin) error {
	policy, ok := e.safeExecutor.PolicyForAction(action)
	if !ok || !isReadExpensiveClass(policy.Class) {
		return nil
	}
	if e.countsReadExpensiveTurnBudget(origin) {
		if e.readExpensiveCallsThisTurn >= maxReadExpensiveCallsPerTurn {
			return friendlyEngineError{cause: tools.ErrToolCapExceeded, message: readExpensiveTurnBudgetMessage}
		}
	}
	if decision, ok := e.allowRateLimited(governance.ClassReadExpensiveTool, action); !ok {
		return friendlyEngineError{cause: governance.ErrRateLimited, message: rateLimitMessage(decision.Reason)}
	}
	if e.countsReadExpensiveTurnBudget(origin) {
		e.readExpensiveCallsThisTurn++
	}
	return nil
}

func isReadExpensiveClass(class tools.ActionClass) bool {
	return class == tools.ActionClassReadExpensiveDefault || class == tools.ActionClassReadExpensivePerTarget
}

func (e *Engine) countsReadExpensiveTurnBudget(origin tools.ExecutionOrigin) bool {
	if e.userTurn == 0 {
		return false
	}
	return origin != tools.OriginWorkflowInternal
}

func (e *Engine) executeRawTool(ctx context.Context, action string, args map[string]any, origin tools.ExecutionOrigin) (map[string]any, error) {
	result, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: origin,
	})
	if err != nil {
		return nil, err
	}
	return result.RawResult, nil
}

func (e *Engine) executeSafeTool(ctx context.Context, req tools.SafeToolRequest) (*tools.SafeToolResult, error) {
	if err := e.checkReadExpensiveBudget(req.Action, req.Origin); err != nil {
		return nil, err
	}
	result, err := e.safeExecutor.ExecuteSafe(ctx, req)
	if err == nil && req.Action == "DescribeCompShareInstance" {
		e.lastInstanceQueryTurn = e.userTurn
	}
	if err == nil && req.Action == "GetCompShareInstanceMonitor" {
		e.lastMonitorTurn = e.userTurn
	}
	if err == nil {
		e.trackMonitorResult(result)
	}
	if err == nil && req.Origin == tools.OriginDirectLLM {
		e.markRegistryInvalidated(req.Action)
		e.recordToolFacts(req.Action, result)
	}
	return result, err
}

// recordToolFacts is the M2 ToolFact writer entry point. Called only on
// successful OriginDirectLLM tool calls — workflow-internal probing
// (OriginWorkflowInternal) and diagnosis-internal calls
// (OriginDiagnosisInternal) are filtered out by the caller, because
// those are not user-driven and would pollute "刚才那台" follow-up
// memory with intermediate state the user never asked about.
//
// Skip-without-effect cases (no fact written, no log noise):
//   - Engine not hydrated (no SetSessionState called this turn — e.g. CLI path).
//   - result is nil or RawResult is nil.
//   - Action is not in the v1 supported set.
//
// v1 supported actions:
//   - DescribeCompShareInstance → instance_state per UHostId
//   - GetCompShareInstanceMonitor → monitor_sample per UHostId
func (e *Engine) recordToolFacts(action string, result *tools.SafeToolResult) {
	if !e.sessionStateHydrated {
		return
	}
	if result == nil || result.RawResult == nil {
		return
	}
	switch action {
	case "DescribeCompShareInstance":
		e.recordInstanceStateFacts(result.RawResult)
	case "GetCompShareInstanceMonitor":
		e.recordMonitorSampleFacts(result.RawResult)
	}
}

// recordInstanceStateFacts extracts one instance_state fact per UHostId
// in the DescribeCompShareInstance result. Numeric fields (cpu, gpu,
// memory) are coerced to float64 via toFactNumeric to keep the payload
// round-trip stable per the contract on ToolFact.
func (e *Engine) recordInstanceStateFacts(raw map[string]any) {
	hosts, _ := raw["UHostSet"].([]any)
	if len(hosts) == 0 {
		return
	}
	nowUnix := time.Now().Unix()
	for _, item := range hosts {
		row, _ := item.(map[string]any)
		if row == nil {
			continue
		}
		snap := entity.InstanceFromMap(row)
		if snap.UHostId == "" {
			continue
		}
		payload := map[string]any{
			"name":     snap.Name,
			"state":    snap.State,
			"gpu":      toFactNumeric(snap.GPU),
			"gpu_type": snap.GpuType,
			"cpu":      toFactNumeric(snap.CPU),
			"memory":   toFactNumeric(snap.Memory),
			"zone":     snap.Zone,
		}
		e.sessionState.RecentFacts = appendFactToSlice(e.sessionState.RecentFacts, ToolFact{
			Kind:           FactKindInstanceState,
			SubjectID:      snap.UHostId,
			Payload:        payload,
			ProducedAtTurn: e.userTurn,
			ProducedAtUnix: nowUnix,
			TTLSeconds:     factTTLSecondsInstanceState,
		})
	}
}

// recordMonitorSampleFacts groups all per-metric scalars from a
// GetCompShareInstanceMonitor result by UHostId and writes one
// monitor_sample fact per host. Multi-GPU disambiguation suffixes
// (gpu_usage.GPU 1 / .GPU 2) are preserved as separate Payload keys
// inside the same per-host fact (M3 ContextAssembler reads them all).
//
// The empty-metrics filter in ExtractMonitorScalars defaults to "all
// known metric keys", so a fact captures whatever the host reported,
// not just what the user requested. This matters for follow-up Qs
// like "GPU 怎么样" after a CPU-only monitor query.
func (e *Engine) recordMonitorSampleFacts(raw map[string]any) {
	scalars := intent.ExtractMonitorScalars(raw, nil)
	if len(scalars) == 0 {
		return
	}
	nowUnix := time.Now().Unix()
	bySubject := make(map[string]map[string]any, len(scalars))
	for _, s := range scalars {
		if s.SubjectID == "" || s.Key == "" {
			continue
		}
		if _, ok := bySubject[s.SubjectID]; !ok {
			bySubject[s.SubjectID] = make(map[string]any)
		}
		bySubject[s.SubjectID][s.Key] = s.Value
	}
	for subjectID, payload := range bySubject {
		if !isAllAcceptedKeys(FactKindMonitorSample, payload) {
			continue
		}
		e.sessionState.RecentFacts = appendFactToSlice(e.sessionState.RecentFacts, ToolFact{
			Kind:           FactKindMonitorSample,
			SubjectID:      subjectID,
			Payload:        payload,
			ProducedAtTurn: e.userTurn,
			ProducedAtUnix: nowUnix,
			TTLSeconds:     factTTLSecondsMonitorSample,
		})
	}
}

// isAllAcceptedKeys verifies every key in payload is accepted for the
// given fact kind via isAcceptedPayloadKey. Used as a guard before
// storing a monitor_sample fact: if the renderer ever emits a key not
// in expectedPayloadKeysForKind (e.g. a new metric added to
// monitorMetricDefinitions but not yet to the contract), the fact is
// dropped instead of polluting the contract. M3 will see the gap and
// the test TestToolFact_PayloadKeysEnforced will catch it on the
// renderer-side first.
func isAllAcceptedKeys(kind string, payload map[string]any) bool {
	for k := range payload {
		if !isAcceptedPayloadKey(kind, k) {
			return false
		}
	}
	return true
}

// recordSelectedInstanceFromEnvelope sets SessionState.SelectedInstance{ID,Name}
// when the handler envelope identifies exactly one instance subject. Called
// only from cutover/resume success paths — see callers in tryPhase1Cutover
// and tryResumeResourceSelection.
//
// Gates:
//   - sessionStateHydrated — never mutate sessionState without an explicit
//     SetSessionState earlier in the turn (CLI path safety, matches the
//     fact writer's gate).
//   - env != nil and Subjects has exactly one item of type SubjectInstance
//     with non-empty ID.
//
// Why "exactly one": multi-instance results (e.g. "show all my instances")
// give Subjects > 1 — the user has not selected anything. Zero-instance
// results (filter matched nothing) give Subjects == 0 — same reasoning.
// This matches the M2 design doc §3.1: write only when the user has
// unambiguously identified a single instance.
func (e *Engine) recordSelectedInstanceFromEnvelope(env *envelope.Envelope) {
	if env == nil || !e.sessionStateHydrated {
		return
	}
	if len(env.Subjects) != 1 {
		return
	}
	s := env.Subjects[0]
	if s.Type != envelope.SubjectInstance || s.ID == "" {
		return
	}
	e.sessionState.SelectedInstanceID = s.ID
	e.sessionState.SelectedInstanceName = s.Name
}

// recordLastIntentFromPlan sets SessionState.LastIntent from the plan's
// classified Intent. Called only on cutover/resume success paths — i.e.
// when the user's intent was confirmed by a fully-dispatched handler
// reply. Refuses to write IntentUnknown / empty / non-RuntimeIntents
// values, so the stored value is always a legal short-circuited
// "future M3 ContextAssembler will switch on this" enum string.
func (e *Engine) recordLastIntentFromPlan(plan intent.Plan) {
	if !e.sessionStateHydrated {
		return
	}
	if plan.Intent == "" || plan.Intent == intent.IntentUnknown {
		return
	}
	if !runtimeIntentMember(plan.Intent) {
		return
	}
	e.sessionState.LastIntent = string(plan.Intent)
}

// runtimeIntentSet is a one-time-built membership set over intent.RuntimeIntents.
// Used by recordLastIntentFromPlan to refuse non-runtime values without
// taking a hard compile-time dep on the intent vocabulary from inside
// session_state.go (the engine package already imports intent, so this
// is internal-only).
var runtimeIntentSet = func() map[intent.Intent]struct{} {
	out := make(map[intent.Intent]struct{}, len(intent.RuntimeIntents()))
	for _, i := range intent.RuntimeIntents() {
		out[i] = struct{}{}
	}
	return out
}()

func runtimeIntentMember(i intent.Intent) bool {
	_, ok := runtimeIntentSet[i]
	return ok
}

func (e *Engine) markRegistryInvalidated(action string) {
	if e.registry == nil {
		return
	}
	e.registry.MarkInvalidated(action)
}

func (e *Engine) toolExecutorFor(origin tools.ExecutionOrigin) tools.ToolExecutor {
	return engineToolExecutor{engine: e, origin: origin}
}

type engineToolExecutor struct {
	engine *Engine
	origin tools.ExecutionOrigin
}

func (x engineToolExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return x.engine.executeRawTool(ctx, action, args, x.origin)
}

// guardMonitorTemporalFinalReply is retained for the future historical-monitor
// stage. It is unreachable for new monitor history calls while
// ErrHistoricalMonitorUnsupported blocks StartTime/EndTime tool arguments.
func (e *Engine) guardMonitorTemporalFinalReply(content string) string {
	if !e.currentMonitorWindow || content == "" {
		return content
	}
	if e.allCurrentHistoricalMonitorResultsNoData() {
		return formatHistoricalMonitorNoDataReply(e.currentMonitorStart, e.currentMonitorEnd, e.currentMonitorNoData)
	}

	startAt := time.Unix(e.currentMonitorStart, 0).In(beijingZone)
	endAt := time.Unix(e.currentMonitorEnd, 0).In(beijingZone)
	targetDate := startAt.Format("2006-01-02")
	targetTimeRange := fmt.Sprintf("%s ~ %s", startAt.Format("15:04"), endAt.Format("15:04"))
	corrected := isoDateRE.ReplaceAllStringFunc(content, func(date string) string {
		if date == targetDate {
			return date
		}
		return targetDate
	})
	corrected = clockRangeRE.ReplaceAllString(corrected, targetTimeRange)
	replacements := map[string]string{
		"当前实时监控":  "该历史时间窗监控",
		"当前监控":    "该历史时间窗监控",
		"当前实时":    "该历史时间窗",
		"当前值":     "该时间窗值",
		"最近较短时间内": "指定历史时间窗内",
	}
	for old, repl := range replacements {
		corrected = strings.ReplaceAll(corrected, old, repl)
	}
	return corrected
}

// trackMonitorResult is retained for the future historical-monitor stage.
// Current user-facing paths reject history-window monitor calls before API
// execution, so this only protects legacy/internal test seams.
func (e *Engine) trackMonitorResult(result *tools.SafeToolResult) {
	if result == nil || result.Action != "GetCompShareInstanceMonitor" || !hasMonitorTimeRangeArgs(result.Args) {
		return
	}
	targets := extractMonitorTargets(result.Args)
	e.currentMonitorTargets = append(e.currentMonitorTargets, targets...)
	if start, end, ok := monitorTimeWindow(result.Args); ok {
		if !e.currentMonitorWindow {
			e.currentMonitorStart = start
			e.currentMonitorEnd = end
			e.currentMonitorWindow = true
		} else {
			if start < e.currentMonitorStart {
				e.currentMonitorStart = start
			}
			if end > e.currentMonitorEnd {
				e.currentMonitorEnd = end
			}
		}
	}
	if status, _ := result.LLMResult["MonitorDataStatus"].(string); status == "NO_DATA_IN_REQUESTED_WINDOW" {
		e.currentMonitorNoData = append(e.currentMonitorNoData, targets...)
	}
}

func (e *Engine) allCurrentHistoricalMonitorResultsNoData() bool {
	if len(e.currentMonitorTargets) == 0 {
		return false
	}
	noData := make(map[string]bool, len(e.currentMonitorNoData))
	for _, target := range e.currentMonitorNoData {
		noData[target] = true
	}
	for _, target := range e.currentMonitorTargets {
		if !noData[target] {
			return false
		}
	}
	return true
}

func formatHistoricalMonitorNoDataReply(start, end int64, targets []string) string {
	startText := time.Unix(start, 0).In(beijingZone).Format("2006-01-02 15:04")
	endText := time.Unix(end, 0).In(beijingZone).Format("2006-01-02 15:04")
	targetText := strings.Join(uniqueStrings(targets), "、")
	if targetText == "" {
		targetText = "所查实例"
	}
	return fmt.Sprintf("北京时间 %s ~ %s，%s 没有返回有效监控数据。不能判断该时间窗内的 CPU、内存、GPU 或显存占用，也不会用其他时间的数据替代。", startText, endText, targetText)
}

func hasMonitorTimeRangeArgs(args map[string]any) bool {
	if args == nil {
		return false
	}
	_, hasStart := args["StartTime"]
	_, hasEnd := args["EndTime"]
	return hasStart || hasEnd
}

func monitorTimeWindow(args map[string]any) (int64, int64, bool) {
	start, okStart := int64Arg(args["StartTime"])
	end, okEnd := int64Arg(args["EndTime"])
	if !okStart || !okEnd {
		return 0, 0, false
	}
	if end < start {
		return 0, 0, false
	}
	return start, end, true
}

func int64Arg(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func extractMonitorTargets(args map[string]any) []string {
	if args == nil {
		return nil
	}
	var targets []string
	switch v := args["UHostIds"].(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				targets = append(targets, s)
			}
		}
	case []string:
		for _, s := range v {
			if s != "" {
				targets = append(targets, s)
			}
		}
	case string:
		if v != "" {
			targets = append(targets, v)
		}
	}
	return targets
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// executeWorkflow runs a predefined workflow and returns the result as a JSON string
// for the LLM to narrate.
func (e *Engine) executeWorkflow(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	if !e.mutatingToolsEnabled {
		msg := mutatingToolsDisabledMessage
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, tools.ErrMutatingActionDisabled))
		return finalReplyPrefix + msg
	}
	// Hard guard — instance-operation workflows MUST have a non-empty UHostId.
	// CreateInstanceWorkflow is excluded because it creates a new instance.
	// NOTE: If you add a workflow that does not target an existing instance,
	// add it to this exclusion list. The default is to block (fail-safe).
	if action != "CreateInstanceWorkflow" {
		uHostId, _ := args["UHostId"].(string)
		if uHostId == "" {
			if single, _ := e.singleRegistryInstance(); single != "" {
				args["UHostId"] = single
				uHostId = single
			}
		}
		if uHostId == "" {
			msg := "请先确认要操作的实例。当有多个实例时，请列出实例列表让用户选择后再执行操作。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
			guardResult := map[string]any{"success": false, "message": msg}
			b, _ := json.Marshal(guardResult)
			return string(b)
		}
	}

	wf, ok := workflow.GetWorkflow(action)
	if !ok {
		msg := fmt.Sprintf("未知的工作流: %s", action)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
		return msg
	}

	if decision, ok := e.allowMutatingTool(action); !ok {
		msg := rateLimitMessage(decision.Reason)
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, governance.ErrRateLimited))
		return finalReplyPrefix + msg
	}

	var wfConfirm workflow.ConfirmFunc
	if e.confirmFn != nil {
		wfConfirm = workflow.ConfirmFunc(e.confirmFn)
	}

	wfEngine := workflow.NewEngine(e.toolExecutorFor(tools.OriginWorkflowInternal), wfConfirm, func(ev workflow.StepEvent) {
		eventType := StepToolCall
		message := fmt.Sprintf("[%d/%d] %s: %s", ev.StepIndex+1, ev.Total, ev.StepName, ev.Status)
		if ev.Message != "" {
			message = message + ": " + ev.Message
		}
		capped, capReason := cappedTraceForFriendlyError(nil, ev.Message)
		if ev.Type == workflow.StepConfirm {
			if ev.Status == "waiting" {
				eventType = StepConfirmNeeded
			} else if ev.Status == "cancelled" {
				eventType = StepBlocked
			}
		}
		switch ev.Status {
		case "failed":
			eventType = StepError
			if _, ok := friendlyMessageFromText(ev.Message); ok {
				eventType = StepBlocked
			}
		case "success":
			if ev.Type == workflow.StepToolCall {
				eventType = StepToolResult
			}
		}
		onStep(StepEvent{
			Type:      eventType,
			Action:    ev.Tool,
			Source:    observability.ToolSourceWorkflowInternal,
			Args:      e.safeExecutor.RedactArgs(ev.Tool, ev.Args),
			Message:   message,
			Capped:    capped,
			CapReason: capReason,
		})
	})

	result, err := wfEngine.Run(ctx, wf, args)
	if err != nil {
		if msg, ok := friendlyToolErrorMessage(err); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, msg, err))
			return finalReplyPrefix + msg
		}
		msg := fmt.Sprintf("工作流执行错误: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
		return msg
	}
	if !result.Success {
		if msg, ok := friendlyMessageFromText(result.Message); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, msg, nil))
			return finalReplyPrefix + msg
		}
	}

	// User-cancelled workflows return a deterministic reply directly
	if !result.Success && result.Message == "用户取消了操作" {
		return finalReplyPrefix + fmt.Sprintf("好的，已取消%s操作。", friendlyActionName(action))
	}

	if result.Success {
		e.markRegistryInvalidated(action)
	}

	b, _ := json.Marshal(result)
	return string(b)
}

// executeDiagnosis runs a diagnostic chain and returns the result as JSON.
func (e *Engine) executeDiagnosis(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	chain, ok := diagnosis.GetChain(action)
	if !ok {
		msg := fmt.Sprintf("未知的诊断链: %s", action)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
		return msg
	}
	uid, _ := args["UHostId"].(string)

	// Vague-failure guard — DiagnoseInitFailure only.
	// Gate 1 (symptom specificity): the user message must contain an
	// init-failure-specific signal. Vague fault language like "跑崩了" /
	// "挂了" is blocked here, even if the LLM provided a target instance.
	// This is a hard safety net behind the prompt-level vague_failure
	// routing class — deliberately does NOT redirect to another Diagnose*.
	if action == "DiagnoseInitFailure" && !containsInitFailureSignal(e.lastUserMsg) &&
		!(uid != "" && e.previousAssistantAskedInitFailureTarget()) {
		msg := "请问是哪台实例出了问题？能描述一下具体现象吗（例如：SSH 断了、GPU 报错、服务崩了、初始化卡住等）？"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
		return finalReplyPrefix + msg
	}

	// Gate 2 (instance disambiguation): symptom is specific, but if no
	// target was provided and the user did not ask for a scan-all, ask
	// which instance. Avoids implicit scan-all when the user has a
	// specific instance in mind but didn't name it.
	//
	// Target check is UHostId-only because SafeToolExecutor filters upstream
	// strips any field not in the DiagnoseInitFailure schema (which only
	// declares UHostId). The LLM is expected to resolve names to UHostIds
	// upstream; if it doesn't, this gate correctly falls through to
	// clarification.
	if action == "DiagnoseInitFailure" {
		if uid == "" && !containsScanAllSignal(e.lastUserMsg) {
			msg := "请问是哪台实例的初始化失败了？"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
			return finalReplyPrefix + msg
		}
	}

	diagEngine := diagnosis.NewEngine(e.toolExecutorFor(tools.OriginDiagnosisInternal), func(ev diagnosis.DiagEvent) {
		var eventType StepType
		message := fmt.Sprintf("[诊断 %d/%d] %s: %s", ev.StepIndex+1, ev.Total, ev.StepName, ev.Status)
		if ev.Message != "" {
			message = message + ": " + ev.Message
		}
		capped, capReason := cappedTraceForFriendlyError(nil, ev.Message)
		switch ev.Status {
		case "running":
			eventType = StepToolCall
		case "failed":
			eventType = StepError
			if _, ok := friendlyMessageFromText(ev.Message); ok {
				eventType = StepBlocked
			}
		default: // "checked", "concluded"
			eventType = StepToolResult
		}
		onStep(StepEvent{
			Type:      eventType,
			Action:    ev.Tool,
			Source:    observability.ToolSourceDiagnosisInternal,
			Args:      e.safeExecutor.RedactArgs(ev.Tool, ev.Args),
			Message:   message,
			Capped:    capped,
			CapReason: capReason,
		})
	})

	result, err := diagEngine.Run(ctx, chain, args)
	if err != nil {
		if msg, ok := friendlyToolErrorMessage(err); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, msg, err))
			return finalReplyPrefix + msg
		}
		msg := fmt.Sprintf("诊断执行错误: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
		return msg
	}
	if !result.Success {
		if msg, ok := friendlyMessageFromText(result.Conclusion); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, msg, nil))
			return finalReplyPrefix + msg
		}
	}

	b, _ := json.Marshal(result)
	return string(b)
}

// StepType identifies what kind of intermediate event occurred.
type StepType int

const (
	StepToolCall      StepType = iota // About to call a tool
	StepToolResult                    // Tool returned result
	StepConfirmNeeded                 // L1 operation needs confirmation
	StepBlocked                       // L2 operation blocked
	StepError                         // Error occurred
)

// StepEvent is an intermediate event during the ReAct loop.
type StepEvent struct {
	Type                       StepType
	Action                     string
	Source                     string
	Args                       map[string]any
	Message                    string
	Display                    string         // content for CLI display only (not sent to LLM), e.g. raw JupyterToken
	TraceResult                map[string]any // redacted result payload for trace hashing only
	Attempts                   int
	RendererInputToolArgHashes []string
	Capped                     string
	CapReason                  string
	RequestedTargets           int
	ExecutedTargets            int
	WindowSeconds              int
}

// trimHistory keeps the message list under maxHistoryMessages by dropping
// the oldest non-system messages. The system prompt (index 0) is always kept.
// Cut point is aligned to a safe message boundary to avoid orphaned tool_calls
// or tool responses (which would make the history malformed for the LLM).
func (e *Engine) trimHistory() {
	if len(e.messages) <= 1+maxHistoryMessages {
		return
	}

	// Target: keep system (index 0) + last maxHistoryMessages messages.
	// Start from the candidate cut point and scan forward to find a safe boundary.
	// Safe boundary = a message whose role is "user" or "assistant" without tool_calls.
	// This ensures we never start with an orphaned tool message or leave
	// an assistant(tool_calls) without its matching tool responses.
	candidateStart := len(e.messages) - maxHistoryMessages
	if candidateStart <= 1 {
		return
	}

	safeStart := candidateStart
	for safeStart < len(e.messages) {
		msg := e.messages[safeStart]
		if msg.Role == openai.ChatMessageRoleUser {
			break // user message is always a safe boundary
		}
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) == 0 {
			break // plain assistant reply is safe
		}
		// Skip tool messages and assistant(tool_calls) to find the next safe point
		safeStart++
	}

	if safeStart >= len(e.messages) {
		return // no safe cut point found, don't trim
	}

	keep := e.messages[safeStart:]
	e.messages = append([]openai.ChatCompletionMessage{e.messages[0]}, keep...)
}

// staleStateNote is a temporary system message injected when prior instance
// state may be outdated. It nudges the model to re-query before acting.
const staleStateNote = "注意：本轮之前的对话中获取的实例状态信息可能已过时，用户可能已在控制台侧手动操作实例。\n如果本轮需要基于实例当前状态作出判断，或执行实例变更操作，必须先调用 DescribeCompShareInstance 获取最新状态后再决策。"

// buildMessagesForLLM returns the message slice to send to the LLM.
// If instance state from a prior turn may be stale, a temporary system note
// is appended. The note is NOT persisted in e.messages.
func (e *Engine) buildMessagesForLLM() []openai.ChatCompletionMessage {
	if e.lastInstanceQueryTurn < 0 || e.lastInstanceQueryTurn >= e.userTurn {
		return e.messages
	}
	// Insert stale note immediately before the latest user message, so the
	// model sees the warning right next to the ask it's about to answer.
	// This is much higher attention than burying the note at index 1
	// (after the main system prompt) in a long conversation.
	lastUserIdx := -1
	for i := len(e.messages) - 1; i >= 0; i-- {
		if e.messages[i].Role == openai.ChatMessageRoleUser {
			lastUserIdx = i
			break
		}
	}
	note := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: staleStateNote,
	}
	// Fallback: no user message in history (shouldn't happen in the ReAct
	// loop, but keep the helper total). Append at end.
	if lastUserIdx < 0 {
		msgs := make([]openai.ChatCompletionMessage, len(e.messages), len(e.messages)+1)
		copy(msgs, e.messages)
		return append(msgs, note)
	}
	msgs := make([]openai.ChatCompletionMessage, 0, len(e.messages)+1)
	msgs = append(msgs, e.messages[:lastUserIdx]...)
	msgs = append(msgs, note)
	msgs = append(msgs, e.messages[lastUserIdx:]...)
	return msgs
}

// PR9 removed ensureProjectId / externalExecutor / pickProjectId. The
// auto-discovery path called ExternalExecutor.SetProjectId, which mutated
// a SharedDeps singleton across sessions — one user's discovered project
// id ended up auto-injected into another user's tool calls. ProjectId now
// only flows from cfg → NewExternalExecutor at construction; runtime
// mutation is gone. When mutating tools that need ProjectId open up,
// route the value through args["ProjectId"] (per-session field on Engine).

// accountBillingUnsupportedReply moved to internal/refusal/templates.go
// (refusal.AccountBillingUnsupported) in the C2 hard-block 归一 refactor.

var monthlyBillKeywords = []string{
	"\u672c\u6708", // 本月
	"\u6708\u5ea6", // 月度
	"\u5f53\u6708", // 当月
}

// thirdPartyServiceForeignBalanceContexts are prefixes that indicate the
// query is about a *third-party service's* account state, not CompShare's.
// When any of these substrings appear in a normalized user message, the
// account-billing hard-block must NOT fire — the question is RAG-answerable
// from corpus docs covering that service (e.g. ModelVerse 18 chunks in
// w0 corpus, OpenAI client docs, etc.).
//
// CONSTRAINT (per skill phase prereq doc; see .claude/CONTEXT.md "Skill 化
// prereq" section): this list is intentionally small and bounded. Do NOT
// grow it as a way to handle new false-positive cases; instead, surface
// those cases as input to skill scope schema design. Long-term home is
// the skill manifest's disambiguating_prefixes field, NOT this engine var.
//
// All entries are stored in normalized form (ASCII lowercased; CJK as-is)
// to match textutil.Normalize output without re-normalizing per call.
var thirdPartyServiceForeignBalanceContexts = []string{
	"modelverse",
	"openai",
	"open ai",
	"anthropic",
	"deepseek",
	"deep seek",
	"volcano",
	"火山", // 火山(火山方舟 / 火山引擎)
	"豆包", // 豆包
}

// accountOnlyDataKeywords are signals that ONLY the account financial
// center can satisfy. Their presence triggers the hard-block regardless
// of instance words elsewhere in the message.
var accountOnlyDataKeywords = []string{
	"\u4f59\u989d",                   // 余额
	"\u603b\u8d26\u5355",             // 总账单
	"\u6d88\u8d39\u6d41\u6c34",       // 消费流水
	"\u6d41\u6c34",                   // 流水
	"\u8d26\u5355\u660e\u7ec6",       // 账单明细
	"\u6263\u8d39\u8bb0\u5f55",       // 扣费记录
	"\u4ea4\u6613\u8bb0\u5f55",       // 交易记录
	"\u5f85\u652f\u4ed8\u8d26\u5355", // 待支付账单
	"\u8ba2\u5355\u72b6\u6001",       // 订单状态
	"balance",
	"transaction record",
	"charge record",
	"payable bill",
	"order status",
}

// monthlyAccountCostKeywords are cost-related words that, when paired
// with a monthly time word, indicate an account-level monthly summary
// question (which is unsupported). Kept separate from the broader
// instance-side billing vocabulary so we don't over-trigger.
var monthlyAccountCostKeywords = []string{
	"\u8d39\u7528",       // 费用
	"\u82b1\u4e86",       // 花了
	"\u82b1\u8d39",       // 花费
	"\u6d88\u8d39",       // 消费
	"\u8d26\u5355",       // 账单
	"\u6263\u8d39",       // 扣费
	"\u6263\u4e86",       // 扣了
	"\u591a\u5c11\u94b1", // 多少钱
	"\u591a\u5c11",       // 多少
}

// accountInstanceScopeKeywords vetoes the monthly-summary branch ONLY.
// The unambiguous account-data branch above is NOT vetoed by these.
var accountInstanceScopeKeywords = []string{
	"\u5b9e\u4f8b", // 实例
	"\u673a\u5668", // 机器
	"\u4e3b\u673a", // 主机
	"\u54ea\u53f0", // 哪台
	"\u6bcf\u53f0", // 每台
	"\u8fd9\u4e9b", // 这些
	"\u54ea\u4e9b", // 哪些
	"\u8fd9\u53f0", // 这台
	"uhost-",
}

var invoiceSubjectKeywords = []string{
	"\u53d1\u7968", // 发票
	"\u5f00\u7968", // 开票
	"invoice",
}

var invoiceRealtimeKeywords = []string{
	"\u72b6\u6001",       // 状态
	"\u8fdb\u5ea6",       // 进度
	"\u5bc4\u9001",       // 寄送
	"\u7269\u6d41",       // 物流
	"\u5230\u54ea",       // 到哪
	"\u5f00\u597d",       // 开好
	"\u5f00\u51fa\u6765", // 开出来
	"\u4e0b\u8f7d",       // 下载
	"\u901a\u8fc7",
	"\u5ba1\u6838",
	"\u6210\u529f",
	"\u4e86\u5417",
	"\u597d\u4e86\u5417",
	"status",
	"progress",
	"delivery",
	"approved",
	"review",
}

var refundSubjectKeywords = []string{
	"\u9000\u6b3e", // 退款
	"refund",
}

var refundRealtimeKeywords = []string{
	"\u8fdb\u5ea6",                   // 进度
	"\u5230\u8d26",                   // 到账
	"\u72b6\u6001",                   // 状态
	"\u5230\u54ea",                   // 到哪
	"\u4ec0\u4e48\u65f6\u5019\u5230", // 什么时候到
	"\u51e0\u65f6\u5230",             // 几时到
	"\u6ca1\u5230\u8d26",             // 没到账
	"\u91d1\u989d",                   // 金额
	"\u6210\u529f",
	"\u4e86\u5417",
	"\u597d\u4e86\u5417",
	"progress",
	"status",
	"received",
	"success",
}

var arrearsSubjectKeywords = []string{
	"\u6b20\u8d39", // 欠费
	"arrears",
	"overdue",
}

var arrearsRealtimeKeywords = []string{
	"\u91d1\u989d",       // 金额
	"\u591a\u5c11",       // 多少
	"\u4e86\u5417",       // 了吗
	"\u662f\u5426",       // 是否
	"\u72b6\u6001",       // 状态
	"\u8ba2\u5355",       // 订单
	"\u8d26\u5355",       // 账单
	"\u5f85\u652f\u4ed8", // 待支付
	"amount",
	"status",
	"payable",
}

var packageSubjectKeywords = []string{
	"\u5957\u9910", // 套餐
	"package",
}

var packageRealtimeKeywords = []string{
	"\u4ec0\u4e48\u65f6\u5019", // 什么时候
	"\u5230\u671f\u65f6\u95f4", // 到期时间
	"\u5230\u671f\u4e86\u5417", // 到期了吗
	"\u8fd8\u5269\u591a\u4e45", // 还剩多久
	"when",
	"expire time",
}

var rechargeSubjectKeywords = []string{
	"\u5145\u503c", // 充值
	"recharge",
	"top up",
}

var rechargeRealtimeKeywords = []string{
	"\u591a\u5c11",       // 多少
	"\u591a\u5c11\u94b1", // 多少钱
	"\u91d1\u989d",       // 金额
	"\u8bb0\u5f55",       // 记录
	"\u72b6\u6001",       // 状态
	"\u6210\u529f",       // 成功
	"\u5230\u8d26",       // 到账
	"amount",
	"record",
	"status",
}

var monitorRecallKeywords = []string{
	"刚才",
	"刚刚",
	"继续",
	"那台",
	"那几台",
	"再看",
	"还有",
	"异常",
	"只看",
}

var monitorMetricKeywords = []string{
	"监控",
	"cpu",
	"gpu",
	"显存",
	"内存",
	"利用率",
	"vram",
	"memory",
}

var historicalMonitorTimeKeywords = []string{
	"\u6628\u5929",             // 昨天
	"\u6628\u665a",             // 昨晚
	"\u524d\u5929",             // 前天
	"\u4eca\u65e9",             // 今早
	"\u4eca\u5929\u65e9\u4e0a", // 今天早上
	"\u4eca\u5929\u4e0a\u5348", // 今天上午
	"\u4eca\u5929\u4e0b\u5348", // 今天下午
	"\u4eca\u5929\u665a\u4e0a", // 今天晚上
	"\u4eca\u5929\u51cc\u6668", // 今天凌晨
	"\u4e0a\u5468",             // 上周
	"\u4e0a\u4e2a\u6708",       // 上个月
	"\u4e0a\u6708",             // 上月
	"\u672c\u5468",             // 本周
	"\u672c\u6708",             // 本月
	"\u534a\u4e2a\u6708",       // 半个月
	"\u8fc7\u53bb",             // 过去
	"\u6700\u8fd1",             // 最近
	"yesterday",
	"last night",
	"today morning",
	"this morning",
	"last week",
	"last month",
	"past",
	"previous",
}

var historicalMonitorSignalKeywords = []string{
	"monitor", "cpu", "gpu", "vram", "memory", "idle", "busy", "load",
	"\u76d1\u63a7",       // 监控
	"\u663e\u5b58",       // 显存
	"\u5185\u5b58",       // 内存
	"\u5229\u7528\u7387", // 利用率
	"\u8d1f\u8f7d",       // 负载
	"\u7a7a\u95f2",       // 空闲
	"\u5fd9",             // 忙
}

// Keyword sets feed inferKnowledgeProductArea below. Each set MUST emit a
// product_area string that matches a label in deploy/kb/stage2b_w0.jsonl \u2014
// otherwise the +2 BM25 productArea boost in Retriever.scoreChunk is a no-op.
// Corpus labels (228 chunks as of 2026-05-20):
//
//	modelverse(97) login(35) resource_purchase(28) image(26)
//	billing_rule(24) driver_cuda(6) init_failure(5) windows(5) monitor(2)
var knowledgeBillingRuleKeywords = []string{
	"billing", "bill", "charge", "cost", "fee", "price", "balance",
	"\u8ba1\u8d39", "\u6263\u8d39", "\u6536\u8d39", "\u8d26\u5355", "\u4f59\u989d", "\u8d39\u7528", "\u4ef7\u683c",
	"invoice", "refund", "arrears", "renewal", "expire",
	"\u53d1\u7968", "\u5f00\u7968", "\u9000\u6b3e", "\u6b20\u8d39", "\u7eed\u8d39", "\u5230\u671f", "\u5305\u65e5", "\u5305\u65f6", "\u5305\u6708", "\u6309\u91cf",
}

var knowledgeImageKeywords = []string{
	"image", "images", "\u955c\u50cf",
}

var knowledgeLoginKeywords = []string{
	"login", "ssh", "jupyter", "jupyterlab", "token", "password",
	"\u767b\u5f55", "\u8fde\u63a5", "\u5bc6\u7801", "\u53e3\u4ee4",
}

var knowledgeModelverseKeywords = []string{
	"model", "models", "claude", "anthropic", "credit", "credits",
	"modelverse",
	"\u6a21\u578b", "\u5957\u9910", "\u79ef\u5206",
}

var knowledgeResourcePurchaseKeywords = []string{
	"\u8d2d\u4e70",       // \u8d2d\u4e70
	"\u89c4\u683c",       // \u89c4\u683c
	"\u62a2\u5360\u5f0f", // \u62a2\u5360\u5f0f
	"\u72ec\u5360\u5f0f", // \u72ec\u5360\u5f0f
}

var knowledgeDriverCudaKeywords = []string{
	"nvidia", "cuda", "nvidia-smi", "driver",
	"\u9a71\u52a8",             // \u9a71\u52a8
	"\u663e\u5361\u9a71\u52a8", // \u663e\u5361\u9a71\u52a8
}

var knowledgeInitFailureKeywords = []string{
	"initializing", "init fail", "install fail",
	"\u521d\u59cb\u5316\u5931\u8d25", // \u521d\u59cb\u5316\u5931\u8d25
	"\u521d\u59cb\u5316\u5361\u4f4f", // \u521d\u59cb\u5316\u5361\u4f4f
	"\u542f\u52a8\u5931\u8d25",       // \u542f\u52a8\u5931\u8d25
}

var knowledgeWindowsKeywords = []string{
	"windows", "rdp", "remote desktop",
	"\u8fdc\u7a0b\u684c\u9762", // \u8fdc\u7a0b\u684c\u9762
}

var knowledgeMonitorKeywords = []string{
	"\u76d1\u63a7\u6307\u6807", // \u76d1\u63a7\u6307\u6807
	"\u663e\u5b58\u5360\u7528", // \u663e\u5b58\u5360\u7528
	// textutil.Normalize collapses whitespace but never INJECTS a space between
	// adjacent CJK and ASCII, so the no-space variants are the load-bearing
	// keywords for real user input ("CPU\u5360\u7528\u7387"). Keep the spaced variants
	// for the alt phrasing ("CPU \u5360\u7528\u7387\u9ad8\u5417").
	"cpu\u5360\u7528",  // cpu\u5360\u7528
	"gpu\u5360\u7528",  // gpu\u5360\u7528
	"cpu \u5360\u7528", // cpu \u5360\u7528
	"gpu \u5360\u7528", // gpu \u5360\u7528
}

// normalizeMsg was moved to internal/textutil.Normalize in the C2
// hard-block 归一 refactor. All engine call sites now invoke
// textutil.Normalize directly. See textutil/normalize.go for the
// canonical implementation + per-package unit tests.

func (e *Engine) previousAssistantAskedInitFailureTarget() bool {
	if e == nil {
		return false
	}
	for i := len(e.messages) - 1; i >= 0; i-- {
		msg := e.messages[i]
		if msg.Role != openai.ChatMessageRoleAssistant {
			continue
		}
		if len(msg.ToolCalls) > 0 {
			continue
		}
		n := textutil.Normalize(msg.Content)
		if n == "" {
			continue
		}
		if strings.Contains(n, "具体现象") ||
			strings.Contains(n, "例如") {
			return false
		}
		return strings.Contains(n, "初始化") &&
			(strings.Contains(n, "哪台") || strings.Contains(n, "哪一台") || strings.Contains(n, "具体"))
	}
	return false
}

// initFailureSignalKeywords is a narrow word list that marks a user message
// as specifically about init-failure symptoms. Keep it tight — keywords
// like "起不来" are too ambiguous (could be SSH / GPU / service) and must
// NOT live here.
var initFailureSignalKeywords = []string{
	"初始化失败",
	"install fail",
	"卡在初始化",
	"卡在启动",
	"开不了机",
	"启动失败",
	"无法启动",
	"启动不了",
	"开机失败",
	"stop 后启动失败",
	"stop后启动失败",
	"starting很久",
	"starting 很久",
	"一直starting",
	"一直 starting",
}

// containsInitFailureSignal reports whether the user message contains an
// init-failure-specific symptom signal. This is Gate 1 of the
// DiagnoseInitFailure guard: vague fault language ("跑崩了", "挂了") does
// NOT match; the user must have named the symptom type explicitly.
func containsInitFailureSignal(msg string) bool {
	n := textutil.Normalize(msg)
	for _, kw := range initFailureSignalKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// scanAllSignalKeywords is a narrow list of phrases that indicate the user
// explicitly wants a broad scan across all instances. Used only as Gate 2
// of the DiagnoseInitFailure guard — consulted AFTER the symptom-specificity
// gate passes. A scan-all phrase alone (without an init-failure signal)
// does NOT bypass the guard.
var scanAllSignalKeywords = []string{
	"所有实例",
	"全部实例",
	"哪些实例",
	"有哪些",
	"帮我扫",
	"全量",
	"所有的",
	"全部失败",
	"失败的实例",
	"扫一下失败",
	"都有哪些",
}

// containsScanAllSignal reports whether the user message expresses an
// explicit intent to scan across all instances.
func containsScanAllSignal(msg string) bool {
	n := textutil.Normalize(msg)
	for _, kw := range scanAllSignalKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// isAccountBillingUnsupported is a permanent product capability boundary.
// Per docs/agent/plan/stage2-intent-planner.md §3.9.3, account-level
// billing/balance/transaction queries are out of scope for the agent
// regardless of IntentPlan classification. Planner may emit
// intent=billing_account_unsupported as a hint, but engine independently
// enforces this hard-block. DO NOT delete when IntentPlan ships.
func isAccountBillingUnsupported(userMsg string) bool {
	return isAccountBillingUnsupportedNormalized(textutil.Normalize(userMsg))
}

// resourceShortageSignalKeywords are the precision-first phrases that
// flag a user message as asking about upstream GPU pool exhaustion
// (error code 226604: "当前资源不足，请稍后再试"). The matcher narrowly
// targets product-specific phrases so it does not collide with adjacent
// "X 不足" senses (余额不足 / 积分不足 / 权限不足).
var resourceShortageSignalKeywords = []string{
	"226604",
	"资源不足", // 资源不足
}

// isResourceShortageQuestion reports whether the user message is asking
// about upstream resource shortage (error code 226604). When true the
// engine short-circuits before any LLM/planner/RAG call and returns
// refusal.ResourceShortage226604 unchanged, so the response stays stable across
// runs and never drifts via LLM paraphrase. Mirrors the
// isAccountBillingUnsupported / isUnsupportedHistoricalMonitorQuestion
// pattern.
func isResourceShortageQuestion(userMsg string) bool {
	n := textutil.Normalize(userMsg)
	for _, kw := range resourceShortageSignalKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

func containsInvoiceRealtimeQuestion(n string) bool {
	return containsAnyKeyword(n, invoiceSubjectKeywords) && containsAnyKeyword(n, invoiceRealtimeKeywords)
}

func containsRefundRealtimeQuestion(n string) bool {
	return containsAnyKeyword(n, refundSubjectKeywords) && containsAnyKeyword(n, refundRealtimeKeywords)
}

func containsArrearsRealtimeQuestion(n string) bool {
	return containsAnyKeyword(n, arrearsSubjectKeywords) && containsAnyKeyword(n, arrearsRealtimeKeywords)
}

func containsPackageRealtimeQuestion(n string) bool {
	return containsAnyKeyword(n, packageSubjectKeywords) && containsAnyKeyword(n, packageRealtimeKeywords)
}

func containsRechargeRealtimeQuestion(n string) bool {
	return containsAnyKeyword(n, rechargeSubjectKeywords) && containsAnyKeyword(n, rechargeRealtimeKeywords)
}

// isAccountBillingUnsupportedNormalized hard-blocks two disjoint classes
// of account-level requests the agent cannot satisfy:
//
//  1. Unambiguous account-financial-center data: 余额 / 总账单 /
//     消费流水 / 流水 / balance. These live in the user's billing
//     center, not in any per-instance API. Even when the message also
//     names instances (e.g. "这些机器导致账号余额还剩多少"), the answer
//     still has to come from the billing center — hard-block ALWAYS,
//     no instance-scope veto.
//
//  2. Account-level monthly summary phrasings (本月/当月/月度 + 费用/
//     花了/消费/账单/扣费) when the user does NOT name a specific
//     instance. Empirically, deepseek-v4-flash violates a prompt-only
//     soft guidance and calls DiagnoseBilling on these, so we keep
//     this as a hard-block. When instance-scope words co-occur, fall
//     through to the normal LLM/tool loop so instance-scoped billing
//     questions remain answerable.
//
// For other ambiguous mixed-scope phrasing (e.g. "查我账号下哪台实例
// 消费最高"), neither branch fires and the request falls through to
// the LLM, steered by the "## 计费问题口径" system-prompt rule.
func isAccountBillingUnsupportedNormalized(n string) bool {
	// False-positive scoping (#34b, 2026-05-18): if the query mentions a
	// third-party service by name, the question is about THAT service's
	// account/billing state, not CompShare's. P32 v1 captured r16 "ModelVerse
	// 的余额怎么查" being wrongly hard-blocked by the bare 余额 keyword. The
	// corpus covers these third-party services (modelverse 18 chunks etc.),
	// so the planner should run and route to knowledge_qa instead.
	//
	// This check is intentionally whole-message-level (not window-based)
	// because user phrasing varies ("ModelVerse 的余额" / "在哪里看
	// modelverse 余额" / "怎么充 modelverse 余额"). The cost of letting the
	// planner handle a mixed-scope question is far lower than the cost of
	// silently hard-blocking a third-party-service docs query.
	if containsNormalizedKeyword(n, thirdPartyServiceForeignBalanceContexts) {
		return false
	}
	// #52 (2026-05-19): finance FAQ / process-question scope-out.
	// h03 ("我的发票什么时候开") + mq05 ("下载速度突然变慢 是欠费了吗 还是
	// 网络高峰") were 4-mode hard-blocked despite being process/diagnostic
	// questions a FAQ corpus can answer. The exemption fires ONLY when all
	// three conditions hold simultaneously (per brief; user 2026-05-19
	// tightening to prevent over-exemption like "账户余额怎么查"):
	//   (1) hits a finance-FAQ topic word (开票/发票/退款/欠费/到期/续费/
	//       包月规则);
	//   (2) hits a process / diagnostic marker (怎么/什么时候/几天/流程/
	//       是不是/还是/会影响/恢复 etc.);
	//   (3) does NOT hit a realtime-account-data marker (我的/账上/还剩/
	//       开好了吗/进度 — these signal "my specific personal data" and
	//       must keep the hard-block path).
	// `余额` is intentionally classified as account-data (NOT a finance-FAQ
	// topic) so "账户余额怎么查" still hard-blocks under the existing
	// accountOnlyDataKeywords check below. See engine_hardblock_scoping_test.go
	// for the full case table.
	if isFinanceFAQProcessQuestion(n) {
		return false
	}
	if containsNormalizedKeyword(n, accountOnlyDataKeywords) ||
		containsInvoiceRealtimeQuestion(n) ||
		containsRefundRealtimeQuestion(n) ||
		containsArrearsRealtimeQuestion(n) ||
		containsPackageRealtimeQuestion(n) ||
		containsRechargeRealtimeQuestion(n) {
		return true
	}
	if containsNormalizedKeyword(n, monthlyBillKeywords) &&
		containsNormalizedKeyword(n, monthlyAccountCostKeywords) &&
		!containsNormalizedKeyword(n, accountInstanceScopeKeywords) {
		return true
	}
	return false
}

// financeFAQTopicWords are finance-related TOPICS that admit FAQ/process
// answers (invoice issuance schedule, refund process flow, arrears policy,
// expiry rules). Distinct from accountOnlyDataKeywords — `余额` lives there
// because it's account-balance data, not a process topic.
//
// CONSTRAINT (#52, memory `feedback_l0_stop_grow_dictionary`): keep this
// list bounded. The long-term home is a finance-policy guard skill manifest;
// this var is the bridge until that skill ships. Growing it to handle a new
// false-positive case is the wrong fix — surface it as skill schema input.
var financeFAQTopicWords = []string{
	"开票",   // 开票
	"发票",   // 发票
	"退款",   // 退款
	"欠费",   // 欠费
	"到期",   // 到期
	"续费",   // 续费
	"包月规则", // 包月规则
}

// financeProcessMarkerWords are phrasing markers that indicate the user is
// asking about a process / rule / time-window / diagnostic — i.e. content
// a FAQ corpus can answer — rather than asking for the realtime state of
// their own personal record.
var financeProcessMarkerWords = []string{
	"怎么",   // 怎么
	"如何",   // 如何
	"什么时候", // 什么时候
	"多久",   // 多久
	"几天",   // 几天
	"流程",   // 流程
	"规则",   // 规则
	"是不是",  // 是不是
	"还是",   // 还是
	"会影响",  // 会影响
	"影响",   // 影响
	"恢复",   // 恢复
}

// financeRealtimeAccountMarkers are signals the user is asking about their
// own specific record (status / progress / amount remaining). Their presence
// vetoes the financeFAQ exemption — the question goes back through the
// existing account-data check, which routes to hard-block as before.
//
// Note: this is intentionally a "veto list" not an "expand-the-block list".
// Pure account-data words like 余额 / 流水 / 账单明细 don't appear here
// because they already trigger accountOnlyDataKeywords below; this veto
// only handles the personal-status phrasing layer.
var financeRealtimeAccountMarkers = []string{
	"我的",   // 我的
	"我账户",  // 我账户
	"账上",   // 账上
	"我现在",  // 我现在
	"当前状态", // 当前状态
	"开好了吗", // 开好了吗
	"寄了吗",  // 寄了吗
	"进度",   // 进度
	"还剩",   // 还剩
	"剩多少",  // 剩多少
}

// isFinanceFAQProcessQuestion implements the #52 3-condition AND scope-out
// per brief: topic + marker + NOT realtime → exempt from hard-block, letting
// the planner route to knowledge_qa / RAG. See the comment block above
// the call site in isAccountBillingUnsupportedNormalized for context.
func isFinanceFAQProcessQuestion(normalized string) bool {
	if !containsNormalizedKeyword(normalized, financeFAQTopicWords) {
		return false
	}
	if !containsNormalizedKeyword(normalized, financeProcessMarkerWords) {
		return false
	}
	if containsNormalizedKeyword(normalized, financeRealtimeAccountMarkers) {
		return false
	}
	return true
}

// shouldForceMonitorRecall reports whether the current turn is an adjacent
// monitor follow-up that should force a fresh GetCompShareInstanceMonitor call
// instead of letting the LLM reuse prior monitor numbers. Conditions (all must
// hold):
//   - the immediately previous user turn completed GetCompShareInstanceMonitor
//   - the current message contains a curated follow-up keyword
//   - the current message also contains a monitor metric keyword
//
// This is a narrow engine-layer bridge until IntentPlan shadow routing owns
// monitor follow-up classification.
func (e *Engine) shouldForceMonitorRecall(userMsg string) bool {
	if e.lastMonitorTurn < 0 || e.userTurn != e.lastMonitorTurn+1 {
		return false
	}
	n := textutil.Normalize(userMsg)
	return containsAnyKeyword(n, monitorRecallKeywords) && containsAnyKeyword(n, monitorMetricKeywords)
}

func isUnsupportedHistoricalMonitorQuestion(userMsg string) bool {
	n := textutil.Normalize(userMsg)
	if !containsAnyKeyword(n, historicalMonitorSignalKeywords) {
		return false
	}
	if containsAnyKeyword(n, historicalMonitorTimeKeywords) {
		return true
	}
	return clockRangeRE.MatchString(userMsg) ||
		isoDateRE.MatchString(userMsg) ||
		historicalDurationRE.MatchString(userMsg)
}

func containsAnyKeyword(normalized string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(normalized, kw) {
			return true
		}
	}
	return false
}

func containsNormalizedKeyword(normalized string, keywords []string) bool {
	return containsAnyKeyword(normalized, keywords)
}

// inferKnowledgeProductArea returns a product_area label matching one of the
// deploy/kb/stage2b_w0.jsonl product_area values. The match flows into
// Retriever.scoreChunk where chunks with the same productArea get +2 BM25.
// Order matters: more-specific labels (init_failure / windows / driver_cuda)
// are checked before broader ones (image / modelverse / billing_rule) to avoid
// the broader keyword sets shadowing the niche groups.
func inferKnowledgeProductArea(userMsg string) string {
	n := textutil.Normalize(userMsg)
	switch {
	case containsAnyKeyword(n, knowledgeInitFailureKeywords):
		return "init_failure"
	case containsAnyKeyword(n, knowledgeWindowsKeywords):
		return "windows"
	case containsAnyKeyword(n, knowledgeDriverCudaKeywords):
		return "driver_cuda"
	case containsAnyKeyword(n, knowledgeMonitorKeywords):
		return "monitor"
	case containsAnyKeyword(n, knowledgeImageKeywords):
		return "image"
	case containsAnyKeyword(n, knowledgeLoginKeywords):
		return "login"
	case containsAnyKeyword(n, knowledgeResourcePurchaseKeywords):
		return "resource_purchase"
	case containsAnyKeyword(n, knowledgeBillingRuleKeywords):
		return "billing_rule"
	case containsAnyKeyword(n, knowledgeModelverseKeywords):
		return "modelverse"
	default:
		return ""
	}
}

// pickProjectId removed in PR9 with ensureProjectId. See comment block
// at the former ensureProjectId site (search for "PR9 removed").
