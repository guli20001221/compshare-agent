package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/refusal"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func customerSupportToolCall() llm.ChatResponse {
	return llm.ChatResponse{ToolCalls: []openai.ToolCall{
		toolCall("support", tools.CustomerSupportHandoffName, `{}`),
	}}
}

func TestChatCustomerSupportHandoffIsAnAgentDecision(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall()}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	onStep, steps := collectSteps()

	reply, err := eng.Chat(context.Background(), "请帮我转接人工客服", onStep)
	require.NoError(t, err)
	require.Equal(t, refusal.HumanAgentTransfer, reply)
	require.Len(t, model.calls, 1, "the request must reach the central Agent")
	require.Contains(t, toolNames(model.calls[0].Tools), tools.CustomerSupportHandoffName)
	require.Len(t, *steps, 2)
	require.Equal(t, StepToolCall, (*steps)[0].Type)
	require.Equal(t, StepToolResult, (*steps)[1].Type)
	require.Equal(t, tools.CustomerSupportHandoffName, (*steps)[0].Action)
	require.Contains(t, (*steps)[1].Message, "未确认接通或受理")

	toolJSON, err := json.Marshal(model.calls[0].Tools)
	require.NoError(t, err)
	require.NotContains(t, string(toolJSON), "qrcode.png", "the model must not author the channel renderer")
	require.NotContains(t, string(toolJSON), agentprotocol.FeishuCustomerSupportMarker)
}

func TestChatCustomerSupportMentionsRemainWithTheCentralAgent(t *testing.T) {
	cases := []string{
		"不要转人工，继续帮我排查",
		"你刚才说的转人工是什么意思？",
		"人工客服能解决什么问题？",
		"我先不找人工客服",
	}
	for _, question := range cases {
		t.Run(question, func(t *testing.T) {
			const answer = "好的，我继续根据你的问题处理。"
			model := &mockLLM{responses: []llm.ChatResponse{{Content: answer}}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)

			reply, err := eng.Chat(context.Background(), question, noopStep)
			require.NoError(t, err)
			require.Equal(t, answer, reply)
			require.Len(t, model.calls, 1, "a mention must not be intercepted before semantic interpretation")
			require.NotContains(t, reply, "qrcode.png")
		})
	}
}

func TestFeishuModesUseTheirSupportRenderer(t *testing.T) {
	for name, opts := range map[string]ChatOptions{
		"public platform without console handoff":    {PublicPlatformReadOnly: true},
		"knowledge only with console handoff":        {KnowledgeOnly: true, FeishuConsoleHandoff: true},
		"knowledge precedence keeps Feishu renderer": {KnowledgeOnly: true, PublicPlatformReadOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			model := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall()}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)

			reply, err := eng.ChatWithOptions(context.Background(), "我需要人工客服", noopStep, opts)
			require.NoError(t, err)
			require.Equal(t, agentprotocol.FeishuCustomerSupportMarker, reply)
			require.Len(t, model.calls, 1)
			require.Contains(t, toolNames(model.calls[0].Tools), tools.CustomerSupportHandoffName)
			require.NotContains(t, model.calls[0].Messages[0].Content, agentprotocol.FeishuCustomerSupportMarker,
				"the adapter marker is emitted only by the tool executor")
			require.NotContains(t, strings.Join(messageContents(model.calls[0].Messages), "\n"), "qrcode.png")
		})
	}
}

func TestCustomerSupportDisplayProjectionDoesNotEnterColdModelHistory(t *testing.T) {
	hotModel := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall()}}
	hot := NewWithDeps(hotModel, &mockExecutor{}, nil)
	reply, err := hot.Chat(context.Background(), "请帮我转接人工客服", noopStep)
	require.NoError(t, err)
	require.Equal(t, refusal.HumanAgentTransfer, reply)
	transcript, stats := hot.LastTurnTranscript()
	require.True(t, stats.Attempted)
	require.NotEmpty(t, transcript)

	coldModel := &mockLLM{responses: []llm.ChatResponse{{Content: "我继续处理。"}}}
	cold := NewWithDeps(coldModel, &mockExecutor{}, nil)
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "请帮我转接人工客服"},
		{Role: openai.ChatMessageRoleAssistant, Content: refusal.HumanAgentTransfer, Transcript: transcript},
	})
	_, err = cold.Chat(context.Background(), "继续", noopStep)
	require.NoError(t, err)
	require.Len(t, coldModel.calls, 1)
	history := strings.Join(messageContents(coldModel.calls[0].Messages), "\n")
	require.Contains(t, history, agentprotocol.CustomerSupportHistoryCompletion)
	require.Contains(t, history, "本次未返回备用入口或工单地址")
	require.NotContains(t, history, "qrcode.png")
	require.NotContains(t, history, agentprotocol.FeishuCustomerSupportMarker)
}

func TestUnavailableSupportEntryReportStaysWithTheCentralAgent(t *testing.T) {
	const answer = "你反馈这个客服入口已满，我没有已核实的替代入口，暂时无法帮你接通人工。"
	model := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall(), {Content: answer}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	_, err := eng.Chat(context.Background(), "请帮我联系人工客服", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "上面的客服入口显示群人数已满", noopStep)
	require.NoError(t, err)
	require.Equal(t, answer, reply)
	require.NotContains(t, reply, "qrcode.png")
	require.Len(t, model.calls, 2, "entry availability is interpreted by the central Agent, not a keyword router")
	history := strings.Join(messageContents(model.calls[1].Messages), "\n")
	require.Contains(t, history, "群人数已满")
	require.Contains(t, history, agentprotocol.CustomerSupportHistoryCompletion)
	require.NotContains(t, history, "qrcode.png")
	toolJSON, err := json.Marshal(model.calls[1].Tools)
	require.NoError(t, err)
	require.Contains(t, string(toolJSON), "满员或失效时不要重复调用")
	require.Contains(t, string(toolJSON), "不编造工单菜单或接待状态")
}

func migrationReadExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"RetCode": 0, "TotalCount": float64(1), "UHostSet": []any{
			map[string]any{"UHostId": "uhost-migrating", "Name": "train-1", "State": "Migrating"},
		}},
	}}
}

// The production shape behind a swallowed answer: the Agent reads the account,
// then hands off in a later round. The read is evidence only the Agent can
// narrate, so the handoff no longer ends the turn — the Agent writes its answer
// and the channel entry is appended after it.
func TestSupportHandoffAfterAReadKeepsTheAgentsAnswer(t *testing.T) {
	const narration = "你的实例 uhost-migrating 正在系统盘迁移，迁移由平台发起，期间无法开机；具体原因需要人工核实。"
	// The production client streams every content delta; the streaming mock
	// mirrors that so the stream/persisted-reply parity below is meaningful.
	model := &streamingScriptedLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("list", "ReadCapability_resource_info", `{}`)}},
		customerSupportToolCall(),
		{Content: narration},
	}}
	eng := NewWithDeps(model, migrationReadExecutor(), nil)
	onStep, steps := collectSteps()

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "我的实例为什么在迁移？帮我转人工", onStep, ChatOptions{
		OnTextDelta: func(d string) { deltas = append(deltas, d) },
	})
	require.NoError(t, err)
	require.Equal(t, narration+"\n\n"+refusal.HumanAgentTransfer, reply)
	require.Equal(t, reply, strings.Join(deltas, ""), "the appended entry is streamed exactly as persisted")
	require.Len(t, model.calls, 3, "the handoff observation returns to the Agent for the answer")

	closing := model.calls[2]
	require.Equal(t, toolNames(model.calls[1].Tools), toolNames(closing.Tools),
		"the closing call keeps the same window, so its prompt prefix stays cacheable")
	observation := closing.Messages[len(closing.Messages)-1]
	require.Equal(t, openai.ChatMessageRoleTool, observation.Role)
	require.Equal(t, "support", observation.ToolCallID)
	require.Contains(t, observation.Content, "附上配置的客服联系入口")
	require.NotContains(t, observation.Content, "qrcode.png")
	history := strings.Join(messageContents(eng.messages), "\n")
	require.Contains(t, history, narration)
	require.NotContains(t, history, "qrcode.png", "the channel renderer never enters model history")
	require.Contains(t, (*steps)[len(*steps)-1].Message, "未确认接通或受理")
}

// A handoff batched with another tool defers the same way, whichever side of
// the batch it sits on.
func TestSupportHandoffBatchedWithAReadKeepsTheAgentsAnswer(t *testing.T) {
	const narration = "账号下只有一台实例，状态是迁移中。"
	for _, order := range [][]openai.ToolCall{
		{toolCall("list", "ReadCapability_resource_info", `{}`), toolCall("support", tools.CustomerSupportHandoffName, `{}`)},
		{toolCall("support", tools.CustomerSupportHandoffName, `{}`), toolCall("list", "ReadCapability_resource_info", `{}`)},
	} {
		model := &mockLLM{responses: []llm.ChatResponse{{ToolCalls: order}, {Content: narration}}}
		executor := migrationReadExecutor()
		eng := NewWithDeps(model, executor, nil)

		reply, err := eng.Chat(context.Background(), "看下我的实例，然后转人工", noopStep)
		require.NoError(t, err)
		require.Equal(t, narration+"\n\n"+refusal.HumanAgentTransfer, reply)
		require.Contains(t, executor.calls, "DescribeCompShareInstance", "the batch-mate still runs")
		require.Len(t, model.calls, 2)
	}
}

// A second handoff call in the same turn — batched or in a later round — asks
// for the same entry, which is delivered once.
func TestRepeatedSupportHandoffInOneTurnDeliversOneEntry(t *testing.T) {
	const narration = "实例正在迁移中。"
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("list", "ReadCapability_resource_info", `{}`),
			toolCall("support-1", tools.CustomerSupportHandoffName, `{}`),
			toolCall("support-2", tools.CustomerSupportHandoffName, `{}`),
		}},
		{ToolCalls: []openai.ToolCall{toolCall("support-3", tools.CustomerSupportHandoffName, `{}`)}},
		{Content: narration},
	}}
	eng := NewWithDeps(model, migrationReadExecutor(), nil)

	reply, err := eng.Chat(context.Background(), "看下我的实例，然后转人工", noopStep)
	require.NoError(t, err)
	require.Equal(t, narration+"\n\n"+refusal.HumanAgentTransfer, reply)
	require.Equal(t, 1, strings.Count(reply, "qrcode.png"))
	require.Len(t, model.calls, 3)
	for _, id := range []string{"support-1", "support-2", "support-3"} {
		var seen bool
		for _, message := range eng.messages {
			if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == id {
				seen = true
				require.Contains(t, message.Content, "附上配置的客服联系入口")
			}
		}
		require.True(t, seen, "every call keeps its own observation so history stays well-formed: "+id)
	}
}

// The Agent may correctly add nothing after a deferred handoff; the entry alone
// is then the reply, and model history closes on the semantic completion.
func TestDeferredSupportHandoffWithNoAgentProseDeliversTheEntryAlone(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("list", "ReadCapability_resource_info", `{}`)}},
		customerSupportToolCall(),
		{Content: ""},
	}}
	eng := NewWithDeps(model, migrationReadExecutor(), nil)

	reply, err := eng.Chat(context.Background(), "看下我的实例，然后转人工", noopStep)
	require.NoError(t, err)
	require.Equal(t, refusal.HumanAgentTransfer, reply)
	require.NotContains(t, reply, emptyReplyFallbackMessage)
	require.Equal(t, agentprotocol.CustomerSupportHistoryCompletion, eng.messages[len(eng.messages)-1].Content)
}

// The entry is what the Agent decided before the closing model call failed; it
// is still delivered, and the turn is not downgraded to an error.
func TestDeferredSupportHandoffSurvivesALaterModelFailure(t *testing.T) {
	model := &handoffThenFailingLLM{}
	eng := NewWithDeps(model, migrationReadExecutor(), nil)

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "看下我的实例，然后转人工", noopStep, ChatOptions{
		OnTextDelta: func(d string) { deltas = append(deltas, d) },
	})
	require.NoError(t, err)
	require.Equal(t, refusal.HumanAgentTransfer, reply)
	require.Equal(t, reply, strings.Join(deltas, ""))
	require.Equal(t, 4, model.calls, "the failed answer round and the tool-free closing call both fail before the entry is delivered")
	require.Equal(t, agentprotocol.CustomerSupportHistoryCompletion, eng.messages[len(eng.messages)-1].Content)
}

type handoffThenFailingLLM struct {
	calls int
}

func (m *handoffThenFailingLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls++
	switch m.calls {
	case 1:
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{toolCall("list", "ReadCapability_resource_info", `{}`)}}, nil
	case 2:
		resp := customerSupportToolCall()
		return &resp, nil
	}
	return nil, errors.New("test model failure after the handoff")
}

// The Feishu renderer defers the same way: the adapter marker follows the
// Agent's answer, so the adapter can send the answer and then the support entry.
func TestFeishuDeferredHandoffAppendsTheMarkerAfterTheAnswer(t *testing.T) {
	const narration = "文档里没有这项规则，需要平台人员核实。"
	retriever := &scriptedKnowledgeRetriever{}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("kb", "SearchKnowledge", `{"query":"账号无法登录"}`)}},
		customerSupportToolCall(),
		{Content: narration},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.ChatWithOptions(context.Background(), "账号登录不上，找人工", noopStep, ChatOptions{KnowledgeOnly: true, FeishuConsoleHandoff: true})
	require.NoError(t, err)
	require.Equal(t, narration+"\n\n"+agentprotocol.FeishuCustomerSupportMarker, reply)
	require.Len(t, model.calls, 3)
	require.NotContains(t, strings.Join(messageContents(eng.messages), "\n"), agentprotocol.FeishuCustomerSupportMarker)
}

// A deferred turn replays like any narrated turn: the cold model history holds
// the Agent's answer, never the QR markup persisted for display.
func TestDeferredSupportHandoffDisplayProjectionDoesNotEnterColdModelHistory(t *testing.T) {
	const narration = "实例正在迁移中，原因需要人工核实。"
	hotModel := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("list", "ReadCapability_resource_info", `{}`)}},
		customerSupportToolCall(),
		{Content: narration},
	}}
	hot := NewWithDeps(hotModel, migrationReadExecutor(), nil)
	reply, err := hot.Chat(context.Background(), "我的实例为什么在迁移？帮我转人工", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, "qrcode.png")
	transcript, stats := hot.LastTurnTranscript()
	require.True(t, stats.Attempted)

	coldModel := &mockLLM{responses: []llm.ChatResponse{{Content: "我继续处理。"}}}
	cold := NewWithDeps(coldModel, migrationReadExecutor(), nil)
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "我的实例为什么在迁移？帮我转人工"},
		{Role: openai.ChatMessageRoleAssistant, Content: reply, Transcript: transcript},
	})
	_, err = cold.Chat(context.Background(), "继续", noopStep)
	require.NoError(t, err)
	history := strings.Join(messageContents(coldModel.calls[0].Messages), "\n")
	require.Contains(t, history, narration)
	require.NotContains(t, history, "qrcode.png")
}

func messageContents(messages []openai.ChatCompletionMessage) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Content)
	}
	return out
}

func TestHumanAgentTransferReplyContainsSupportQR(t *testing.T) {
	require.Contains(t, refusal.HumanAgentTransfer, "ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png")
	require.True(t, strings.HasPrefix(refusal.HumanAgentTransfer, "如需人工客服协助，请扫描"))
	require.NotContains(t, refusal.HumanAgentTransfer, "已为您转接")
	require.NotContains(t, refusal.HumanAgentTransfer, "会有专人为您服务")
	require.Contains(t, refusal.HumanAgentTransfer, "不代表已接通或受理")
}
