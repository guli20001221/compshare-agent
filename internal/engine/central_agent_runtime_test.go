package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestCentralAgentReadTurnHasOneSemanticChainAndStructuredCapability(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read", tools.ReadPlatformCapabilityName, `{"capability":"resource_info","slots":{}}`)}},
		{Content: "你有一台名为 train-a 的实例。"},
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(64),
		}}},
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentResourceInfo, Confidence: 1}}}}
	eng := NewWithDeps(model, executor, nil)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo}})
	eng.enableCentralAgentRuntimeForTest()

	reply, err := eng.Chat(context.Background(), "我有哪些实例？", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, "train-a")
	require.Empty(t, planner.calls, "router must not precede the central Agent")
	require.Len(t, model.calls, 2)
	require.Contains(t, executor.calls, "DescribeCompShareInstance")
	firstTools := toolNames(model.calls[0].Tools)
	require.Contains(t, firstTools, tools.ReadPlatformCapabilityName)
	require.Contains(t, firstTools, "SearchKnowledge")
	require.NotContains(t, firstTools, "DescribeCompShareInstance", "underlying handler APIs must stay behind the evidence adapter")
	require.NotContains(t, firstTools, "StopInstanceWorkflow")
}

func TestCentralAgentShortFollowupReceivesCommittedConversation(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{{Content: "在 Windows 终端中可用 Ctrl+Shift+V 粘贴。"}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.RehydrateHistory([]HistoryMessage{
		{Role: "user", Content: "Windows 终端怎么复制？"},
		{Role: "assistant", Content: "可以用 Ctrl+Shift+C 复制。"},
	})
	eng.enableCentralAgentRuntimeForTest()

	_, err := eng.Chat(context.Background(), "粘贴呢", noopStep)
	require.NoError(t, err)
	require.Len(t, model.calls, 1)
	rendered := renderTestMessages(model.calls[0].Messages)
	require.Contains(t, rendered, "Windows 终端怎么复制？")
	require.Contains(t, rendered, "Ctrl+Shift+C")
	require.Contains(t, rendered, "粘贴呢")
}

func TestCentralAgentBlocksHallucinatedLegacyWriteTool(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("write", "StopInstanceWorkflow", `{"UHostId":"uhost-1"}`)}},
		{Content: "该操作尚未执行。"},
	}}
	executor := &mockExecutor{}
	eng := NewWithDeps(model, executor, func(string, map[string]any) bool { return true })
	eng.enableCentralAgentRuntimeForTest()
	eng.SetMutatingToolsEnabled(true)

	reply, err := eng.Chat(context.Background(), "停止 uhost-1", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, "尚未执行")
	require.Empty(t, executor.calls)
}

func TestCentralAgentAuthorizesSearchAtExecutionBoundary(t *testing.T) {
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.enableCentralAgentRuntimeForTest()
	eng.SetKnowledgeRetriever(retriever)
	out := eng.executeTool(context.Background(), toolCall("search", "SearchKnowledge", `{"query":"终端粘贴快捷键"}`), noopStep)
	require.False(t, strings.Contains(out, "unavailable for this route"))
	require.True(t, eng.knowledgeQAAgentLoopThisTurn)
	require.Len(t, retriever.calls, 1)
}

func TestAgentContextCarriesPendingSelectionOrderWithoutResumingLegacyHandler(t *testing.T) {
	now := time.Now()
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		PendingSelectionKind: "instance", PendingSelectionProducedAtUnix: now.Unix(), PendingSelectionTTLSeconds: 300,
		PendingSelectionItems: []PendingSelectionItem{{Index: 1, ID: "uhost-1", Name: "first"}, {Index: 2, ID: "uhost-2", Name: "second"}},
	}, 1)
	view := (ContextCompiler{}).CompileForTurn(eng, "第二台呢", "turn-selection", now)
	card := renderAgentContextCard(view)
	require.Contains(t, card, "uhost-1")
	require.Contains(t, card, "序号=1")
	require.Contains(t, card, "uhost-2")
	require.Contains(t, card, "序号=2")
}

func TestCentralSessionUsesCentralPromptInsteadOfLegacyWorkflowCatalog(t *testing.T) {
	deps := &SharedDeps{LLMClient: &mockLLM{}, ExternalExecutor: &mockExecutor{}}
	eng := NewSession(deps, SessionOptions{MutatingToolsEnabled: true})
	eng.InitWithContext("test user")
	system := renderTestMessages(eng.MessagesSnapshot())
	require.Contains(t, system, "本轮唯一的业务判断者")
	require.NotContains(t, system, "StopInstanceWorkflow")
}
