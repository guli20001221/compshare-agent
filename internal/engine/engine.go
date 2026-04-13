package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/prompt"
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
	llmClient  LLMClient
	executor   tools.ToolExecutor
	confirmFn  ConfirmFunc
	messages   []openai.ChatCompletionMessage // conversation history
}

func New(cfg *config.Config, confirmFn ConfirmFunc) *Engine {
	executor := tools.NewExternalExecutor(cfg.Agent)
	return &Engine{
		llmClient: llm.NewClient(cfg.Agent.LLM),
		executor:  executor,
		confirmFn: confirmFn,
	}
}

// NewWithDeps creates an Engine with injected dependencies (for testing).
func NewWithDeps(client LLMClient, executor tools.ToolExecutor, confirmFn ConfirmFunc) *Engine {
	return &Engine{
		llmClient: client,
		executor:  executor,
		confirmFn: confirmFn,
	}
}

// Init performs first-turn context injection:
// calls DescribeCompShareInstance and builds the system prompt.
// Returns opening suggestions.
func (e *Engine) Init(ctx context.Context) ([]prompt.Suggestion, error) {
	// Auto-inject user instance context
	userCtx := "暂无用户信息"
	result, err := e.executor.Execute(ctx, "DescribeCompShareInstance", map[string]any{})
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

// Chat processes one user message through the ReAct loop and returns the final text reply.
// The callback is invoked for each intermediate step (tool calls, thinking, etc.).
func (e *Engine) Chat(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, error) {
	// Trim before appending to guarantee the new user message is never dropped.
	e.trimHistory()

	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userMsg,
	})

	for round := 0; round < maxReActRounds; round++ {
		resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{
			Messages: e.messages,
			Tools:    tools.Registry,
		})
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

		for _, tc := range resp.ToolCalls {
			toolResult := e.executeTool(ctx, tc, onStep)
			e.messages = append(e.messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    toolResult,
				ToolCallID: tc.ID,
			})
		}
	}

	return "抱歉，处理轮次超限，请重新描述您的需求。", nil
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
		onStep(StepEvent{Type: StepToolCall, Action: action, Args: args})
		return e.executeWorkflow(ctx, action, args, onStep)
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
		return msg
	}

	// L1: must get user confirmation before executing
	if level == security.L1 {
		onStep(StepEvent{Type: StepConfirmNeeded, Action: action, Args: args, Message: "此操作需要您确认"})
		if e.confirmFn == nil || !e.confirmFn(action, args) {
			msg := fmt.Sprintf("操作 %s 已取消（用户未确认）。", action)
			onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
			return msg
		}
	}

	// Strip parameters not in the tool schema to prevent LLM injection
	args = filterAllowedParams(action, args)

	onStep(StepEvent{Type: StepToolCall, Action: action, Args: args})

	// Execute external API
	result, err := e.executor.Execute(ctx, action, args)
	if err != nil {
		errMsg := fmt.Sprintf("API 调用失败: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: errMsg})
		return errMsg
	}

	formatted := prompt.FormatToolResult(result)
	onStep(StepEvent{Type: StepToolResult, Action: action, Message: "调用成功"})
	return formatted
}

// executeWorkflow runs a predefined workflow and returns the result as a JSON string
// for the LLM to narrate.
func (e *Engine) executeWorkflow(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
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
		if ev.Status == "failed" {
			eventType = StepError
		}
		onStep(StepEvent{
			Type:    eventType,
			Action:  ev.Tool,
			Args:    ev.Args,
			Message: fmt.Sprintf("[%d/%d] %s: %s", ev.StepIndex+1, ev.Total, ev.StepName, ev.Status),
		})
	})

	result, err := wfEngine.Run(ctx, wf, args)
	if err != nil {
		msg := fmt.Sprintf("工作流执行错误: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
		return msg
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
}

// trimHistory keeps the message list under maxHistoryMessages by dropping
// the oldest non-system messages. The system prompt (index 0) is always kept.
func (e *Engine) trimHistory() {
	if len(e.messages) <= 1+maxHistoryMessages {
		return
	}
	// Keep: messages[0] (system) + last maxHistoryMessages messages
	keep := e.messages[len(e.messages)-maxHistoryMessages:]
	e.messages = append([]openai.ChatCompletionMessage{e.messages[0]}, keep...)
}
