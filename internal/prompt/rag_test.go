package prompt

import (
	"strings"
	"testing"
)

func TestBuildRAGMessagesPreservesConflictAndConditionRules(t *testing.T) {
	messages := BuildRAGMessages("Coding Plan 的用量周期是怎么计算的", []RAGReference{
		{
			Number:  1,
			Title:   "额度重置机制：滚动 5 小时窗口",
			Content: "Coding Plan 采用固定 5 小时窗口刷新额度。",
		},
	}, false, false)
	if len(messages) == 0 {
		t.Fatal("expected messages")
	}
	system := messages[0].Content
	for _, want := range []string{
		"标题和正文存在冲突",
		"以正文中的明确陈述为准",
		"保留资料里的原始条件",
		"不要把示例改写成通用规则",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing %q: %s", want, system)
		}
	}
}
