package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

func TestCentralAgentPromptContainsOneContractAndNoLegacyWorkflowCatalog(t *testing.T) {
	text, ids := BuildSystemWithOptionsAndTrace("context", BuildOptions{MutatingToolsEnabled: true})
	require.Equal(t, []string{"identity", "scope_boundary", "behavior", "tool_observation_contract", "knowledge_turn_policy", "reply_style", "user_state"}, ids)
	for _, action := range workflow.RegisteredWorkflowActions() {
		require.NotContains(t, text, action)
	}
	require.Equal(t, 1, strings.Count(text, "需要平台文档或新的技术证据时再检索"))
	require.Contains(t, text, "本轮唯一的业务判断者")
	// The shared write-proposal contract lives HERE and only here — P7 deleted the
	// per-tool restatement, so a Request tool's description carries only its own
	// semantic boundary. These assert the four invariants of that contract, pinned
	// to the B4 wording (2026-07-21 rephrase; the strings changed, the contract did
	// not). Keep them string-pinned: a silent rewrite of any one of them is a
	// behavior change to every write path at once.
	//
	// 1. do-request vs question: act on "do X", ANSWER how-to/rules/fee/feasibility.
	require.Contains(t, text, "用户问的是怎么做、规则、计费或可行性时，直接回答")
	// 2. submit now with whatever is already known, rather than gathering first.
	require.Contains(t, text, "适用就立即提交，带上此刻已明确的值")
	require.Contains(t, text, "即使部分值还只是用户的口头说法或尚未确定，也照样提交")
	require.Contains(t, text, "无需提交前自己查全或逐条问全")
	// 3. do not stage the confirm card in prose before calling. B4 moved this from
	//    the behavior segment to the reply-style segment; it must still be present
	//    somewhere in the assembled prompt.
	require.Contains(t, text, "不要在动作之前先写结论或参数清单")
	// 4. missing/conflicting values come back from the call, not from interrogation.
	require.Contains(t, text, "缺失与冲突由返回结果指出")

	require.Contains(t, text, "动作建议不会直接执行")
	require.Contains(t, text, "只有 Request* 调用成功，才能声称已发起操作或打开确认卡")
	require.Contains(t, text, "不得用普通文字模拟确认卡")
	require.Equal(t, 1, strings.Count(text, "动作建议不会直接执行"), "shared write behavior must have one prompt source")
	require.Equal(t, 1, strings.Count(text, "只能并列列为待核查项"), "uncertain observations must have one shared rule")
	require.Contains(t, text, "不做概率排序，也不得据此排除其他层级的问题")
	require.Equal(t, 1, strings.Count(text, "扩展只能增加候选"),
		"catalog recommendation must preserve the user's grounded baseline when expanding a lexical search")
	require.Equal(t, 1, strings.Count(text, "平台当前目录、可用性、状态、价格、库存、热度和实例详情"),
		"all current platform facts must share one source-selection rule")
	require.Equal(t, 1, strings.Count(text, "对应能力失败或没有返回候选时"),
		"a failed live read must not be replaced with a remembered catalog object")
	require.NotContains(t, text, "semantic_queries",
		"one capability's parameter name belongs in its schema, not the shared Agent prompt")
	require.Equal(t, 1, strings.Count(text, "真实使用量或热度作为取舍依据"),
		"equally suitable catalog candidates need one evidence-based tie breaker")
	require.Equal(t, 1, strings.Count(text, "可选筛选条件只填写用户已经明确表达的条件"),
		"optional facets must not silently become user choices")
	require.Equal(t, 1, strings.Count(text, "非写目标的目录对象例外"),
		"historical catalog ids may be carried only through the narrow non-target exception")
	require.Contains(t, text, "近期完整对话已逐字展示精确 ID 和来源、当前请求承接它时")
	require.Contains(t, text, "仍须实时核验和确认",
		"carrying a historical catalog id must never imply availability or user approval")

	// P2 is additive: it teaches the model how to read the structured observation
	// envelope without weakening the B4 write-authorization wording above.
	require.Contains(t, text, "根级 status、data、error.code、retryable、next_step、meta 为准")
	require.Contains(t, text, "NO_CITABLE_EVIDENCE")
	require.Equal(t, 1, strings.Count(text, "## 工具结果"))
	// A call the model itself malformed needs nothing from the user. The
	// exception is written INTO the needs_input clause rather than after it:
	// stated separately, the generic "补问缺字段" rule is what the model reads
	// first, and it turns the model's own JSON mistake into a question for a
	// user who supplied nothing wrong.
	require.Contains(t, text, "needs_input 补问缺字段，但 next_step=correct_tool_call 时用户无需补充")
	require.Contains(t, text, "改正参数后重发同一次调用，不要提问")
	require.NotContains(t, text, "InfiniteTalk")
	require.NotContains(t, text, "LiveTalking")
}
