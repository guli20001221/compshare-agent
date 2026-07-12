package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/grounding"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Knowledge tools (GetGPUSpecs / GetGPURecommendation / GetModelVRAMRequirement)
// execute locally and RETURN before executeSafeTool, which is where the grounding
// harvest lives. So the validator never saw their payloads, and a reply that read a
// GPU spec straight off a tool result — the correct, grounded thing to do — was
// reported as a fabrication.
//
// This is not hypothetical: the 4090's FP16 figure (82.6 TFLOPS) lives in
// knowledge/gpu_specs.go and is served by GetGPUSpecs, and it was the single
// most-flagged "violation" in the 17-session capture. The number was right, the
// source was right, and the validator was wrong.
//
// The lesson is the reason this test exists at all: a guard that reports false
// positives on correct behaviour is worse than no guard, because the first thing
// anyone does with a noisy guard is stop believing it — and by then it cannot tell
// them about the real fabrication either.
func TestGPUSpecReadOffAKnowledgeToolIsGrounded(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Round 1: the model looks the spec up, exactly as it should.
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "GetGPUSpecs", `{"GpuType":"4090"}`)}},
		// Round 2: it reports the figure the tool handed back.
		{Content: "RTX 4090 的 FP16 算力是 82.6 TFLOPS，显存 24 GB。"},
	}}

	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.ChatWithOptions(context.Background(), "4090 什么规格", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Contains(t, reply, "82.6")

	// The harvest must have seen the knowledge-tool payload.
	require.NotNil(t, eng.turnFacts)
	require.False(t, eng.turnFacts.Empty(), "no facts harvested — the knowledge tool's payload never reached the grounding bag")

	violations := grounding.Check(reply, eng.turnFacts)
	assert.Empty(t, violations,
		"a GPU spec the model read off GetGPUSpecs was reported as ungrounded; "+
			"the figure is real, it came from knowledge/gpu_specs.go, and the validator simply "+
			"was not shown the payload: %v", violations)
}

// The counterpart, and the reason the fix above is not just "harvest everything and
// stop flagging things": a figure the model did NOT get from any tool must still be
// caught. If the fix had worked by weakening the check rather than by widening the
// harvest, this would go quiet too.
func TestSpecTheModelNeverLookedUpIsStillCaught(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		// The model looks up the 4090...
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "GetGPUSpecs", `{"GpuType":"4090"}`)}},
		// ...and then quotes a figure for a card it never asked about.
		{Content: "RTX 4090 是 82.6 TFLOPS。顺便一提，H200 的 FP16 是 1979 TFLOPS。"},
	}}

	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.ChatWithOptions(context.Background(), "4090 什么规格", noopStep, ChatOptions{})
	require.NoError(t, err)

	violations := grounding.Check(reply, eng.turnFacts)
	require.NotEmpty(t, violations, "an unlooked-up spec passed as grounded — the harvest fix has blinded the check")

	var flagged string
	for _, v := range violations {
		if v.Claim == "1979 TFLOPS" {
			flagged = v.Claim
		}
	}
	assert.Equal(t, "1979 TFLOPS", flagged,
		"the fabricated H200 figure was not among the violations %v; widening the harvest must not "+
			"launder numbers the model never actually looked up", violations)
}

// SearchKnowledge hands the model searchKnowledgeResultJSON(ledger), but the harvest
// was fed only ledger.Items[].Summary and .Snippet — a NARROWER view than what the
// model actually read. Anything the model correctly quoted from the part of the
// evidence the harvest had not been shown came back as a fabrication.
//
// Not hypothetical. Asked 是多少钱, the model read the disk-price table out of chunk
// w0-billing_rule-gitlab-compshare-docs-bill-7e69b1aa and quoted 0.0005元/GB/小时,
// 0.011元/GB/日 and 0.3元/GB/月 — verbatim, all three correct — and the validator
// reported all three as invented prices. Quoting the price list accurately is the
// single most valuable thing this agent does; calling that a fabrication is the
// fastest way to get the guard switched off.
//
// The invariant: facts == what the model was shown. Not a subset (correct answers get
// accused), not a superset (invented ones get certified).
func TestFiguresQuotedFromRetrievedEvidenceAreGrounded(t *testing.T) {
	f := grounding.NewFacts()

	// Stands in for the payload SearchKnowledge returns — the real chunk's price table.
	f.AddRaw(`{"items":[{"chunk_id":"w0-billing_rule-7e69b1aa",` +
		`"text":"| 后付费 | 按量 | 0.0005元/GB/小时 | 预付费 | 包日 | 0.011元/GB/日 | 预付费 | 包月 | 0.3元/GB/月 |"}]}`)

	violations := grounding.Check("云盘按量后付费 0.0005元/GB/小时，包日 0.011元/GB/日，包月 0.3元/GB/月。", f)
	assert.Empty(t, violations,
		"prices quoted verbatim from the retrieved chunk were reported as fabrications: %v", violations)

	// And it still bites: a price that appeared in no chunk is still caught.
	assert.NotEmpty(t, grounding.Check("云盘包月 9.9元/GB/月。", f),
		"an invented price passed as grounded — the harvest fix has blinded the check")
}
