package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/prompt"
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
	maxReadExpensiveCallsPerTurn = 20
)

const (
	rateLimitQPSMessage   = "请求过于频繁，请稍后再试。"
	rateLimitDailyMessage = "今日额度已用完，请明天再试。"
)

const (
	toolCapExceededMessage         = "本次最多支持查询 20 台实例，请缩小范围后重试。"
	historyWindowExceededMessage   = "历史监控时间窗最多支持 24 小时，请缩短时间范围后重试。"
	readExpensiveTurnBudgetMessage = "本轮读取类查询次数已达上限，请缩小问题范围后重试。"
)

// Force-tool / hard-block priority chain (highest first):
//
//  1. isAccountBillingUnsupported -> canned reply, no LLM call (hard-block)
//  2. shouldForceMonitorRecall    -> tool_choice=GetCompShareInstanceMonitor
//                                    (BRIDGE T-001.f1, capability-gated)
//  3. (future) f3a resource info follow-up (BRIDGE T-001.f3a, if implemented)
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
	beijingZone  = time.FixedZone("CST", 8*3600)
	isoDateRE    = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	clockRangeRE = regexp.MustCompile(`\b\d{1,2}:\d{2}\s*(?:~|-|到|至)\s*\d{1,2}:\d{2}\b`)
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

type IntentPlannerOptions struct {
	EnabledIntents []intent.Intent
	Model          string
}

// Engine runs the ReAct loop: User → LLM → Tool → LLM → ... → Reply.
type Engine struct {
	llmClient                  LLMClient
	safeExecutor               *tools.SafeToolExecutor
	registry                   *entity.EntityRegistry
	intentPlanner              IntentPlanner
	intentPlannerModel         string
	intentCutoverIntents       map[intent.Intent]struct{}
	knowledgeRetriever         KnowledgeRetriever
	plannerTraceObserver       func(observability.PlannerTrace)
	retrievalTraceObserver     func(observability.RetrievalTrace)
	rateLimiter                governance.RateLimiter
	rateLimitSubject           string
	rateLimitObserver          func(governance.Decision)
	readExpensiveCallsThisTurn int
	hardBlockObserver          func(observability.EngineHardBlockTrace)
	confirmFn                  ConfirmFunc
	messages                   []openai.ChatCompletionMessage // conversation history
	userTurn                   int                            // incremented at start of each Chat() call
	lastInstanceQueryTurn      int                            // set to userTurn on successful DescribeCompShareInstance
	lastMonitorTurn            int                            // set to userTurn on successful GetCompShareInstanceMonitor
	currentMonitorTargets      []string                       // historical monitor targets queried in the current turn
	currentMonitorNoData       []string                       // current-turn historical monitor targets with no data samples
	currentMonitorStart        int64                          // start of the current historical monitor window, if any
	currentMonitorEnd          int64                          // end of the current historical monitor window, if any
	currentMonitorWindow       bool                           // true when currentMonitorStart/End are known
	// supportsObjectToolChoice gates force-tool guards (e.g. shouldForceMonitorRecall)
	// from sending object tool_choice on models that don't support it (notably
	// deepseek-v4-flash in thinking mode, which 400s). When false, guards still
	// run their detection logic but fall through to LLM auto routing.
	supportsObjectToolChoice bool
	// Raw user message for the current turn. Set at the start of Chat().
	// Read by executeDiagnosis guards for signal matching. Never mutated
	// mid-turn.
	lastUserMsg string
}

func New(cfg *config.Config, confirmFn ConfirmFunc) *Engine {
	cap := llm.LookupCapability(cfg.Agent.LLM.BaseURL, cfg.Agent.LLM.Model)
	subject, ok := governance.SubjectKeyFromPublicKey(cfg.Agent.PublicKey)
	if !ok {
		fmt.Fprintln(os.Stderr, "warning: rate limiter using anonymous subject (public key missing)")
	}
	eng := &Engine{
		llmClient:                llm.NewClient(cfg.Agent.LLM),
		confirmFn:                confirmFn,
		registry:                 entity.NewRegistry(),
		// MemoryLimiter is process-local and suitable for local demo or
		// single-instance deployment only. Multi-replica production needs a
		// centralized limiter such as Redis or an API gateway.
		rateLimiter:              governance.NewMemoryLimiter(cfg.Agent.RateLimit.Limits()),
		rateLimitSubject:         subject,
		lastInstanceQueryTurn:    -1,
		lastMonitorTurn:          -1,
		supportsObjectToolChoice: cap.SupportsObjectToolChoice,
	}
	eng.safeExecutor = newSafeToolExecutor(tools.NewExternalExecutor(cfg.Agent), confirmFn)
	return eng
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
		lastInstanceQueryTurn:    -1,
		lastMonitorTurn:          -1,
		supportsObjectToolChoice: true,
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

func (e *Engine) SetIntentPlanner(planner IntentPlanner, opts IntentPlannerOptions) {
	e.intentPlanner = planner
	e.intentPlannerModel = opts.Model
	e.intentCutoverIntents = map[intent.Intent]struct{}{}
	for _, enabled := range opts.EnabledIntents {
		switch enabled {
		case intent.IntentResourceInfo, intent.IntentMonitorQuery:
			e.intentCutoverIntents[enabled] = struct{}{}
		}
	}
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

func (e *Engine) SetRetrievalTraceObserver(observer func(observability.RetrievalTrace)) {
	e.retrievalTraceObserver = observer
}

func (e *Engine) SetRateLimitObserver(observer func(governance.Decision)) {
	e.rateLimitObserver = observer
}

func (e *Engine) RateLimitSubjectKey() string {
	return e.rateLimitSubject
}

func (e *Engine) RateLimitDecision(req governance.Request) governance.Decision {
	decision, _ := e.allowRateLimited(req.Class, req.Action)
	return decision
}

func (e *Engine) SetHardBlockObserver(observer func(observability.EngineHardBlockTrace)) {
	e.hardBlockObserver = observer
}

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
	// Ensure ProjectId is available before any write API may need it
	// (e.g. UpdateCompShareStopScheduler). Silent failure: if discovery
	// fails, scheduler APIs will surface a clear platform-level error.
	e.ensureProjectId(ctx)

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

	systemPrompt := prompt.BuildSystem(userCtx)
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
	systemPrompt := prompt.BuildSystem(userCtx)
	e.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}
}

// Chat processes one user message through the ReAct loop and returns the final text reply.
// The callback is invoked for each intermediate step (tool calls, thinking, etc.).
func (e *Engine) Chat(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, error) {
	e.userTurn++
	e.lastUserMsg = userMsg
	e.readExpensiveCallsThisTurn = 0

	// Trim before appending to guarantee the new user message is never dropped.
	e.trimHistory()
	priorText := e.PlannerPriorTextSnapshot()

	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userMsg,
	})

	if isAccountBillingUnsupported(userMsg) {
		if e.hardBlockObserver != nil {
			e.hardBlockObserver(observability.EngineHardBlockTrace{
				Hit:      true,
				Category: "account_billing_unsupported",
			})
		}
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: accountBillingUnsupportedReply,
		})
		return accountBillingUnsupportedReply, nil
	}

	e.currentMonitorTargets = nil
	e.currentMonitorNoData = nil
	e.currentMonitorStart = 0
	e.currentMonitorEnd = 0
	e.currentMonitorWindow = false

	forceMonitorRecall := e.shouldForceMonitorRecall(userMsg)
	if reply, handled := e.tryPlannerDispatch(ctx, userMsg, priorText, onStep); handled {
		return reply, nil
	}

	for round := 0; round < maxReActRounds; round++ {
		req := llm.ChatRequest{
			Messages: e.buildMessagesForLLM(),
			Tools:    tools.Registry,
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
		resp, err := e.llmClient.Chat(ctx, req)
		if err != nil {
			return "", fmt.Errorf("LLM 调用失败: %w", err)
		}

		// No tool calls → final text reply
		if len(resp.ToolCalls) == 0 {
			content := e.guardMonitorTemporalFinalReply(resp.Content)
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

func (e *Engine) tryPlannerDispatch(ctx context.Context, userMsg, priorText string, onStep func(StepEvent)) (string, bool) {
	if !e.plannerDispatchEnabled() {
		return "", false
	}

	dispatch := e.callPlannerOnce(ctx, userMsg, priorText)
	if status, ok := e.commonPlannerCandidateStatus(dispatch.result); !ok {
		e.emitPlannerTrace(dispatch.result, status, dispatch.latency)
		return "", false
	}

	if dispatch.result.Plan.Intent == intent.IntentResourceInfo || dispatch.result.Plan.Intent == intent.IntentMonitorQuery {
		return e.tryPhase1Cutover(ctx, dispatch, onStep)
	}
	if reply, handled := e.tryStage2BRetrieval(dispatch, userMsg); handled {
		return reply, true
	}
	if dispatch.result.Plan.Intent == intent.IntentKnowledgeQA {
		return "", false
	}

	e.emitPlannerTrace(dispatch.result, intent.CutoverStatusFallbackIneligible, dispatch.latency)
	return "", false
}

func (e *Engine) plannerDispatchEnabled() bool {
	return e != nil && e.intentPlanner != nil &&
		(len(e.intentCutoverIntents) > 0 || e.knowledgeRetriever != nil)
}

func (e *Engine) callPlannerOnce(ctx context.Context, userMsg, priorText string) plannerDispatchResult {
	start := time.Now()
	result := engineFallbackPlannerResult()
	snapshot := e.RegistrySnapshot()
	if _, ok := e.allowRateLimited(governance.ClassLLM, "intent_planner"); ok {
		planned, err := e.intentPlanner.Plan(ctx, intent.PlannerInput{
			UserText:  userMsg,
			PriorText: priorText,
			Resolver:  snapshot,
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

	return plannerDispatchResult{result: result, latency: latency, snapshot: snapshot}
}

func (e *Engine) tryPhase1Cutover(ctx context.Context, dispatch plannerDispatchResult, onStep func(StepEvent)) (string, bool) {
	result := dispatch.result
	if result.Plan.Intent != intent.IntentResourceInfo && result.Plan.Intent != intent.IntentMonitorQuery {
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
	}
	var handled intent.HandlerResult
	switch result.Plan.Intent {
	case intent.IntentResourceInfo:
		handled = handler.HandleResourceInfo(ctx, req)
	case intent.IntentMonitorQuery:
		handled = handler.HandleMonitorQuery(ctx, req)
	default:
		e.emitPlannerTrace(result, intent.CutoverStatusFallbackIneligible, dispatch.latency)
		return "", false
	}

	e.emitPlannerTrace(result, handled.CutoverStatus, dispatch.latency)
	if handled.Status == intent.HandlerStatusFallbackBeforeTool {
		return "", false
	}

	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: handled.Reply,
	})
	return handled.Reply, true
}

func (e *Engine) tryStage2BRetrieval(dispatch plannerDispatchResult, userMsg string) (string, bool) {
	result := dispatch.result
	if result.Plan.Intent != intent.IntentKnowledgeQA {
		return "", false
	}
	if e.knowledgeRetriever == nil {
		e.emitRetrievalTrace(observability.RetrievalTrace{})
		e.emitPlannerTrace(result, intent.CutoverStatusFallbackRetrievalDisabled, dispatch.latency)
		return "", false
	}

	retrieved := e.knowledgeRetriever.Retrieve(userMsg, inferKnowledgeProductArea(userMsg))
	e.emitRetrievalTrace(observability.RetrievalTrace{
		Enabled:   retrieved.Enabled,
		KBVersion: retrieved.KBVersion,
		Hits:      len(retrieved.Hits),
	})
	if retrieved.Empty || len(retrieved.Hits) == 0 {
		e.emitPlannerTrace(result, intent.CutoverStatusFallbackRetrievalMiss, dispatch.latency)
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: knowledge.KnowledgeMissReply,
		})
		return knowledge.KnowledgeMissReply, true
	}

	reply := knowledge.RenderKnowledgeAnswer(retrieved)
	e.emitPlannerTrace(result, intent.CutoverStatusDispatchedRetrieval, dispatch.latency)
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	})
	return reply, true
}

func (e *Engine) commonPlannerCandidateStatus(result intent.PlannerResult) (intent.CutoverStatus, bool) {
	if result.Fallback || result.LastValidationCode != "" ||
		result.Plan.SchemaVersion != intent.SchemaVersion || result.Plan.Intent == "" {
		return intent.CutoverStatusFallbackInvalid, false
	}
	if result.Plan.HardBlockHint {
		return intent.CutoverStatusFallbackHardBlockHint, false
	}
	if result.Plan.Confidence < 0.60 {
		return intent.CutoverStatusFallbackLowConfidence, false
	}
	return intent.CutoverStatusDispatched, true
}

func (e *Engine) phase1CutoverCandidateStatus(result intent.PlannerResult) (intent.CutoverStatus, bool) {
	if result.Plan.Retrieval.Enabled {
		return intent.CutoverStatusFallbackIneligible, false
	}
	if result.Plan.Intent != intent.IntentResourceInfo && result.Plan.Intent != intent.IntentMonitorQuery {
		return intent.CutoverStatusFallbackIneligible, false
	}
	if _, ok := e.intentCutoverIntents[result.Plan.Intent]; !ok {
		return intent.CutoverStatusFallbackIneligible, false
	}
	return intent.CutoverStatusDispatched, true
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
				x.emit(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourcePlannerHandler, Args: args})
			},
		},
	})
	if err != nil {
		if msg, ok := friendlyToolErrorMessage(err); ok {
			x.emit(blockedStepEvent(action, observability.ToolSourcePlannerHandler, args, msg, err))
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

func friendlyToolErrorMessage(err error) (string, bool) {
	var friendly friendlyEngineError
	if errors.As(err, &friendly) {
		return friendly.message, true
	}
	switch {
	case errors.Is(err, tools.ErrHistoryWindowExceeded):
		return historyWindowExceededMessage, true
	case errors.Is(err, tools.ErrToolCapExceeded):
		return toolCapExceededMessage, true
	case errors.Is(err, governance.ErrRateLimited):
		return rateLimitQPSMessage, true
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
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, args, msg, governance.ErrRateLimited))
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
				onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: args})
			},
		},
	})
	if err != nil {
		if msg, ok := friendlyToolErrorMessage(err); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, args, msg, err))
			return friendlyToolResultJSON(msg)
		}
		if errors.Is(err, tools.ErrDestructiveAction) {
			msg := fmt.Sprintf("安全限制：%s 是破坏性操作（L2），已拒绝执行。请到控制台手动操作。", action)
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
			return finalReplyPrefix + msg
		}
		if errors.Is(err, tools.ErrUserDeclined) {
			msg := fmt.Sprintf("操作 %s 已取消（用户未确认）。", action)
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
	}
	return result, err
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
	// Hard guard — instance-operation workflows MUST have a non-empty UHostId.
	// CreateInstanceWorkflow is excluded because it creates a new instance.
	// NOTE: If you add a workflow that does not target an existing instance,
	// add it to this exclusion list. The default is to block (fail-safe).
	if action != "CreateInstanceWorkflow" {
		uHostId, _ := args["UHostId"].(string)
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
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, args, msg, governance.ErrRateLimited))
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
		return finalReplyPrefix + fmt.Sprintf("%s 已取消。", action)
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

	// Vague-failure guard — DiagnoseInitFailure only.
	// Gate 1 (symptom specificity): the user message must contain an
	// init-failure-specific signal. Vague fault language like "跑崩了" /
	// "挂了" is blocked here, even if the LLM provided a target instance.
	// This is a hard safety net behind the prompt-level vague_failure
	// routing class — deliberately does NOT redirect to another Diagnose*.
	if action == "DiagnoseInitFailure" && !containsInitFailureSignal(e.lastUserMsg) {
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
		uid, _ := args["UHostId"].(string)
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

// ensureProjectId makes sure the underlying ExternalExecutor has a ProjectId.
// If config already supplied one, it's a no-op. Otherwise it calls
// GetProjectList and picks the IsDefault project (or the first one available).
// Silent failure: on any error (non-ExternalExecutor, network, malformed
// response), the function returns and leaves ProjectId empty — scheduler
// APIs will then fail with a clear platform-level error that the caller
// can see in the agent reply.
func (e *Engine) ensureProjectId(ctx context.Context) {
	ext := e.externalExecutor()
	if ext == nil {
		return // test executor or other non-external implementation
	}
	if ext.ProjectId() != "" {
		return
	}
	resp, err := e.executeRawTool(ctx, "GetProjectList", nil, tools.OriginDirectLLM)
	if err != nil {
		return
	}
	if id := pickProjectId(resp); id != "" {
		ext.SetProjectId(id)
	}
}

func (e *Engine) externalExecutor() *tools.ExternalExecutor {
	if e.safeExecutor == nil {
		return nil
	}
	return e.safeExecutor.ExternalExecutor()
}

const accountBillingUnsupportedReply = "这类账号级账单、余额和消费流水信息当前不支持由助手查询。请到控制台的财务中心查看：账户总览看余额，账单管理看月度账单，消费记录看扣费流水。"

var monthlyBillKeywords = []string{
	"\u672c\u6708", // 本月
	"\u6708\u5ea6", // 月度
	"\u5f53\u6708", // 当月
}

// accountOnlyDataKeywords are signals that ONLY the account financial
// center can satisfy. Their presence triggers the hard-block regardless
// of instance words elsewhere in the message.
var accountOnlyDataKeywords = []string{
	"\u4f59\u989d",             // 余额
	"\u603b\u8d26\u5355",       // 总账单
	"\u6d88\u8d39\u6d41\u6c34", // 消费流水
	"\u6d41\u6c34",             // 流水
	"balance",
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

var knowledgeBillingKeywords = []string{
	"billing", "bill", "charge", "cost", "fee", "price", "balance",
	"\u8ba1\u8d39", "\u6263\u8d39", "\u6536\u8d39", "\u8d26\u5355", "\u4f59\u989d", "\u8d39\u7528", "\u4ef7\u683c",
}

var knowledgeImageKeywords = []string{
	"image", "images", "\u955c\u50cf",
}

var knowledgeLoginKeywords = []string{
	"login", "ssh", "jupyter", "jupyterlab", "token", "password",
	"\u767b\u5f55", "\u8fde\u63a5", "\u5bc6\u7801", "\u53e3\u4ee4",
}

var knowledgeNetworkKeywords = []string{
	"port", "ports", "firewall", "network", "\u8bbf\u95ee", "\u7aef\u53e3", "\u9632\u706b\u5899", "\u7f51\u7edc",
}

var knowledgeModelSuiteKeywords = []string{
	"model", "models", "claude", "anthropic", "credit", "credits",
	"\u6a21\u578b", "\u5957\u9910", "\u79ef\u5206",
}

// normalizeMsg standardizes a user message for signal matching:
// trims whitespace, collapses internal whitespace runs to a single space,
// and lowercases ASCII letters. CJK characters are preserved as-is.
// The returned value is used only for substring matching; the caller's
// original string is never mutated.
func normalizeMsg(s string) string {
	var b strings.Builder
	prevSpace := true // treat start as space so leading whitespace collapses
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	out := b.String()
	return strings.TrimRight(out, " ")
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
	n := normalizeMsg(msg)
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
	n := normalizeMsg(msg)
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
	return isAccountBillingUnsupportedNormalized(normalizeMsg(userMsg))
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
	if containsNormalizedKeyword(n, accountOnlyDataKeywords) {
		return true
	}
	if containsNormalizedKeyword(n, monthlyBillKeywords) &&
		containsNormalizedKeyword(n, monthlyAccountCostKeywords) &&
		!containsNormalizedKeyword(n, accountInstanceScopeKeywords) {
		return true
	}
	return false
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
	n := normalizeMsg(userMsg)
	return containsAnyKeyword(n, monitorRecallKeywords) && containsAnyKeyword(n, monitorMetricKeywords)
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

func inferKnowledgeProductArea(userMsg string) string {
	n := normalizeMsg(userMsg)
	switch {
	case containsAnyKeyword(n, knowledgeBillingKeywords):
		return "billing"
	case containsAnyKeyword(n, knowledgeImageKeywords):
		return "image"
	case containsAnyKeyword(n, knowledgeLoginKeywords):
		return "login"
	case containsAnyKeyword(n, knowledgeNetworkKeywords):
		return "network"
	case containsAnyKeyword(n, knowledgeModelSuiteKeywords):
		return "model_suite"
	default:
		return ""
	}
}

// pickProjectId extracts a ProjectId from a GetProjectList response.
// Prefers the IsDefault=true entry; falls back to the first non-empty
// ProjectId in ProjectSet.
func pickProjectId(resp map[string]any) string {
	if resp == nil {
		return ""
	}
	set, ok := resp["ProjectSet"].([]any)
	if !ok {
		return ""
	}
	var fallback string
	for _, item := range set {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := p["ProjectId"].(string)
		if id == "" {
			continue
		}
		if def, _ := p["IsDefault"].(bool); def {
			return id
		}
		if fallback == "" {
			fallback = id
		}
	}
	return fallback
}
