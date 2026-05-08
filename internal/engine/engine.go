package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
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
	safeExecutor          *tools.SafeToolExecutor
	confirmFn             ConfirmFunc
	messages              []openai.ChatCompletionMessage // conversation history
	userTurn              int                            // incremented at start of each Chat() call
	lastInstanceQueryTurn int                            // set to userTurn on successful DescribeCompShareInstance
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
}

func New(cfg *config.Config, confirmFn ConfirmFunc) *Engine {
	eng := &Engine{
		llmClient:             llm.NewClient(cfg.Agent.LLM),
		confirmFn:             confirmFn,
		lastInstanceQueryTurn: -1,
		lastDiagnosisTurn:     -1,
	}
	eng.safeExecutor = newSafeToolExecutor(tools.NewExternalExecutor(cfg.Agent), confirmFn)
	return eng
}

// NewWithDeps creates an Engine with injected dependencies (for testing).
func NewWithDeps(client LLMClient, executor tools.ToolExecutor, confirmFn ConfirmFunc) *Engine {
	eng := &Engine{
		llmClient:             client,
		confirmFn:             confirmFn,
		lastInstanceQueryTurn: -1,
		lastDiagnosisTurn:     -1,
	}
	eng.safeExecutor = newSafeToolExecutor(executor, confirmFn)
	return eng
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
	result, err := e.executeRawTool(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100}, tools.OriginDirectLLM)
	if err != nil {
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

	// Trim before appending to guarantee the new user message is never dropped.
	e.trimHistory()

	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userMsg,
	})

	forceBilling := e.shouldForceBillingDiagnosis(userMsg)

	for round := 0; round < maxReActRounds; round++ {
		req := llm.ChatRequest{
			Messages: e.buildMessagesForLLM(),
			Tools:    tools.Registry,
		}
		// Hard guard: billing-diagnosis follow-up in the immediately next turn
		// must re-run DiagnoseBilling instead of reusing the prior conclusion.
		// Scope: first LLM call of this turn only; subsequent ReAct rounds
		// narrate the fresh tool result freely.
		if round == 0 && forceBilling {
			req.ToolChoice = openai.ToolChoice{
				Type:     openai.ToolTypeFunction,
				Function: openai.ToolFunction{Name: "DiagnoseBilling"},
			}
		}
		resp, err := e.llmClient.Chat(ctx, req)
		if err != nil {
			return "", fmt.Errorf("LLM 调用失败: %w", err)
		}

		// No tool calls → final text reply
		if len(resp.ToolCalls) == 0 {
			e.messages = append(e.messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: resp.Content,
			})
			return resp.Content, nil
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
		onStep(StepEvent{Type: StepError, Action: action, Message: errMsg})
		return errMsg
	}

	// Knowledge tools execute locally — no API call, no security check needed
	if knowledge.IsKnowledgeTool(action) {
		args = e.safeExecutor.FilterArgs(action, args)
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
		args = e.safeExecutor.FilterArgs(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Args: e.safeExecutor.RedactArgs(action, args)})
		return e.executeWorkflow(ctx, action, args, onStep)
	}

	// Diagnosis meta-tools → delegate to diagnosis engine.
	if diagnosis.IsDiagnosisTool(action) {
		args = e.safeExecutor.FilterArgs(action, args)
		onStep(StepEvent{Type: StepToolCall, Action: action, Args: e.safeExecutor.RedactArgs(action, args)})
		return e.executeDiagnosis(ctx, action, args, onStep)
	}

	result, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: tools.OriginDirectLLM,
		Hooks: tools.SafeToolHooks{
			OnConfirmNeeded: func(action string, args map[string]any) {
				onStep(StepEvent{Type: StepConfirmNeeded, Action: action, Args: e.safeExecutor.RedactArgs(action, args), Message: "此操作需要您确认"})
			},
			OnBeforeCall: func(action string, args map[string]any) {
				onStep(StepEvent{Type: StepToolCall, Action: action, Args: args})
			},
		},
	})
	if err != nil {
		if errors.Is(err, tools.ErrDestructiveAction) {
			msg := fmt.Sprintf("安全限制：%s 是破坏性操作（L2），已拒绝执行。请到控制台手动操作。", action)
			onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
			return finalReplyPrefix + msg
		}
		if errors.Is(err, tools.ErrUserDeclined) {
			msg := fmt.Sprintf("操作 %s 已取消（用户未确认）。", action)
			onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
			return finalReplyPrefix + msg
		}
		errMsg := fmt.Sprintf("API 调用失败: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: errMsg})
		return errMsg
	}

	var display string
	if result.Display.Kind == "JupyterToken" && result.Display.Value != "" {
		display = fmt.Sprintf("Jupyter Token: %s", result.Display.Value)
	}

	formatted := prompt.FormatToolResult(result.LLMResult)
	onStep(StepEvent{Type: StepToolResult, Action: action, Message: "调用成功", Display: display})
	return formatted
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
	result, err := e.safeExecutor.ExecuteSafe(ctx, req)
	if err == nil && req.Action == "DescribeCompShareInstance" {
		e.lastInstanceQueryTurn = e.userTurn
	}
	return result, err
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

	wfEngine := workflow.NewEngine(e.toolExecutorFor(tools.OriginWorkflowInternal), wfConfirm, func(ev workflow.StepEvent) {
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
			Args:    e.safeExecutor.RedactArgs(ev.Tool, ev.Args),
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
	// Target check is UHostId-only because SafeToolExecutor filters upstream
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

	diagEngine := diagnosis.NewEngine(e.toolExecutorFor(tools.OriginDiagnosisInternal), func(ev diagnosis.DiagEvent) {
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
			Args:    e.safeExecutor.RedactArgs(ev.Tool, ev.Args),
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
