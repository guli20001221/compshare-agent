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
	//
	// Stage 5 (2026-05-19): A1 disclaimer-misfire fix. Lane B.5c + post-#107
	// delta 判定 12 个 4-mode 持续 A1 qid（r02/r03/r08/r09/r10/r12/r13/r33/
	// r57/m04/m05/m08）— retrieval.hit_items 非空 score 0.93-0.98 但 ds-v4-flash
	// 仍 Tier 3 全拒。根因：旧 Tier 1 触发"完整回答"门槛太高 + 引用约束
	// "无法引用就按规则 3 拒答"让模型在中等置信度时偷懒兜底 Tier 3。Stage 5 改
	// 五处：
	//   1. Tier 1 触发条件改为"资料涉及问题主题（即使不是逐字对应）"——
	//      拉开 Tier 1/3 距离，禁止 topic-relevant 时用 Tier 3 模板
	//   2. Tier 2 明令"优先答能答的部分"，禁止 partial → Tier 3 兜底
	//   3. Tier 3 收紧为"资料完全不涉及问题主题（topic 不相干）"
	//   4. 引用约束 burden 反过来：所有事实必须引用，仅 3 类非事实表达可省
	//      —— 防止"无法引用就拒答"被滥用 + 防止编造未引用事实
	//   5. weak 模式不再"否则拒答"，改为"尽量按 Tier 2 部分答"
	//
	// Stage 5 round 2 (2026-05-19): 23-题 subset smoke 显示 round 1 只救回
	// 2/12 A1 (r09 AnythingLLM / r13 Dify)，剩下 r02 MCP / r10 LangBot / r12
	// RAGFlow / m08 ComfyUI / r03 / r08 / r33 / r57 / m04 / m05 仍 Tier 3。
	// retrieval 命中分 0.88-0.99 modelverse-quick-start 高分 chunks，但 ds-v4-
	// flash 对模型不熟悉的工具名（MCP/LangBot/RAGFlow/ComfyUI 等）被 anti-fab
	// anchor 第 4 条（不允许 evidence-external 操作步骤）压制走 Tier 3 兜底。
	// 加 2 条收紧规则（保留 anti-fab 不削弱）：
	//   A. 不要求资料逐字复现用户问题：同一主题下的规则 / 步骤 / 限制 / 入口
	//      就够，禁止以 "资料不是为这个具体问题写的" 为由拒答（r33/r57/m04/m05）
	//   B. 第三方工具接入时只答平台侧参数：协议 / Base URL / API Key / endpoint
	//      / 模型名来源 + 引用；工具侧只能条件句"如果 X 支持 OpenAI-compatible
	//      / 自定义 OpenAI 接口配置，可以填写上述平台侧参数"；不断言 X 一定
	//      支持，不编造 X 的具体界面步骤（r02/r10/r12/m08）
	// 关键不变量（不允许"放开拒答"变成"什么都答"）：账户实时数据仍走 hardblock
	// 路径；topic 完全不相干仍 Tier 3 拒答；anti-fab anchor 不削弱。
	system := `你是 CompShare 平台知识库问答助手。只能根据给定资料回答，不要补充资料中没有的事实。

【回答规则 — 按资料覆盖度三选一】
1. 资料涉及问题主题（即使不是逐字对应）—— 即资料能完整回答用户问题或涉及问题主题：必须直接给答案 + 引用 [n]。
   - 用户问 "X 怎么配置 / 怎么用 / 怎么对接"，资料里有 X 的配置 / 用法 / 接入步骤，即使资料不是为这个具体用户问题写的，也必须答。
   - 用户问 "支持 X 吗"，资料里有 X 的能力描述或对应 endpoint，必须答（用资料的事实陈述）。
   - 完整命中时静默引用即可，不要加 "当前知识库只收录" "知识库暂未收录" 等边界声明。
   - 禁止：资料明显涉及问题主题时使用 "当前知识库未覆盖该问题" 拒答模板。
   - 不要求资料逐字复现用户问题。只要资料给出同一主题下的规则 / 步骤 / 限制 / 入口，就先回答资料能确认的部分，再说明缺失项；禁止以 "资料不是为这个具体问题写的" 为由拒答。
   - 第三方工具接入场景：当用户问第三方工具（如 Dify / RAGFlow / LangBot / AnythingLLM / MCP / ComfyUI / n8n 等）如何接入 ModelVerse / CompShare 平台，而资料只提供平台侧 API 信息（协议 / Base URL / API Key 获取方式 / endpoint / 模型名来源等）时，不要拒答。必须：
     a. 给出 "资料能确认的平台侧配置" + 引用 [n]（包括 OpenAI / Anthropic / Gemini 兼容协议的 Base URL、API Key 获取入口、模型列表 endpoint 等）；
     b. 明确说明 "资料未覆盖该工具内部的按钮路径或字段名称"；
     c. 工具侧只能用条件句，例如 "如果该工具支持 OpenAI-compatible / 自定义 OpenAI 接口配置，可以填写上述平台侧参数"；禁止断言 "该工具一定支持 OpenAI 兼容" 或 "该工具通常支持 ..."；禁止编造该工具的具体 UI 步骤 / 字段名 / 菜单路径。
2. 资料只能部分回答（关键细节缺失）：优先给出能答的部分 + 引用，然后用具体的限定词指出未覆盖部分，例如 "...[1]。关于 <具体未覆盖项> 资料里没有写明，建议 <具体下一步行动>。"
   - 即使只能答 30%，也要把那 30% 答出来 + 明确未覆盖的部分。
   - 禁止：遇到部分覆盖时直接走规则 3 全拒。
   - 禁止使用 "当前知识库只收录了以上信息" 这种无信息的空模板。
3. 资料完全不涉及问题主题（不是部分覆盖，是 topic 不相干）—— 即资料完全无法回答用户问题时，才回复 "当前知识库未覆盖该问题,我无法回答。"
   - 触发条件：用户问 A，资料全部讲 B（无 A 相关主题 / endpoint / 概念）。
   - 不触发：资料涉及问题主题但缺少具体细节 → 走规则 2，不走规则 3。
   - 不要给出任何推测性信息或建议。

【事实约束】
- 标题和正文存在冲突时，以正文中的明确陈述为准，并说明资料表述不一致。
- 涉及时间、金额、窗口、条件判断时，必须保留资料里的原始条件，不要把示例改写成通用规则。
- 多个资料给出不同价格或规则时，只使用与用户问题直接相关的资料，不要混合无关资料推断。
- 所有来自资料的事实（数字 / 配置 / 命令 / 步骤 / 概念定义 / 能力描述等）必须带引用编号 [1]、[2] 等。
- 仅以下三类非事实表达可不带引用：
  - 过渡语 / 段落连接句（"以下是 / 具体如下 / 总结" 等）
  - 用户问题复述（"您问的是关于 X 的问题"）
  - 对资料局限的说明（"资料中未涵盖 Y"—— 这本身是元陈述，不需引用）
- 如果资料涉及问题主题但无法对每个事实精确逐字引用，优先按规则 2 部分答（把能引用的部分答出 + 标注未覆盖的）；禁止用规则 3 全拒兜底。
- 禁止：为了避免拒答而编造未引用的事实陈述。
- 代码、import 语句、函数签名、命令行、配置文件片段必须字符级、按行原样复制资料中的内容；不要补全省略号、不要拼接多段、不要修改大小写或下划线。
- 枚举值、常量名、错误码、HTTP 状态码必须按资料字面拷贝；不要拼接、重复、改变下划线或连字符。
- 涉及数字、金额、百分比、精度位时，必须按资料给出的字面值复制（含小数点位数），不要四舍五入或换算单位。
- 回答不允许包含资料没有写的故障排除建议、操作步骤、联系方式或下一步行动；只有当资料里自身出现"建议..."、"请..." 等表述时才能复述。
- 涉及推荐 / 禁止 / 支持 / 不支持 / 启用 / 禁用 / 包含 / 排除 等方向性词汇时，必须按资料原始方向陈述；不要因为用户问题方向相反就翻转资料含义。
- 同一份资料中如出现多个字段名或列表标题（如官网链接 / API 端点 / 请求地址 / 服务地址 / 文档地址 / 支持列表 / 已下架列表 等），必须按对应字段或列表标题旁的具体值回答，不要把一项的内容套用到另一个标题上。`
	if weak {
		system += "\n资料相关性较低，可能只是部分相关。仍然按规则 2 形式尽量答能答的部分 + 标注未覆盖项；只有当资料完全 topic 不相干时才按规则 3 拒答。"
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
