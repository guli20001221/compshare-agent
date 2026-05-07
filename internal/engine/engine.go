package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/sanitizer"
	"github.com/compshare-agent/internal/security"
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
)

// ConfirmFunc asks the user to confirm an L1 operation. Returns true if confirmed.
type ConfirmFunc func(action string, args map[string]any) bool

// LLMClient abstracts the LLM chat interface for testability.
type LLMClient interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

// Engine runs the ReAct loop: User → LLM → Tool → LLM → ... → Reply.
type Engine struct {
	llmClient             LLMClient
	executor              tools.ToolExecutor
	confirmFn             ConfirmFunc
	messages              []openai.ChatCompletionMessage // conversation history
	userTurn              int                            // incremented at start of each Chat() call
	lastInstanceQueryTurn int                            // set to userTurn on successful DescribeCompShareInstance
	lastMonitorQueryTurn  int                            // set to userTurn on successful GetCompShareInstanceMonitor
	lastMonitorTargets    []string                       // UHostIds extracted from the last monitor query
	currentMonitorTargets []string                       // historical monitor targets queried in the current turn
	currentMonitorNoData  []string                       // current-turn historical monitor targets with no data samples
	// Diagnosis follow-up tracking (narrow, only DiagnoseBilling for now).
	// Updated after a successful executeDiagnosis run; read at the start of
	// the next Chat() to decide whether to force DiagnoseBilling via tool_choice.
	lastDiagnosisTool    string   // empty until a tracked diagnosis completes
	lastDiagnosisTurn    int      // init -1; set to userTurn when tracked diagnosis runs
	lastDiagnosisTargets []string // target strings extracted from diagnosis args (UHostId, Name)
	// Raw user message for the current turn. Set at the start of Chat().
	// Read by executeDiagnosis guards for signal matching. Never mutated
	// mid-turn.
	lastUserMsg string
	// Static system prompt body (without the per-turn time prefix).
	// Set in Init/InitWithContext; read by refreshSystemMessage.
	systemPromptBase string
	// Clock injection point. Defaults to time.Now in constructors.
	// Tests override to assert deterministic time-prefix injection.
	nowFn func() time.Time
}

func New(cfg *config.Config, confirmFn ConfirmFunc) *Engine {
	eng := &Engine{
		llmClient:             llm.NewClient(cfg.Agent.LLM),
		confirmFn:             confirmFn,
		lastInstanceQueryTurn: -1,
		lastMonitorQueryTurn:  -1,
		lastDiagnosisTurn:     -1,
		nowFn:                 time.Now,
	}
	eng.executor = &freshnessTracker{inner: tools.NewExternalExecutor(cfg.Agent), engine: eng}
	return eng
}

// NewWithDeps creates an Engine with injected dependencies (for testing).
func NewWithDeps(client LLMClient, executor tools.ToolExecutor, confirmFn ConfirmFunc) *Engine {
	eng := &Engine{
		llmClient:             client,
		confirmFn:             confirmFn,
		lastInstanceQueryTurn: -1,
		lastMonitorQueryTurn:  -1,
		lastDiagnosisTurn:     -1,
		nowFn:                 time.Now,
	}
	eng.executor = &freshnessTracker{inner: executor, engine: eng}
	return eng
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
	result, err := e.executor.Execute(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100})
	if err != nil {
		// Context injection is best-effort; continue with default context.
		_ = err
	} else {
		userCtx = prompt.FormatInstanceContext(result)
	}

	e.systemPromptBase = prompt.BuildSystem(userCtx)
	e.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: e.composeSystemContent()},
	}

	// Determine suggestions based on user state
	stage := prompt.NewUser
	if err == nil {
		stage = prompt.ClassifyUser(result)
	}
	return prompt.GetSuggestions(stage), nil
}

// InitWithContext performs context injection with a pre-built user context string,
// bypassing the DescribeCompShareInstance API call. Used for testing.
func (e *Engine) InitWithContext(userCtx string) {
	e.systemPromptBase = prompt.BuildSystem(userCtx)
	e.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: e.composeSystemContent()},
	}
}

// beijingZone is the wall-clock zone used for the per-turn time prefix.
// Hoisted to package level to avoid re-allocating a FixedZone on every
// Chat() call; the label is purely cosmetic since composeSystemContent
// formats without a zone abbreviation.
var beijingZone = time.FixedZone("CST", 8*3600)

// timePrefixFormat is the literal prefix written into messages[0]. Tests
// pin against this so prompt-side renames are caught.
const timePrefixFormat = "当前北京时间：%s\n\n%s"

// composeSystemContent prepends the current Beijing wall-clock time to the
// stored system prompt body. The clock is read each call so per-turn
// refreshes pick up the latest time. Returns the body unchanged if the
// engine has no nowFn (defensive; constructors always set one).
func (e *Engine) composeSystemContent() string {
	if e.nowFn == nil {
		return e.systemPromptBase
	}
	timeStr := e.nowFn().In(beijingZone).Format("2006-01-02 15:04")
	return fmt.Sprintf(timePrefixFormat, timeStr, e.systemPromptBase)
}

// refreshSystemMessage rewrites messages[0] in place with the current time
// prefix. No-op if the engine has not been initialized (systemPromptBase
// empty) or if messages[0] is not a system message (defensive).
func (e *Engine) refreshSystemMessage() {
	if e.systemPromptBase == "" {
		return
	}
	if len(e.messages) == 0 || e.messages[0].Role != openai.ChatMessageRoleSystem {
		return
	}
	e.messages[0].Content = e.composeSystemContent()
}

// Chat processes one user message through the ReAct loop and returns the final text reply.
// The callback is invoked for each intermediate step (tool calls, thinking, etc.).
func (e *Engine) Chat(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, error) {
	e.userTurn++
	e.lastUserMsg = userMsg
	e.currentMonitorTargets = nil
	e.currentMonitorNoData = nil

	// Refresh wall-clock prefix in messages[0] so absolute-time queries
	// like "昨天下午 2 点" anchor against the current real time, not the
	// session-start time.
	e.refreshSystemMessage()

	// Trim before appending to guarantee the new user message is never dropped.
	e.trimHistory()

	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userMsg,
	})

	if isAccountBillingUnsupported(userMsg) {
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: accountBillingUnsupportedReply,
		})
		return accountBillingUnsupportedReply, nil
	}

	forceBilling := e.shouldForceBillingDiagnosis(userMsg)
	forceMonitor := e.shouldForceMonitorRefresh(userMsg)
	forceMonitorDiscovery := e.shouldForceMonitorInstanceDiscovery(userMsg)
	forceResourceInfoDiscovery := shouldForceResourceInfoDiscovery(userMsg)
	forceMixedMonitorBilling := shouldForceMixedMonitorBilling(userMsg)
	forceMonitorAfterDiscovery := false
	forceBillingAfterMonitor := false

	for round := 0; round < maxReActRounds; round++ {
		req := llm.ChatRequest{
			Messages: e.buildMessagesForLLM(),
			Tools:    tools.Registry,
		}
		// Hard guard: billing-diagnosis follow-up in the immediately next turn
		// must re-run DiagnoseBilling instead of reusing the prior conclusion.
		// Scope: first LLM call of this turn only; subsequent ReAct rounds
		// narrate the fresh tool result freely.
		if round == 0 && forceMixedMonitorBilling {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "DescribeCompShareInstance"},
			}
		} else if round == 0 && forceBilling {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "DiagnoseBilling"},
			}
		} else if forceBillingAfterMonitor {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "DiagnoseBilling"},
			}
			forceBillingAfterMonitor = false
		} else if forceMonitorAfterDiscovery {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "GetCompShareInstanceMonitor"},
			}
			forceMonitorAfterDiscovery = false
		} else if round == 0 && forceMonitor {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "GetCompShareInstanceMonitor"},
			}
		} else if round == 0 && forceMonitorDiscovery {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "DescribeCompShareInstance"},
			}
		} else if round == 0 && forceResourceInfoDiscovery {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "DescribeCompShareInstance"},
			}
		}
		resp, err := e.llmClient.Chat(ctx, req)
		if err != nil {
			return "", fmt.Errorf("LLM 调用失败: %w", err)
		}

		// No tool calls → final text reply
		if len(resp.ToolCalls) == 0 {
			content := e.guardMonitorTemporalFinalReply(resp.Content)
			if e.shouldContinueHistoricalMonitorAfterDiscovery(content) {
				forceMonitorAfterDiscovery = true
				continue
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
			if forceMixedMonitorBilling {
				switch tc.Function.Name {
				case "DescribeCompShareInstance":
					forceMonitorAfterDiscovery = true
				case "GetCompShareInstanceMonitor":
					forceBillingAfterMonitor = true
				}
			}

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
	rawArgs := strings.TrimSpace(tc.Function.Arguments)
	if rawArgs == "" {
		rawArgs = "{}"
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		errMsg := fmt.Sprintf("parameter parse error: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: errMsg})
		return errMsg
	}

	// Knowledge tools execute locally — no API call, no security check needed
	if knowledge.IsKnowledgeTool(action) {
		args = filterAllowedParams(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Args: args})
		result, err := knowledge.ExecuteTool(action, args)
		if err != nil {
			errMsg := fmt.Sprintf("知识查询失败: %v", err)
			onStep(StepEvent{Type: StepError, Action: action, Message: errMsg})
			return errMsg
		}
		onStep(StepEvent{Type: StepToolResult, Action: action, Message: "查询成功"})
		return knowledge.ResultToJSON(result)
	}

	// Workflow meta-tools → delegate to workflow engine.
	// Security: LLM-provided args are filtered here before entering the workflow.
	// Workflow steps bypass per-tool L1 checks because step definitions are hardcoded
	// (not LLM-controlled) and each workflow has its own Confirm step for user approval.
	// Invariant: BuildArgs functions must only reference specific named keys from wfCtx.Params.
	if workflow.IsWorkflowTool(action) {
		args = filterAllowedParams(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Args: sanitizer.SanitizeArgs(action, args)})
		return e.executeWorkflow(ctx, action, args, onStep)
	}

	// Diagnosis meta-tools → delegate to diagnosis engine.
	if diagnosis.IsDiagnosisTool(action) {
		args = filterAllowedParams(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Args: sanitizer.SanitizeArgs(action, args)})
		return e.executeDiagnosis(ctx, action, args, onStep)
	}

	// External API tools: security check
	level, err := security.Check(action)
	if err != nil {
		onStep(StepEvent{Type: StepError, Action: action, Message: err.Error()})
		return fmt.Sprintf("错误: %s", err.Error())
	}

	if level == security.L2 {
		msg := fmt.Sprintf("安全限制：%s 是破坏性操作（L2），已拒绝执行。请到控制台手动操作。", action)
		onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
		return finalReplyPrefix + msg
	}

	// L1: must get user confirmation before executing
	if level == security.L1 {
		onStep(StepEvent{Type: StepConfirmNeeded, Action: action, Args: sanitizer.SanitizeArgs(action, args), Message: "此操作需要您确认"})
		if e.confirmFn == nil || !e.confirmFn(action, args) {
			msg := fmt.Sprintf("操作 %s 已取消（用户未确认）。", action)
			onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
			return finalReplyPrefix + msg
		}
	}

	// Strip parameters not in the tool schema to prevent LLM injection
	args = filterAllowedParams(action, args)

	e.normalizeMonitorTimeArgs(action, args)

	if msg, ok := e.blockInvalidMonitorBatchHistory(action, args); ok {
		onStep(StepEvent{Type: StepBlocked, Action: action, Args: sanitizer.SanitizeArgs(action, args), Message: msg})
		guardResult := map[string]any{
			"success": false,
			"code":    "MonitorHistoryBatchNotSupported",
			"message": msg,
		}
		return prompt.FormatToolResult(guardResult)
	}

	onStep(StepEvent{Type: StepToolCall, Action: action, Args: args})

	// Execute external API
	result, err := e.executor.Execute(ctx, action, args)
	if err != nil {
		errMsg := fmt.Sprintf("API 调用失败: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: errMsg})
		return errMsg
	}
	e.trackMonitorResult(action, args, result)

	// Dual-channel sanitization: extract display-only content before redacting
	var display string
	if action == "DescribeCompShareJupyterToken" {
		if token := sanitizer.ExtractJupyterToken(result); token != "" {
			display = fmt.Sprintf("Jupyter Token: %s", token)
		}
	}

	// Sanitize before sending to LLM context
	sanitized := sanitizer.Sanitize(action, result)
	e.enrichToolResultForCurrentIntent(action, sanitized)
	formatted := prompt.FormatToolResult(sanitized)
	onStep(StepEvent{Type: StepToolResult, Action: action, Message: "调用成功", Display: display})
	return formatted
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
			onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
			guardResult := map[string]any{"success": false, "message": msg}
			b, _ := json.Marshal(guardResult)
			return string(b)
		}
	}

	wf, ok := workflow.GetWorkflow(action)
	if !ok {
		msg := fmt.Sprintf("未知的工作流: %s", action)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
		return msg
	}

	var wfConfirm workflow.ConfirmFunc
	if e.confirmFn != nil {
		wfConfirm = workflow.ConfirmFunc(e.confirmFn)
	}

	wfEngine := workflow.NewEngine(e.executor, wfConfirm, func(ev workflow.StepEvent) {
		eventType := StepToolCall
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
		case "success":
			if ev.Type == workflow.StepToolCall {
				eventType = StepToolResult
			}
		}
		onStep(StepEvent{
			Type:    eventType,
			Action:  ev.Tool,
			Args:    sanitizer.SanitizeArgs(ev.Tool, ev.Args),
			Message: fmt.Sprintf("[%d/%d] %s: %s", ev.StepIndex+1, ev.Total, ev.StepName, ev.Status),
		})
	})

	result, err := wfEngine.Run(ctx, wf, args)
	if err != nil {
		msg := fmt.Sprintf("工作流执行错误: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
		return msg
	}

	// User-cancelled workflows return a deterministic reply directly
	if !result.Success && result.Message == "用户取消了操作" {
		return finalReplyPrefix + fmt.Sprintf("%s 已取消。", action)
	}

	b, _ := json.Marshal(result)
	return string(b)
}

// executeDiagnosis runs a diagnostic chain and returns the result as JSON.
func (e *Engine) executeDiagnosis(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	chain, ok := diagnosis.GetChain(action)
	if !ok {
		msg := fmt.Sprintf("未知的诊断链: %s", action)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
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
		onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
		return finalReplyPrefix + msg
	}

	// Gate 2 (instance disambiguation): symptom is specific, but if no
	// target was provided and the user did not ask for a scan-all, ask
	// which instance. Avoids implicit scan-all when the user has a
	// specific instance in mind but didn't name it.
	//
	// Target check is UHostId-only because filterAllowedParams upstream
	// strips any field not in the DiagnoseInitFailure schema (which only
	// declares UHostId). The LLM is expected to resolve names to UHostIds
	// upstream; if it doesn't, this gate correctly falls through to
	// clarification.
	if action == "DiagnoseInitFailure" {
		uid, _ := args["UHostId"].(string)
		if uid == "" && !containsScanAllSignal(e.lastUserMsg) {
			msg := "请问是哪台实例的初始化失败了？"
			onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
			return finalReplyPrefix + msg
		}
	}

	diagEngine := diagnosis.NewEngine(e.executor, func(ev diagnosis.DiagEvent) {
		var eventType StepType
		switch ev.Status {
		case "running":
			eventType = StepToolCall
		case "failed":
			eventType = StepError
		default: // "checked", "concluded"
			eventType = StepToolResult
		}
		onStep(StepEvent{
			Type:    eventType,
			Action:  ev.Tool,
			Args:    sanitizer.SanitizeArgs(ev.Tool, ev.Args),
			Message: fmt.Sprintf("[诊断 %d/%d] %s: %s", ev.StepIndex+1, ev.Total, ev.StepName, ev.Status),
		})
	})

	result, err := diagEngine.Run(ctx, chain, args)
	if err != nil {
		msg := fmt.Sprintf("诊断执行错误: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
		return msg
	}

	// Narrow tracking: record DiagnoseBilling completion for next-turn stale
	// follow-up detection. Other Diagnose* chains are not tracked yet — extend
	// deliberately once this guard proves out on billing.
	if action == "DiagnoseBilling" {
		e.lastDiagnosisTool = action
		e.lastDiagnosisTurn = e.userTurn
		e.lastDiagnosisTargets = extractDiagnosisTargets(args)
	}

	b, _ := json.Marshal(result)
	return string(b)
}

// filterAllowedParams strips parameters not defined in the tool registry schema.
// This prevents the LLM from injecting extra fields into API calls.
func filterAllowedParams(action string, args map[string]any) map[string]any {
	allowed := getAllowedParams(action)
	if allowed == nil {
		return args // unknown tool, pass through (will be caught by security check)
	}
	filtered := make(map[string]any, len(allowed))
	for _, key := range allowed {
		if v, ok := args[key]; ok {
			filtered[key] = v
		}
	}
	return filtered
}

// getAllowedParams extracts parameter names from the tool registry.
func getAllowedParams(action string) []string {
	for _, tool := range tools.Registry {
		if tool.Function == nil || tool.Function.Name != action {
			continue
		}
		params, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			return nil
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			return nil
		}
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		return keys
	}
	return nil
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
	Type    StepType
	Action  string
	Args    map[string]any
	Message string
	Display string // content for CLI display only (not sent to LLM), e.g. raw JupyterToken
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
// Temporary system notes are inserted right before the latest user message so
// high-priority per-turn facts stay close to the ask. These notes are NOT
// persisted in e.messages.
func (e *Engine) buildMessagesForLLM() []openai.ChatCompletionMessage {
	notes := e.ephemeralSystemNotesForLLM()
	if len(notes) == 0 {
		return e.messages
	}
	// Insert notes immediately before the latest user message. This is much
	// higher attention than burying them near the main system prompt in a long
	// conversation.
	lastUserIdx := -1
	for i := len(e.messages) - 1; i >= 0; i-- {
		if e.messages[i].Role == openai.ChatMessageRoleUser {
			lastUserIdx = i
			break
		}
	}
	// Fallback: no user message in history (shouldn't happen in the ReAct
	// loop, but keep the helper total). Append at end.
	if lastUserIdx < 0 {
		msgs := make([]openai.ChatCompletionMessage, len(e.messages), len(e.messages)+len(notes))
		copy(msgs, e.messages)
		return append(msgs, notes...)
	}
	msgs := make([]openai.ChatCompletionMessage, 0, len(e.messages)+len(notes))
	msgs = append(msgs, e.messages[:lastUserIdx]...)
	msgs = append(msgs, notes...)
	msgs = append(msgs, e.messages[lastUserIdx:]...)
	return msgs
}

func (e *Engine) ephemeralSystemNotesForLLM() []openai.ChatCompletionMessage {
	var notes []openai.ChatCompletionMessage
	if e.lastInstanceQueryTurn >= 0 && e.lastInstanceQueryTurn < e.userTurn {
		notes = append(notes, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: staleStateNote,
		})
	}
	if note, ok := e.monitorTemporalContextNote(); ok {
		notes = append(notes, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: note,
		})
	}
	return notes
}

func (e *Engine) monitorTemporalContextNote() (string, bool) {
	start, end, ok := e.monitorTemporalWindowForCurrentTurn()
	if !ok {
		return "", false
	}
	return formatMonitorTemporalContextNote(start, end), true
}

func (e *Engine) monitorTemporalWindowForCurrentTurn() (int64, int64, bool) {
	latest, ok := e.latestUserMessage()
	if !ok {
		return 0, 0, false
	}
	if start, end, ok := e.inferMonitorWindowFromMessage(latest); ok {
		return start, end, true
	}
	n := normalizeMsg(latest)
	if !containsNormalizedKeyword(n, monitorSelectionFollowUpKeywords) {
		return 0, 0, false
	}
	if start, end, ok := e.inferMonitorWindowFromPreviousUserMessages(); ok {
		return start, end, true
	}
	return 0, 0, false
}

func (e *Engine) latestUserMessage() (string, bool) {
	for i := len(e.messages) - 1; i >= 0; i-- {
		if e.messages[i].Role == openai.ChatMessageRoleUser {
			return e.messages[i].Content, true
		}
	}
	return "", false
}

func (e *Engine) inferMonitorWindowFromPreviousUserMessages() (int64, int64, bool) {
	checkedUsers := 0
	skipLatest := true
	for i := len(e.messages) - 1; i >= 0 && checkedUsers < 4; i-- {
		msg := e.messages[i]
		if msg.Role != openai.ChatMessageRoleUser {
			continue
		}
		if skipLatest {
			skipLatest = false
			continue
		}
		checkedUsers++
		if start, end, ok := e.inferMonitorWindowFromMessage(msg.Content); ok {
			return start, end, true
		}
	}
	return 0, 0, false
}

func formatMonitorTemporalContextNote(start, end int64) string {
	startText := time.Unix(start, 0).In(beijingZone).Format("2006-01-02 15:04:05")
	endText := time.Unix(end, 0).In(beijingZone).Format("2006-01-02 15:04:05")
	return fmt.Sprintf("注意：本轮监控查询的相对时间已由系统解析为北京时间 %s 至 %s（StartTime=%d, EndTime=%d）。回答、追问和工具参数必须使用这个时间窗口，不要自行重新推算日期或输出其他日期。", startText, endText, start, end)
}

func (e *Engine) guardMonitorTemporalFinalReply(content string) string {
	start, end, ok := e.monitorTemporalWindowForCurrentTurn()
	if !ok || content == "" {
		return content
	}
	if e.allCurrentHistoricalMonitorResultsNoData() {
		return formatHistoricalMonitorNoDataReply(start, end, e.currentMonitorNoData)
	}
	startAt := time.Unix(start, 0).In(beijingZone)
	endAt := time.Unix(end, 0).In(beijingZone)
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

func (e *Engine) trackMonitorResult(action string, args map[string]any, result map[string]any) {
	if action != "GetCompShareInstanceMonitor" || !hasMonitorTimeRangeArgs(args) {
		return
	}
	targets := extractMonitorTargets(args)
	e.currentMonitorTargets = append(e.currentMonitorTargets, targets...)
	if !monitorResultHasSamples(result) {
		e.currentMonitorNoData = append(e.currentMonitorNoData, targets...)
		result["MonitorDataStatus"] = "NO_DATA_IN_REQUESTED_WINDOW"
		result["MonitorDataGuidance"] = "该请求时间窗没有返回有效监控采样点；不要使用当前实时数据替代，也不要编造 CPU/内存/GPU 数值。"
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

func monitorResultHasSamples(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "Value" && val != nil {
				return true
			}
			if monitorResultHasSamples(val) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if monitorResultHasSamples(item) {
				return true
			}
		}
	}
	return false
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

func (e *Engine) enrichToolResultForCurrentIntent(action string, result map[string]any) {
	if action != "DescribeCompShareInstance" || !shouldForceResourceInfoDiscovery(e.lastUserMsg) {
		return
	}
	if summary := formatResourceInfoSummary(result); summary != "" {
		result["ResourceInfoSummary"] = summary
	}
}

func formatResourceInfoSummary(result map[string]any) string {
	hosts, ok := result["UHostSet"].([]any)
	if !ok || len(hosts) == 0 {
		return ""
	}
	const maxSummaryHosts = 100
	limit := len(hosts)
	if limit > maxSummaryHosts {
		limit = maxSummaryHosts
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Instance resource info summary (%d instances):\n", len(hosts))
	for i := 0; i < limit; i++ {
		host, ok := hosts[i].(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- Name=%s UHostId=%s State=%s GpuType=%s GPU=%s ChargeType=%s ExpireTime=%s AutoRenew=%s\n",
			scalarString(host["Name"]),
			scalarString(host["UHostId"]),
			scalarString(host["State"]),
			scalarString(host["GpuType"]),
			scalarString(host["GPU"]),
			scalarString(host["ChargeType"]),
			scalarString(host["ExpireTime"]),
			scalarString(host["AutoRenew"]),
		)
	}
	if len(hosts) > limit {
		fmt.Fprintf(&b, "- ... omitted %d more instances\n", len(hosts)-limit)
	}
	return b.String()
}

func scalarString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// freshnessTracker wraps a ToolExecutor to track when instance state was last
// successfully queried. When DescribeCompShareInstance returns without error,
// it updates the engine's lastInstanceQueryTurn. This works uniformly across
// direct API calls, workflow internals, and diagnosis internals.
type freshnessTracker struct {
	inner  tools.ToolExecutor
	engine *Engine
}

func (t *freshnessTracker) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	result, err := t.inner.Execute(ctx, action, args)
	if err != nil {
		return result, err
	}
	switch action {
	case "DescribeCompShareInstance":
		t.engine.lastInstanceQueryTurn = t.engine.userTurn
	case "GetCompShareInstanceMonitor":
		t.engine.lastMonitorQueryTurn = t.engine.userTurn
		t.engine.lastMonitorTargets = extractMonitorTargets(args)
	}
	return result, err
}

// ensureProjectId makes sure the underlying ExternalExecutor has a ProjectId.
// If config already supplied one, it's a no-op. Otherwise it calls
// GetProjectList and picks the IsDefault project (or the first one available).
// Silent failure: on any error (non-ExternalExecutor, network, malformed
// response), the function returns and leaves ProjectId empty — scheduler
// APIs will then fail with a clear platform-level error that the caller
// can see in the agent reply.
func (e *Engine) ensureProjectId(ctx context.Context) {
	ext := unwrapExternalExecutor(e.executor)
	if ext == nil {
		return // test executor or other non-external implementation
	}
	if ext.ProjectId() != "" {
		return
	}
	resp, err := e.executor.Execute(ctx, "GetProjectList", nil)
	if err != nil {
		return
	}
	if id := pickProjectId(resp); id != "" {
		ext.SetProjectId(id)
	}
}

// unwrapExternalExecutor peels off wrapper layers (e.g. freshnessTracker) to
// find the underlying *tools.ExternalExecutor. Returns nil if the chain
// doesn't terminate in an ExternalExecutor (e.g. tests with mock executors).
func unwrapExternalExecutor(exec tools.ToolExecutor) *tools.ExternalExecutor {
	if t, ok := exec.(*freshnessTracker); ok {
		return unwrapExternalExecutor(t.inner)
	}
	if ext, ok := exec.(*tools.ExternalExecutor); ok {
		return ext
	}
	return nil
}

// billingFollowUpKeywords is a narrow, single-domain keyword list used only
// to detect billing-diagnosis follow-up phrasing when the user did not
// repeat the instance name/id. Do not widen to general-purpose intent NLU.
var billingFollowUpKeywords = []string{
	"扣费",
	"费用",
	"计费",
	"还在",
	"为什么还",
}

// monitorRefreshKeywords is intentionally narrow. It only detects an
// adjacent follow-up about live resource metrics after a monitor query already
// succeeded; it is not a general intent classifier.
var monitorRefreshKeywords = []string{
	"\u76d1\u63a7", // 监控
	"cpu",
	"gpu",
	"memory",
	"vram",
	"\u5185\u5b58",       // 内存
	"\u663e\u5b58",       // 显存
	"\u5229\u7528\u7387", // 利用率
	"\u4f7f\u7528\u7387", // 使用率
	"\u5360\u7528",       // 占用
	"\u5360\u6ee1",       // 占满
	"\u5f02\u5e38",       // 异常
	"\u7a7a\u95f2",       // 空闲
	"\u8d8b\u52bf",       // 趋势
}

var monitorHistoryTimeKeywords = []string{
	"\u6628\u5929",       // 昨天
	"\u524d\u5929",       // 前天
	"\u4eca\u5929",       // 今天
	"\u8fc7\u53bb",       // 过去
	"\u4e0a\u5468",       // 上周
	"\u672c\u5468",       // 本周
	"\u4e0a\u5348",       // 上午
	"\u4e0b\u5348",       // 下午
	"\u4e2d\u5348",       // 中午
	"\u665a\u4e0a",       // 晚上
	"\u5c0f\u65f6",       // 小时
	"\u5206\u949f",       // 分钟
	"\u65f6\u95f4\u6bb5", // 时间段
	"14:00",
	"2\u70b9",      // 2点
	"\u4e24\u70b9", // 两点
}

var monitorSelectionFollowUpKeywords = []string{
	"\u5168\u90e8", // 全部
	"\u5168\u90fd", // 全都
	"\u90fd\u67e5", // 都查
	"\u90fd\u770b", // 都看
	"\u8fd9\u4e9b", // 这些
	"\u90a3\u4e9b", // 那些
	"\u4e0a\u9762", // 上面
	"\u5019\u9009", // 候选
}

var monitorNeedlessConfirmationKeywords = []string{
	"\u9700\u8981\u6211\u7ee7\u7eed", // 需要我继续
	"\u8981\u6211\u7ee7\u7eed",       // 要我继续
	"\u662f\u5426\u7ee7\u7eed",       // 是否继续
	"\u7ee7\u7eed\u67e5",             // 继续查
	"\u7ee7\u7eed\u67e5\u8be2",       // 继续查询
	"\u786e\u8ba4\u4e00\u4e0b",       // 确认一下
	"\u5bf9\u5427",                   // 对吧
	"\u54ea\u53f0",                   // 哪台
	"\u54ea\u4e00\u53f0",             // 哪一台
	"\u5177\u4f53\u770b",             // 具体看
	"\u7279\u5b9a\u54ea\u53f0",       // 特定哪台
}

var monitorNoCandidateKeywords = []string{
	"\u6ca1\u6709\u8fd0\u884c\u4e2d", // 没有运行中
	"\u6ca1\u6709\u5339\u914d",       // 没有匹配
	"\u672a\u627e\u5230",             // 未找到
	"\u65e0\u7b26\u5408",             // 无符合
}

var monitorPrimaryIntentKeywords = []string{
	"\u76d1\u63a7", // 监控
}

var monitorMetricSubjectKeywords = []string{
	"cpu",
	"gpu",
	"memory",
	"vram",
	"\u5185\u5b58", // 内存
	"\u663e\u5b58", // 显存
}

var monitorMetricAskKeywords = []string{
	"\u5360\u7528",       // 占用
	"\u5360\u6ee1",       // 占满
	"\u4f7f\u7528\u7387", // 使用率
	"\u5229\u7528\u7387", // 利用率
	"\u5f02\u5e38",       // 异常
	"\u7a7a\u95f2",       // 空闲
	"\u8dd1\u6ee1",       // 跑满
	"\u8fc7\u9ad8",       // 过高
}

var resourceInfoDiscoveryKeywords = []string{
	"\u5230\u671f",             // 到期
	"\u7eed\u8d39",             // 续费
	"\u81ea\u52a8\u7eed\u8d39", // 自动续费
	"expiretime",
	"autorenew",
}

var monitorReuseKeywords = []string{
	"\u57fa\u4e8e\u521a\u624d", // 基于刚才
	"\u6839\u636e\u521a\u624d", // 根据刚才
	"\u521a\u624d\u7684",       // 刚才的
	"\u4e0a\u9762\u7684",       // 上面的
	"\u4e0a\u6b21",             // 上次
	"\u4e0d\u7528\u91cd\u65b0", // 不用重新
	"\u4e0d\u8981\u91cd\u65b0", // 不要重新
	"\u522b\u91cd\u65b0",       // 别重新
	"\u65e0\u9700\u91cd\u65b0", // 无需重新
}

var accountBillingKeywords = []string{
	"\u8d26\u53f7", // 账号
	"\u8d26\u5355", // 账单
	"\u4f59\u989d", // 余额
	"\u8d39\u7528", // 费用
	"\u6263\u8d39", // 扣费
	"\u8ba1\u8d39", // 计费
	"\u6d88\u8d39", // 消费
	"\u6d41\u6c34", // 流水
	"\u660e\u7ec6", // 明细
	"bill",
	"billing",
	"balance",
}

var accountScopeKeywords = []string{
	"\u8d26\u53f7", // 账号
	"\u8d26\u6237", // 账户
	"account",
}

var accountBillKeywords = []string{
	"\u8d26\u5355",             // 账单
	"\u603b\u8d26\u5355",       // 总账单
	"\u4f59\u989d",             // 余额
	"\u6d88\u8d39",             // 消费
	"\u6d88\u8d39\u6d41\u6c34", // 消费流水
	"\u6d41\u6c34",             // 流水
	"\u660e\u7ec6",             // 明细
	"\u53d1\u7968",             // 发票
	"\u5145\u503c",             // 充值
	"\u6b20\u8d39",             // 欠费
	"bill",
	"billing",
	"balance",
	"invoice",
}

var monthlyBillKeywords = []string{
	"\u672c\u6708", // 本月
	"\u6708\u5ea6", // 月度
	"\u5f53\u6708", // 当月
}

var instanceBillingSignalKeywords = []string{
	"\u8d39\u7528", // 费用
	"\u6263\u8d39", // 扣费
	"\u8ba1\u8d39", // 计费
	"\u6536\u8d39", // 收费
	"\u6210\u672c", // 成本
}

var instanceBillingScopeKeywords = []string{
	"\u5b9e\u4f8b",       // 实例
	"\u673a\u5668",       // 机器
	"\u4e3b\u673a",       // 主机
	"\u5173\u673a",       // 关机
	"\u5f53\u524d",       // 当前
	"\u8fd9\u4e9b",       // 这些
	"\u54ea\u4e9b",       // 哪些
	"\u78c1\u76d8",       // 磁盘
	"\u6309\u91cf",       // 按量
	"\u540e\u4ed8\u8d39", // 后付费
	"\u5305\u65e5",       // 包日
	"\u5305\u6708",       // 包月
}

const accountBillingUnsupportedReply = "这类账号级账单、余额和消费流水信息当前不支持由助手查询。请到控制台的财务中心查看：账户总览看余额，账单管理看月度账单，消费记录看扣费流水。"

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

// extractDiagnosisTargets pulls user-visible targets (instance id / name)
// from a diagnosis tool's args. The returned slice is used for substring
// matching against the next user message to detect topic continuity.
func extractDiagnosisTargets(args map[string]any) []string {
	var targets []string
	if v, ok := args["UHostId"].(string); ok && v != "" {
		targets = append(targets, v)
	}
	if v, ok := args["Name"].(string); ok && v != "" {
		targets = append(targets, v)
	}
	return targets
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

func containsNormalizedKeyword(normalizedMsg string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(normalizedMsg, normalizeMsg(kw)) {
			return true
		}
	}
	return false
}

func hasMonitorTimeRangeArgs(args map[string]any) bool {
	if args == nil {
		return false
	}
	_, hasStart := args["StartTime"]
	_, hasEnd := args["EndTime"]
	return hasStart || hasEnd
}

func (e *Engine) hasHistoricalMonitorContext() bool {
	checkedUsers := 0
	for i := len(e.messages) - 1; i >= 0 && checkedUsers < 4; i-- {
		msg := e.messages[i]
		if msg.Role != openai.ChatMessageRoleUser {
			continue
		}
		checkedUsers++
		n := normalizeMsg(msg.Content)
		if containsNormalizedKeyword(n, monitorRefreshKeywords) &&
			containsNormalizedKeyword(n, monitorHistoryTimeKeywords) {
			return true
		}
	}
	return false
}

func (e *Engine) blockInvalidMonitorBatchHistory(action string, args map[string]any) (string, bool) {
	if action != "GetCompShareInstanceMonitor" {
		return "", false
	}
	targets := extractMonitorTargets(args)
	if len(targets) <= 1 {
		return "", false
	}
	if !hasMonitorTimeRangeArgs(args) && !e.hasHistoricalMonitorContext() {
		return "", false
	}
	return "历史时间窗监控不能批量查询。请逐台单实例查询：每次调用 GetCompShareInstanceMonitor 只传 1 个 UHostId，并携带对应 StartTime/EndTime；如果要查全部实例，请拆成多次单实例调用。", true
}

func (e *Engine) normalizeMonitorTimeArgs(action string, args map[string]any) {
	if action != "GetCompShareInstanceMonitor" {
		return
	}
	if len(extractMonitorTargets(args)) != 1 {
		return
	}
	start, end, ok := e.inferMonitorWindowFromRecentUserMessages()
	if !ok {
		return
	}
	args["StartTime"] = start
	args["EndTime"] = end
}

func (e *Engine) inferMonitorWindowFromRecentUserMessages() (int64, int64, bool) {
	checkedUsers := 0
	for i := len(e.messages) - 1; i >= 0 && checkedUsers < 4; i-- {
		msg := e.messages[i]
		if msg.Role != openai.ChatMessageRoleUser {
			continue
		}
		checkedUsers++
		if start, end, ok := e.inferMonitorWindowFromMessage(msg.Content); ok {
			return start, end, true
		}
	}
	return 0, 0, false
}

func (e *Engine) inferMonitorWindowFromMessage(msg string) (int64, int64, bool) {
	n := normalizeMsg(msg)
	if !containsNormalizedKeyword(n, monitorRefreshKeywords) {
		return 0, 0, false
	}
	nowFn := e.nowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().In(beijingZone)
	if d, ok := parseRelativeDuration(n); ok {
		end := now.Unix()
		return end - int64(d.Seconds()), end, true
	}
	baseDate, hasDate := parseMonitorBaseDate(n, now)
	if !hasDate {
		return 0, 0, false
	}
	startMinute, endMinute, ok := parseClockWindowMinutes(n)
	if !ok {
		return 0, 0, false
	}
	start := time.Date(baseDate.Year(), baseDate.Month(), baseDate.Day(), 0, 0, 0, 0, beijingZone).
		Add(time.Duration(startMinute) * time.Minute)
	end := time.Date(baseDate.Year(), baseDate.Month(), baseDate.Day(), 0, 0, 0, 0, beijingZone).
		Add(time.Duration(endMinute) * time.Minute)
	if !end.After(start) {
		end = start.Add(30 * time.Minute)
	}
	return start.Unix(), end.Unix(), true
}

func parseMonitorBaseDate(n string, now time.Time) (time.Time, bool) {
	if strings.Contains(n, "\u524d\u5929") { // 前天
		return now.AddDate(0, 0, -2), true
	}
	if strings.Contains(n, "\u6628\u5929") { // 昨天
		return now.AddDate(0, 0, -1), true
	}
	if strings.Contains(n, "\u4eca\u5929") { // 今天
		return now, true
	}
	if m := monthDayRE.FindStringSubmatch(n); len(m) == 3 {
		month, _ := strconv.Atoi(m[1])
		day, _ := strconv.Atoi(m[2])
		if month >= 1 && month <= 12 && day >= 1 && day <= 31 {
			return time.Date(now.Year(), time.Month(month), day, 0, 0, 0, 0, beijingZone), true
		}
	}
	return time.Time{}, false
}

func parseRelativeDuration(n string) (time.Duration, bool) {
	if !strings.Contains(n, "\u8fc7\u53bb") { // 过去
		return 0, false
	}
	if m := durationRE.FindStringSubmatch(n); len(m) == 3 {
		v, _ := strconv.Atoi(m[1])
		if v <= 0 {
			return 0, false
		}
		switch m[2] {
		case "\u5206\u949f": // 分钟
			return time.Duration(v) * time.Minute, true
		case "\u5c0f\u65f6": // 小时
			return time.Duration(v) * time.Hour, true
		}
	}
	return 0, false
}

func parseClockWindowMinutes(n string) (int, int, bool) {
	start, startEnd, ok := parseFirstClockMinute(n)
	if !ok {
		return 0, 0, false
	}
	if idx := strings.IndexAny(n[startEnd:], "\u5230\u81f3-~"); idx >= 0 {
		suffix := n[startEnd+idx+1:]
		if end, _, ok := parseFirstClockMinuteWithContext(suffix, n); ok {
			return start, end, true
		}
	}
	return start - 15, start + 15, true
}

func parseFirstClockMinute(s string) (int, int, bool) {
	return parseFirstClockMinuteWithContext(s, s)
}

func parseFirstClockMinuteWithContext(s, context string) (int, int, bool) {
	if m := clockRE.FindStringSubmatchIndex(s); len(m) >= 6 {
		hour, _ := strconv.Atoi(s[m[2]:m[3]])
		minute, _ := strconv.Atoi(s[m[4]:m[5]])
		if hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59 {
			return adjustHourByDayPart(hour, context)*60 + minute, m[1], true
		}
	}
	if m := hourRE.FindStringSubmatchIndex(s); len(m) >= 4 {
		hour, _ := strconv.Atoi(s[m[2]:m[3]])
		if hour >= 0 && hour <= 23 {
			return adjustHourByDayPart(hour, context) * 60, m[1], true
		}
	}
	for token, hour := range chineseHourTokens {
		if idx := strings.Index(s, token); idx >= 0 {
			return adjustHourByDayPart(hour, context) * 60, idx + len(token), true
		}
	}
	return 0, 0, false
}

func adjustHourByDayPart(hour int, context string) int {
	if hour < 12 && (strings.Contains(context, "\u4e0b\u5348") || strings.Contains(context, "\u665a\u4e0a")) {
		return hour + 12
	}
	if hour < 11 && strings.Contains(context, "\u4e2d\u5348") {
		return hour + 12
	}
	return hour
}

var (
	monthDayRE   = regexp.MustCompile(`(\d{1,2})\s*月\s*(\d{1,2})\s*(?:日|号)?`)
	durationRE   = regexp.MustCompile(`(\d{1,3})\s*(分钟|小时)`)
	clockRE      = regexp.MustCompile(`(\d{1,2})\s*[:：]\s*(\d{1,2})`)
	hourRE       = regexp.MustCompile(`(\d{1,2})\s*(?:点|时)`)
	isoDateRE    = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	clockRangeRE = regexp.MustCompile(`\b\d{1,2}:\d{2}\s*(?:~|-|到|至)\s*\d{1,2}:\d{2}\b`)
)

var chineseHourTokens = map[string]int{
	"\u4e00\u70b9": 1,  // 一点
	"\u4e8c\u70b9": 2,  // 二点
	"\u4e24\u70b9": 2,  // 两点
	"\u4e09\u70b9": 3,  // 三点
	"\u56db\u70b9": 4,  // 四点
	"\u4e94\u70b9": 5,  // 五点
	"\u516d\u70b9": 6,  // 六点
	"\u4e03\u70b9": 7,  // 七点
	"\u516b\u70b9": 8,  // 八点
	"\u4e5d\u70b9": 9,  // 九点
	"\u5341\u70b9": 10, // 十点
}

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
//     through so the LLM/`shouldForceSupportedBillingDiagnosis` can
//     route to DiagnoseBilling.
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
	"\u591a\u5c11\u94b1", // 多少钱
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
}

func shouldForceSupportedBillingDiagnosis(userMsg string) bool {
	n := normalizeMsg(userMsg)
	if isAccountBillingUnsupportedNormalized(n) {
		return false
	}
	return containsNormalizedKeyword(n, instanceBillingSignalKeywords) &&
		containsNormalizedKeyword(n, instanceBillingScopeKeywords)
}

// shouldForceMonitorRefresh reports whether the current turn is an adjacent
// monitor-metric follow-up that should force GetCompShareInstanceMonitor via
// tool_choice. This protects live metrics from being summarized from stale
// prior tool results while keeping explicit "use the previous data" requests
// and account-level billing questions out of the monitor guard.
func (e *Engine) shouldForceMonitorRefresh(userMsg string) bool {
	if e.lastMonitorQueryTurn < 0 {
		return false
	}
	if e.userTurn != e.lastMonitorQueryTurn+1 {
		return false
	}
	n := normalizeMsg(userMsg)
	if containsNormalizedKeyword(n, monitorReuseKeywords) {
		return false
	}
	if containsNormalizedKeyword(n, accountBillingKeywords) {
		return false
	}
	return containsNormalizedKeyword(n, monitorRefreshKeywords)
}

func (e *Engine) shouldForceMonitorInstanceDiscovery(userMsg string) bool {
	n := normalizeMsg(userMsg)
	if strings.Contains(n, "uhost-") {
		return false
	}
	if containsNormalizedKeyword(n, monitorReuseKeywords) {
		return false
	}
	if containsNormalizedKeyword(n, accountBillingKeywords) {
		return false
	}
	if containsNormalizedKeyword(n, monitorPrimaryIntentKeywords) {
		return true
	}
	return containsNormalizedKeyword(n, monitorMetricSubjectKeywords) &&
		containsNormalizedKeyword(n, monitorMetricAskKeywords)
}

func shouldForceResourceInfoDiscovery(userMsg string) bool {
	n := normalizeMsg(userMsg)
	if isAccountBillingUnsupportedNormalized(n) {
		return false
	}
	return containsNormalizedKeyword(n, resourceInfoDiscoveryKeywords)
}

func shouldForceMixedMonitorBilling(userMsg string) bool {
	n := normalizeMsg(userMsg)
	if isAccountBillingUnsupportedNormalized(n) {
		return false
	}
	return containsNormalizedKeyword(n, monitorRefreshKeywords) &&
		containsNormalizedKeyword(n, instanceBillingSignalKeywords) &&
		containsNormalizedKeyword(n, instanceBillingScopeKeywords)
}

func (e *Engine) shouldContinueHistoricalMonitorAfterDiscovery(content string) bool {
	if e.lastInstanceQueryTurn != e.userTurn {
		return false
	}
	if e.lastMonitorQueryTurn == e.userTurn {
		return false
	}
	if _, _, ok := e.monitorTemporalWindowForCurrentTurn(); !ok {
		return false
	}
	n := normalizeMsg(content)
	if containsNormalizedKeyword(n, monitorNoCandidateKeywords) {
		return false
	}
	return containsNormalizedKeyword(n, monitorNeedlessConfirmationKeywords)
}

// shouldForceBillingDiagnosis reports whether the current turn is a
// billing-diagnosis follow-up that should force a re-invocation of
// DiagnoseBilling via tool_choice. Conditions (all must hold):
//   - prior tracked diagnosis was DiagnoseBilling
//   - current turn is immediately adjacent (lastDiagnosisTurn + 1)
//   - userMsg matches a billing follow-up keyword (extremely narrow word list)
//
// Note on target tracking: lastDiagnosisTargets is retained for future use
// (e.g. stricter matching or telemetry) but is NOT a gate here. A bare
// instance-name match without a billing keyword is ambiguous — the same
// instance name can appear in restart / release / SSH intents that are
// unrelated to billing. Requiring a billing keyword prevents the guard
// from hijacking adjacent same-instance turns with different intents.
//
// This is intentionally single-domain. Do not extend to other Diagnose*
// tools without re-evaluating against real-account regression.
func (e *Engine) shouldForceBillingDiagnosis(userMsg string) bool {
	if shouldForceSupportedBillingDiagnosis(userMsg) {
		return true
	}
	if e.lastDiagnosisTool != "DiagnoseBilling" {
		return false
	}
	if e.userTurn != e.lastDiagnosisTurn+1 {
		return false
	}
	for _, kw := range billingFollowUpKeywords {
		if strings.Contains(userMsg, kw) {
			return true
		}
	}
	return false
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
