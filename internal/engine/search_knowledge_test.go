package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteSearchKnowledge_LocalDispatchSubstantive proves the P3 hinge: the
// SearchKnowledge ReAct tool dispatches LOCALLY on the engine retriever (never
// through the external/safe executor — its Route is knowledge, not external_api),
// returns a SUBSTANTIVE EvidenceLedger (a chunk-content snippet the agent can
// ground an actionable answer on, not the content-free diagnosis ledger), and
// records the hits so the final-answer no-raw-leak guard can validate the
// synthesis turn.
func TestExecuteSearchKnowledge_LocalDispatchSubstantive(t *testing.T) {
	content := "降低 vLLM 显存占用：缩短上下文 --max-model-len；降低并发 --max-num-seqs；" +
		"多卡张量并行 --tensor-parallel-size；使用量化 quantization。"
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true,
		HitItems: []knowledge.RetrievalHit{{
			Kept:  true,
			Score: 90,
			Chunk: knowledge.KBChunk{
				ChunkID:    "ext-gpu-oom-vllm-001",
				Title:      "vLLM 显存不足 (OOM) 排查",
				SourceType: "external",
				Content:    content,
			},
		}},
	}}}
	exec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, exec, nil)
	eng.SetKnowledgeRetriever(retriever)

	tc := openai.ToolCall{
		ID:   "call-sk",
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      "SearchKnowledge",
			Arguments: `{"query":"vllm 显存不足"}`,
		},
	}
	out := eng.executeTool(context.Background(), tc, noopStep)

	// Substantive evidence: chunk id + a real actionable token in the snippet.
	assert.Contains(t, out, "EvidenceLedger")
	assert.Contains(t, out, "ext-gpu-oom-vllm-001")
	assert.Contains(t, out, "--max-model-len", "result must carry actionable content for the agent to ground on")

	// Local dispatch: the external/safe tool executor was NEVER called.
	assert.Empty(t, exec.calls, "SearchKnowledge must dispatch locally, never via the API/safe executor")

	// Retriever ran with the query; hits recorded for the synthesis guard.
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "vllm 显存不足", retriever.calls[0].question)
	assert.True(t, eng.searchKnowledgeRanThisTurn)
	assert.Len(t, eng.searchKnowledgeHitsThisTurn, 1)
}

// TestPlannerDiagnosis_DeadEndRelaxedWhenAgenticOn proves the P4a flag-gated
// relax: with COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE on, an empty-target diagnosis
// turn (>1 instance) NO LONGER short-circuits with the canned which-instance
// reply — it falls through to the agent lane (ReAct) so the loop can call
// SearchKnowledge first. Flag off is byte-identical (covered by
// TestPlannerDiagnosisClarificationDoesNotRequireEnabledIntent).
func TestPlannerDiagnosis_DeadEndRelaxedWhenAgenticOn(t *testing.T) {
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)

	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: diagnosisPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react path"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "train-a", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "train-b", "State": "Running"},
		},
	}, "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	// "我的机器 SSH 连不上了" = "my machine can't SSH"
	reply, err := eng.Chat(context.Background(), "我的机器 SSH 连不上了", noopStep)
	require.NoError(t, err)
	// "哪台实例" = "which instance" — the canned dead-end phrase.
	assert.NotContains(t, reply, "哪台实例", "flag on: must NOT fire the canned which-instance dead-end")
	assert.NotEmpty(t, mock.calls, "flag on: empty-target diagnosis falls through to the agent lane (ReAct)")
}
