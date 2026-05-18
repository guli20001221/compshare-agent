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

func TestBuildRAGMessagesPromptEncodesThreeTierDisclaimerStrategy(t *testing.T) {
	// PR-RAG-Prompt-Disclaimer-Fix (2026-05-17): the system prompt must
	// explicitly express all three branches so the LLM stops adding empty
	// "当前知识库只收录" disclaimers to complete-hit answers. Phase 0.C
	// measured 80% of complete-hit answers carrying an unnecessary disclaimer
	// under the previous prompt; the rule-1 "strip" instruction is the fix.
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "test", Content: "test content"},
	}, false, false)
	system := messages[0].Content

	// Rule 1 — complete hit must strip the disclaimer.
	for _, want := range []string{
		"完整回答",
		"直接给答案",
		"不要加",
		// The exact phrases that previously leaked into complete-hit answers
		// must be named in the prompt's forbidden list, not just discouraged.
		"当前知识库只收录",
		"知识库暂未收录",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("rule-1 (complete hit -> strip) missing %q in system prompt:\n%s", want, system)
		}
	}

	// Rule 2 — partial hit must use specific-gap natural wording, not the
	// empty template.
	for _, want := range []string{
		"部分回答",
		"具体的限定词",
		// "禁止" specifically anchored to the empty template phrase.
		"禁止",
		"无信息",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("rule-2 (partial hit -> natural wording) missing %q in system prompt:\n%s", want, system)
		}
	}

	// Rule 3 — no hit must use the pure-refusal template (preserved).
	for _, want := range []string{
		"无法回答用户问题",
		"当前知识库未覆盖该问题",
		"我无法回答",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("rule-3 (no hit -> pure refusal) missing %q in system prompt:\n%s", want, system)
		}
	}
}

func TestBuildRAGMessagesPromptEncodesAntiFabricationAnchors(t *testing.T) {
	// Step 6b (2026-05-17): 6 anti-fabrication anchor bullets must be present
	// in the system prompt. Step 5 controlled eval flagged 4 real fab cases
	// under ds-v4-flash + BM25 and PR #94 hybrid eval flagged 2 more under
	// ds-v4-pro + hybrid; collapsing the fab cap (≤0.5%) is a hard contract
	// (memory feedback_hard_contractual_gates_binary). The 6 anchors prescribe
	// character-level literal copy for code+enum+numbers, ban evidence-external
	// suggestions, lock direction-words to evidence wording, and bind field /
	// list labels to their adjacent values — each anchor maps to a fab pattern
	// observed in the controlled or hybrid eval. Test pins one identifying
	// phrase per anchor so a future edit cannot silently drop a clause.
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "test", Content: "test content"},
	}, false, false)
	system := messages[0].Content

	anchors := map[string]string{
		"code / import character-level literal copy (0170 token corruption)":    "字符级、按行原样复制资料",
		"enum / HTTP status code literal copy (0267 token corruption)":          "枚举值、常量名、错误码、HTTP 状态码必须按资料字面拷贝",
		"numeric / amount literal copy (defensive against 0100 math pattern)":   "字面值复制（含小数点位数）",
		"no evidence-external troubleshooting / suggestions (0259 extrapolate)": "故障排除建议、操作步骤、联系方式或下一步行动",
		"direction-word fidelity (0300 logical direction misread)":              "方向性词汇时，必须按资料原始方向陈述",
		"field-name / list-title binding (0020 endpoint, 0028 deprecated list)": "字段或列表标题旁的具体值",
	}
	for purpose, phrase := range anchors {
		if !strings.Contains(system, phrase) {
			t.Fatalf("anti-fab anchor for %s missing phrase %q in system prompt:\n%s", purpose, phrase, system)
		}
	}
}
