package engine

import (
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/require"
)

func TestPlannerIntentShouldUseKnowledgeQA_GenericResourceSemantics(t *testing.T) {
	require.True(t, plannerIntentShouldUseKnowledgeQA(intent.IntentStockAvailability, "一直暂无资源 是什么情况"))
	require.True(t, plannerIntentShouldUseKnowledgeQA(intent.IntentStockAvailability, "Normal 状态是不是说明一定有库存"))

	require.False(t, plannerIntentShouldUseKnowledgeQA(intent.IntentStockAvailability, "4090 有没有货"))
	require.False(t, plannerIntentShouldUseKnowledgeQA(intent.IntentStockAvailability, "H20 SoldOut 是不是没货"))
}

func TestPlannerIntentShouldUseKnowledgeQA_PlanPackageManagement(t *testing.T) {
	require.True(t, plannerIntentShouldUseKnowledgeQA(intent.IntentOperationLifecycle, "删除 Coding Plan 包"))
	require.True(t, plannerIntentShouldUseKnowledgeQA(intent.IntentOperationLifecycle, "取消 Coding Plan 套餐能退款吗"))

	require.False(t, plannerIntentShouldUseKnowledgeQA(intent.IntentOperationLifecycle, "删除 uhost-abc123"))
	require.False(t, plannerIntentShouldUseKnowledgeQA(intent.IntentOperationLifecycle, "给这台实例取消定时关机"))
}

func TestProductKnowledgeQuestionShouldUseTerminalRetrieval(t *testing.T) {
	require.True(t, productKnowledgeQuestionShouldUseTerminalRetrieval("磁盘空间是如何收费的？100GB 原始空间免费吗"))
	require.True(t, productKnowledgeQuestionShouldUseTerminalRetrieval("删除 Coding Plan 包"))
	require.True(t, productKnowledgeQuestionShouldUseTerminalRetrieval("取消 Coding Plan 套餐能退款吗"))
	require.True(t, productKnowledgeQuestionShouldUseTerminalRetrieval("一直暂无资源 是什么情况"))
	require.True(t, productKnowledgeQuestionShouldUseTerminalRetrieval("Normal 状态是不是说明一定有库存"))

	require.False(t, productKnowledgeQuestionShouldUseTerminalRetrieval("4090 有没有货"))
	require.False(t, productKnowledgeQuestionShouldUseTerminalRetrieval("帮我重启这台实例"))
}
