package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"

	openai "github.com/sashabaranov/go-openai"
)

// skill_executor.go is the P2 body-driven, read-only skill executor — the
// implementation the AgentLoop.Reason seam (loop.go) was reserved for. Unlike a
// fixed workflow.Definition (saga), whose StepResults never reach an LLM
// (step.go:96), this runs a BOUNDED LLM loop where the skill's markdown Body() is
// the system instruction and each tool result is fed back into a PRIVATE
// working-set buffer — never the engine's shared e.messages, which a FIFO trim
// would drop mid-skill. That step-result-back-to-the-LLM path is the (A) keystone
// the saga has zero infrastructure for.
//
// It is READ-ONLY by construction: callers MUST pass an Exec wired with a
// read-only origin (OriginDiagnosisInternal). The tool-call protocol is text-JSON
// (not native tool_choice) to stay model-agnostic and dodge the ds-v4-flash
// object-tool_choice 400 (lead decision 2026-06-01).

// ChatClient is the strong-tier LLM the executor drives. The engine's
// agentLLMClient satisfies it structurally.
type ChatClient interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

// SkillProgress reports a step boundary to the host. isResult=false marks a tool
// call about to run; isResult=true marks its result (or the final answer). The
// engine adapts this to its StepEvent/onStep bridge.
type SkillProgress func(action, message string, isResult bool)

// SkillExecOptions configures one read-only body-driven skill run.
type SkillExecOptions struct {
	Body      string               // skill.Body() output, verbatim — cautions already injected
	Tools     []openai.Tool        // tools the model may call (defs feed the protocol text)
	Exec      tools.ToolExecutor   // MUST be a read-only origin (OriginDiagnosisInternal)
	Client    ChatClient           // TierAgent strong model
	Progress  SkillProgress        // optional progress bridge
	OnUsage   func(llm.TokenUsage) // optional per-call token accounting (engine budget)
	MaxRounds int                  // tool-call rounds bound; <=0 ⇒ defaultSkillMaxRounds
}

const (
	// defaultSkillMaxRounds bounds the read-only loop. The diagnose pilot needs at
	// most 2 tool calls; 6 leaves headroom for a clarify/retry without runaway.
	defaultSkillMaxRounds = 6
	// skillMalformedBudget is the format-error tolerance: the first malformed step
	// (bad JSON or an unavailable tool) gets one corrective re-prompt; the second
	// safe-fails (lead decision 2026-06-01 — don't let a confused model loop on
	// bad output).
	skillMalformedBudget = 1
)

// ErrSkillExecUnrecovered is returned when the loop cannot produce a final answer
// (malformed twice, unavailable tool twice, or rounds exhausted). The caller
// renders a friendly safe-fail reply; the loop NEVER mutates and NEVER panics.
var ErrSkillExecUnrecovered = errors.New("skill executor: unrecovered")

const skillMalformedCorrection = "上一步的回复不是有效的 JSON。请严格只回一个 JSON 对象：调用工具用 {\"action\":\"工具名\",\"args\":{...}}，给出结论用 {\"final\":\"给用户的中文回答\"}。"

// RunReadOnlySkill runs the body-driven loop and returns the model's final answer.
// userRequest is the user's raw message; seed carries known context (e.g. UHostId)
// that the engine resolved before dispatch.
func RunReadOnlySkill(ctx context.Context, userRequest string, seed map[string]any, opts SkillExecOptions) (string, error) {
	if opts.Client == nil {
		return "", fmt.Errorf("%w: no LLM client", ErrSkillExecUnrecovered)
	}
	maxRounds := opts.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultSkillMaxRounds
	}
	allowed := make(map[string]struct{}, len(opts.Tools))
	for _, t := range opts.Tools {
		if t.Function != nil {
			allowed[t.Function.Name] = struct{}{}
		}
	}

	ws := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: buildSkillSystemPrompt(opts.Body, opts.Tools)},
		{Role: openai.ChatMessageRoleUser, Content: buildSkillUserSeed(userRequest, seed)},
	}

	malformedBudget := skillMalformedBudget
	for round := 0; round < maxRounds; round++ {
		resp, err := opts.Client.Chat(ctx, llm.ChatRequest{Messages: ws})
		if err != nil {
			return "", fmt.Errorf("%w: LLM call: %v", ErrSkillExecUnrecovered, err)
		}
		if opts.OnUsage != nil {
			opts.OnUsage(resp.Usage)
		}
		content := strings.TrimSpace(resp.Content)
		step, perr := parseSkillStep(content)
		if perr != nil {
			if malformedBudget > 0 {
				malformedBudget--
				ws = append(ws, assistantMsg(content), userMsg(skillMalformedCorrection))
				continue
			}
			return "", fmt.Errorf("%w: malformed step twice", ErrSkillExecUnrecovered)
		}
		if step.Final != "" {
			if opts.Progress != nil {
				opts.Progress("", "完成", true)
			}
			return step.Final, nil
		}
		// Tool call.
		if _, ok := allowed[step.Action]; !ok {
			if malformedBudget > 0 {
				malformedBudget--
				ws = append(ws, assistantMsg(content),
					userMsg(fmt.Sprintf("工具 %q 不可用。可用工具：%s。请只用列出的工具，或用 {\"final\":\"...\"} 给出结论。", step.Action, toolNameList(opts.Tools))))
				continue
			}
			return "", fmt.Errorf("%w: unavailable tool %q", ErrSkillExecUnrecovered, step.Action)
		}
		if opts.Progress != nil {
			opts.Progress(step.Action, "调用 "+step.Action, false)
		}
		result, execErr := opts.Exec.Execute(ctx, step.Action, step.Args)
		if opts.Progress != nil {
			if execErr != nil {
				opts.Progress(step.Action, "调用失败", true)
			} else {
				opts.Progress(step.Action, "调用成功", true)
			}
		}
		ws = append(ws, assistantMsg(content),
			userMsg(fmt.Sprintf("工具 %s 返回：\n%s", step.Action, marshalToolResult(result, execErr))))
	}
	return "", fmt.Errorf("%w: exceeded %d rounds without a final answer", ErrSkillExecUnrecovered, maxRounds)
}

type skillStep struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
	Final  string         `json:"final"`
}

// parseSkillStep extracts the single JSON object from the model's reply and
// classifies it as a tool call ({"action",...}) or a terminal answer ({"final"}).
// A reply with neither, or no JSON object at all, is a malformed step.
func parseSkillStep(content string) (skillStep, error) {
	obj := extractJSONObject(content)
	if obj == "" {
		return skillStep{}, fmt.Errorf("no JSON object")
	}
	var step skillStep
	if err := json.Unmarshal([]byte(obj), &step); err != nil {
		return skillStep{}, err
	}
	if step.Final == "" && step.Action == "" {
		return skillStep{}, fmt.Errorf("neither action nor final")
	}
	if step.Args == nil {
		step.Args = map[string]any{}
	}
	return step, nil
}

// extractJSONObject returns the first complete brace-balanced JSON object in s
// (string-literal aware), or "" if none. Tolerates prose before/after the object.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func buildSkillSystemPrompt(body string, toolDefs []openai.Tool) string {
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\n---\n可用工具：\n")
	for _, t := range toolDefs {
		if t.Function == nil {
			continue
		}
		fmt.Fprintf(&b, "- %s：%s\n", t.Function.Name, t.Function.Description)
	}
	b.WriteString("\n每一步只回一个 JSON 对象，不要任何额外文字：\n")
	b.WriteString("- 调用工具：{\"action\":\"工具名\",\"args\":{...}}\n")
	b.WriteString("- 给出最终结论：{\"final\":\"给用户的中文回答\"}\n")
	b.WriteString("按正文方法逐步排查，掌握足够信息后用 {\"final\":...} 结束。")
	return b.String()
}

func buildSkillUserSeed(userRequest string, seed map[string]any) string {
	var b strings.Builder
	b.WriteString("用户请求：")
	b.WriteString(strings.TrimSpace(userRequest))
	if len(seed) > 0 {
		if raw, err := json.Marshal(seed); err == nil {
			b.WriteString("\n已知上下文(JSON)：")
			b.Write(raw)
		}
	}
	return b.String()
}

func marshalToolResult(result map[string]any, execErr error) string {
	if execErr != nil {
		b, _ := json.Marshal(map[string]any{"error": execErr.Error()})
		return string(b)
	}
	b, err := json.Marshal(result)
	if err != nil {
		return `{"error":"result not serializable"}`
	}
	return string(b)
}

func assistantMsg(c string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: c}
}

func userMsg(c string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: c}
}

func toolNameList(toolDefs []openai.Tool) string {
	names := make([]string, 0, len(toolDefs))
	for _, t := range toolDefs {
		if t.Function != nil {
			names = append(names, t.Function.Name)
		}
	}
	return strings.Join(names, ", ")
}
