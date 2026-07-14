package engine

import (
	"strings"

	"github.com/compshare-agent/internal/intent"
)

// Deterministic enumeration for the agent loop (step 2 of the context work).
//
// PR B's premise is that routing every intent through the ReAct loop buys the model the
// conversation history it needs to resolve "那个" / "5090" / "控制台". The 2026-07-12
// capture confirmed the win — and confirmed its price: with direct dispatch off, the
// loop answered 我目前部署的实例 by naming three of thirteen instances and inventing a
// fourteenth. The fast path, rendering the same payload in Go, invented none across six
// such turns.
//
// The fix is not to police the model's typing afterwards, it is to stop asking it to
// type. When an instance lookup comes back, the tool result carries the finished table
// alongside the data, and the agent is told to emit that block verbatim. It keeps the
// reasoning and the context; it loses the opportunity to hallucinate a machine.
//
// Boot-only flag, Go-package default OFF, so unit tests and the current production path
// are unaffected until the A/B says otherwise.
var agentDeterministicRenderEnabled bool

// SetAgentDeterministicRenderEnabled freezes the flag at boot, mirroring the shape of
// the other engine gates.
func SetAgentDeterministicRenderEnabled(v bool) { agentDeterministicRenderEnabled = v }

// renderedInstanceTableKey and displayInstructionKey are the two fields added to the
// model-visible tool result. The instruction rides WITH the data rather than in the
// system prompt on purpose: it is only true when this particular result is in hand, and
// a standing prompt rule about a field that is usually absent is a rule the model learns
// to ignore.
// The model is asked to emit a PLACEHOLDER, not to copy the table.
//
// The first version of this handed the model a finished table and told it to reproduce
// the block verbatim. Live, it did better — it stopped inventing IDs — but it still
// retyped: given a ten-row rendered table it wrote six rows in its own markdown. Of
// course it did. "Copy this exactly" is a request an LLM can decline by degrees, and a
// mechanism whose safety depends on the model choosing to transcribe faithfully is just
// the original bug with extra steps.
//
// So the table never passes through the model at all. The model writes
// {{INSTANCE_TABLE}} where the list belongs, and the engine substitutes the rendered
// text into the finished reply. It cannot mistype what it never types.
const (
	renderedInstanceTableKey = "RenderedInstanceTable"
	displayInstructionKey    = "DisplayInstruction"
	instanceTablePlaceholder = "{{INSTANCE_TABLE}}"
	instanceDisplayDirective = "要向用户展示实例列表时，请在回答中原样写下占位符 {{INSTANCE_TABLE}}（单独一行），" +
		"系统会自动替换为完整、准确的实例表格。不要自行誊写或改写实例 ID、规格、数量、台数——" +
		"手写的实例信息会出错。占位符之外可以正常写解释和建议。"
)

// substituteInstanceTable replaces the placeholder in the model's finished reply with the
// deterministically rendered table.
//
// If the model ignored the placeholder and hand-wrote a list anyway, this does nothing —
// the grounding observer will record it, and that is the signal that this contract needs
// hardening further (e.g. withholding raw IDs from the model entirely). Silently
// papering over a disobeyed contract would hide exactly the measurement we are here for.
func substituteInstanceTable(reply, table string) (string, bool) {
	if table == "" || !strings.Contains(reply, instanceTablePlaceholder) {
		return reply, false
	}
	return strings.ReplaceAll(reply, instanceTablePlaceholder, table), true
}

// attachDeterministicInstanceTable adds the canonical table to a successful instance
// lookup so the agent can show it without retyping it. No-op unless the flag is on, the
// action is an instance lookup, and the payload actually holds instances.
func (e *Engine) attachDeterministicInstanceTable(action string, raw, llmResult map[string]any) bool {
	if !agentDeterministicRenderEnabled {
		return false
	}
	if action != "DescribeCompShareInstance" {
		return false
	}
	if raw == nil || llmResult == nil {
		return false
	}
	table, ok := intent.RenderInstanceTableFromDescribe(raw)
	if !ok {
		return false
	}
	// The model is shown the table (so it knows what the user will see and can write
	// sensible prose around it) but is told to reference it by placeholder. The engine
	// keeps its own copy: the substitution must use the table WE rendered, never a
	// version that has passed through the model.
	llmResult[renderedInstanceTableKey] = table
	llmResult[displayInstructionKey] = instanceDisplayDirective
	e.instanceTableThisTurn = table
	return true
}
