package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

func TestCentralAgentPromptContainsOneContractAndNoLegacyWorkflowCatalog(t *testing.T) {
	text, ids := BuildSystemWithOptionsAndTrace("context", BuildOptions{MutatingToolsEnabled: true})
	require.Equal(t, []string{"identity", "scope_boundary", "behavior", "knowledge_turn_policy", "reply_style", "user_state"}, ids)
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
	// 3. do not stage the confirm card in prose before calling. B4 moved this from
	//    the behavior segment to the reply-style segment; it must still be present
	//    somewhere in the assembled prompt.
	require.Contains(t, text, "不要在动作之前先写结论或参数清单")
	// 4. missing/conflicting values come back from the call, not from interrogation.
	require.Contains(t, text, "缺失与冲突由返回结果指出")

	require.Contains(t, text, "动作建议不会直接执行")
	require.Equal(t, 1, strings.Count(text, "动作建议不会直接执行"), "shared write behavior must have one prompt source")
	require.Equal(t, 1, strings.Count(text, "只能并列列为待核查项"), "uncertain observations must have one shared rule")
	require.Equal(t, 1, strings.Count(text, "扩展只能增加候选"),
		"catalog recommendation must preserve the user's grounded baseline when expanding a lexical search")
	require.Equal(t, 1, strings.Count(text, "必须先调用对应的真实目录查询能力"),
		"knowledge snippets must not replace the live catalog for catalog recommendations")
	require.Equal(t, 1, strings.Count(text, "真实使用量或热度作为取舍依据"),
		"equally suitable catalog candidates need one evidence-based tie breaker")
	require.Equal(t, 1, strings.Count(text, "可选筛选条件只填写用户已经明确表达的条件"),
		"optional facets must not silently become user choices")
	require.NotContains(t, text, "InfiniteTalk")
	require.NotContains(t, text, "LiveTalking")
}
