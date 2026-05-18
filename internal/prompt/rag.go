package prompt

import (
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

type RAGReference struct {
	Number  int
	Title   string
	Content string
}

func BuildRAGMessages(question string, refs []RAGReference, weak bool, retry bool) []openai.ChatCompletionMessage {
	// PR-RAG-Prompt-Disclaimer-Fix (2026-05-17): three-tier disclaimer strategy.
	// Pre-fix the prompt told the LLM to add "当前知识库只收录了以下信息" whenever
	// coverage was partial, but the trigger was so vague that complete-hit
	// answers also routinely carried the disclaimer (Phase 0.C measured 80%
	// of complete_hit answers carrying an unnecessary disclaimer). Split into
	// 3 explicit branches so the LLM stops emitting a disclaimer on complete
	// hits and uses a specific-gap natural wording on partial hits.
	// MUST stay in sync with scripts/rag_w0/evaluate_answers.py _answer_prompt
	// (memory feedback_eval_target_must_match_runtime_path).
	//
	// Step 6b (2026-05-17): six anti-fabrication anchor bullets appended to
	// 【事实约束】. Step 5 controlled eval flagged 4 real fab cases under
	// ds-v4-flash + BM25 (token corruption / extrapolation / direction
	// misread) and PR #94 hybrid eval flagged 2 more under ds-v4-pro + hybrid
	// (field-vs-value confusion / list-role flip). Anchors prescribe
	// character-level literal copy for code+enum+numbers, ban evidence-external
	// suggestions, lock direction-words to evidence wording, and bind field /
	// list labels to their adjacent values so the LLM cannot transpose them.
	system := `你是 CompShare 平台知识库问答助手。只能根据给定资料回答，不要补充资料中没有的事实。

【回答规则 — 按资料覆盖度三选一】
1. 资料能完整回答用户问题：直接给答案 + 引用 [n]，不要加 "当前知识库只收录" "知识库暂未收录" 等边界声明。完整命中时静默引用即可。
2. 资料只能部分回答（关键细节缺失）：用具体的限定词指出未覆盖部分，例如 "...[1]。关于 <具体未覆盖项> 资料里没有写明，建议 <具体下一步行动>。" 禁止使用 "当前知识库只收录了以上信息" 这种无信息的空模板。
3. 资料完全无法回答用户问题：回复 "当前知识库未覆盖该问题,我无法回答。" 不要给出任何推测性信息或建议。

【事实约束】
- 标题和正文存在冲突时，以正文中的明确陈述为准，并说明资料表述不一致。
- 涉及时间、金额、窗口、条件判断时，必须保留资料里的原始条件，不要把示例改写成通用规则。
- 多个资料给出不同价格或规则时，只使用与用户问题直接相关的资料，不要混合无关资料推断。
- 回答中的每个事实必须带引用编号 [1]、[2] 等；如果无法引用，按规则 3 拒答。
- 代码、import 语句、函数签名、命令行、配置文件片段必须字符级、按行原样复制资料中的内容；不要补全省略号、不要拼接多段、不要修改大小写或下划线。
- 枚举值、常量名、错误码、HTTP 状态码必须按资料字面拷贝；不要拼接、重复、改变下划线或连字符。
- 涉及数字、金额、百分比、精度位时，必须按资料给出的字面值复制（含小数点位数），不要四舍五入或换算单位。
- 回答不允许包含资料没有写的故障排除建议、操作步骤、联系方式或下一步行动；只有当资料里自身出现"建议..."、"请..." 等表述时才能复述。
- 涉及推荐 / 禁止 / 支持 / 不支持 / 启用 / 禁用 / 包含 / 排除 等方向性词汇时，必须按资料原始方向陈述；不要因为用户问题方向相反就翻转资料含义。
- 同一份资料中如出现多个字段名或列表标题（如官网链接 / API 端点 / 请求地址 / 服务地址 / 文档地址 / 支持列表 / 已下架列表 等），必须按对应字段或列表标题旁的具体值回答，不要把一项的内容套用到另一个标题上。`
	if weak {
		system += "\n资料相关性较低；只有在资料确实能回答时才回答，否则拒答。"
	}
	if retry {
		system += "\n上一次回答缺少引用。必须带 [1]、[2] 这样的引用编号；如果无法引用，直接拒答。"
	}

	var user strings.Builder
	user.WriteString("用户问题：\n")
	user.WriteString(strings.TrimSpace(question))
	user.WriteString("\n\n资料：\n")
	for _, ref := range refs {
		number := ref.Number
		if number <= 0 {
			number = 1
		}
		user.WriteString(fmt.Sprintf("[%d] %s\n", number, strings.TrimSpace(ref.Title)))
		user.WriteString(strings.TrimSpace(ref.Content))
		user.WriteString("\n\n")
	}
	user.WriteString("请基于以上资料回答。")

	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: system},
		{Role: openai.ChatMessageRoleUser, Content: user.String()},
	}
}
