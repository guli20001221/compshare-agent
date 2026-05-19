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

func TestBuildRAGMessagesPromptEncodesA1AntiMisfirePatterns(t *testing.T) {
	// Stage 5 (2026-05-19): A1 disclaimer-misfire fix. Lane B.5c + post-#107
	// delta judged 12 4-mode-persistent A1 qids (r02/r03/r08/r09/r10/r12/r13/
	// r33/r57/m04/m05/m08) where retrieval.hit_items was non-empty with
	// score 0.93-0.98 but ds-v4-flash returned the Tier-3 hard-refusal
	// template anyway. Root cause: previous Tier 1 trigger required "完整
	// 回答" which the model judged too strictly; previous citation rule
	// "无法引用就按规则 3 拒答" let the model bail out on partial coverage.
	// Stage 5 widens Tier 1 to "资料涉及问题主题（即使不是逐字对应）" and
	// explicitly forbids using the Tier-3 template when topic-relevant
	// evidence is present. Each phrase here maps to a specific failure
	// mode observed in the 12 qids; future edits cannot silently weaken
	// the directives that fix the misfire.
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "test", Content: "test content"},
	}, false, false)
	system := messages[0].Content

	for _, want := range []string{
		// Widened Tier 1 trigger — topic-relevance is enough, not byte-exact.
		"资料涉及问题主题",
		"即使不是逐字对应",
		// Concrete examples that bind the trigger to the 12 misfire qids
		// (MCP/ComfyUI/embedding/AnythingLLM/LangBot/RAGFlow/Dify "怎么
		// 配置 / 怎么用 / 怎么对接" + "支持 X 吗").
		"怎么配置",
		"怎么对接",
		"支持 X 吗",
		// Forbidden behaviour: do not use Tier-3 template on topic-relevant
		// evidence. This is the rule the 12 misfire qids violated.
		"禁止：资料明显涉及问题主题时使用",
		"拒答模板",
		// Forbidden behaviour: do not fall through partial coverage to Tier 3.
		"禁止：遇到部分覆盖时直接走规则 3 全拒",
		// Tier 3 contracted to "topic 不相干" only.
		"完全不涉及问题主题",
		"topic 不相干",
		"用户问 A",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("A1 anti-misfire pattern %q missing in system prompt:\n%s", want, system)
		}
	}
}

func TestBuildRAGMessagesPromptEncodesBurdenReversedCitation(t *testing.T) {
	// Stage 5 (2026-05-19): Citation burden inverted. Previous rule
	// "回答中的每个事实必须带引用编号；如果无法引用，按规则 3 拒答"
	// gave the model permission to bail out to Tier 3 whenever any
	// single fact couldn't be cited; combined with the strict Tier 1
	// trigger this produced the 12 A1 misfires. Stage 5 says: all
	// facts MUST cite, with three explicitly-named non-fact escape
	// hatches (transitions / question-restatement / meta-statements
	// about source limits). Critically the burden-reversed rule must
	// be paired with an explicit ban on fabricating uncited facts to
	// avoid "release refusal" turning into "release fabrication"
	// (user 2026-05-19 callout in brief §1).
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "test", Content: "test content"},
	}, false, false)
	system := messages[0].Content

	for _, want := range []string{
		// Default: all material-derived facts must cite.
		"所有来自资料的事实",
		"必须带引用编号",
		// Three explicitly-named non-fact escape hatches.
		"三类非事实表达可不带引用",
		"过渡语",
		"用户问题复述",
		"元陈述",
		// Partial-answer route is preferred over Tier-3 bail.
		"无法对每个事实精确逐字引用",
		"优先按规则 2 部分答",
		"禁止用规则 3 全拒兜底",
		// Hard ban on fabricated uncited facts.
		"禁止：为了避免拒答而编造未引用的事实陈述",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("burden-reversed citation rule %q missing in system prompt:\n%s", want, system)
		}
	}

	// Negative assertion: the old "无法引用就按规则 3 拒答" clause must be
	// gone. Leaving it in would re-enable the misfire path.
	for _, mustNotContain := range []string{
		"如果无法引用，按规则 3 拒答",
		"如果无法引用就按规则 3 拒答",
	} {
		if strings.Contains(system, mustNotContain) {
			t.Fatalf("legacy bail-out clause %q must be removed from prompt:\n%s", mustNotContain, system)
		}
	}
}

func TestBuildRAGMessagesWeakModeAllowsPartialAnswer(t *testing.T) {
	// Stage 5 (2026-05-19): weak retrieval mode previously appended
	// "只有在资料确实能回答时才回答，否则拒答" which compounded with
	// the strict Tier-1 trigger to force Tier-3 fallbacks on partial-
	// coverage evidence. Stage 5 rewrites the weak clause so it falls
	// back to rule 2 (partial answer with explicit gaps) rather than
	// to rule 3 (hard refusal). retry mode is unchanged because it
	// encodes the cited_guard contract.
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "test", Content: "test content"},
	}, true /*weak*/, false)
	system := messages[0].Content

	for _, want := range []string{
		"资料相关性较低",
		"可能只是部分相关",
		// Weak mode now routes through rule 2, not rule 3.
		"按规则 2 形式尽量答能答的部分",
		"完全 topic 不相干时才按规则 3 拒答",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("weak-mode partial-answer rewrite missing %q in system prompt:\n%s", want, system)
		}
	}

	// Negative assertion: the old bail-out "否则拒答" tail must not survive.
	if strings.Contains(system, "否则拒答") {
		t.Fatalf("legacy weak-mode bail-out '否则拒答' must be removed:\n%s", system)
	}
}

func TestBuildRAGMessagesPromptAllowsNonByteExactTopicMatch(t *testing.T) {
	// Stage 5 round 2 (2026-05-19): the round-1 23-Q subset smoke showed
	// ds-v4-flash still refused r33/r57/m04/m05 despite topic-relevant
	// hits, because the model insisted on byte-exact question/evidence
	// alignment. Round 2 adds an explicit clause that lifts the byte-exact
	// requirement and forbids the "资料不是为这个具体问题写的" bail-out.
	// Without this clause the four non-third-party A1 misfires (r33 Mac
	// RDP, r57 Windows IME, m04 spot vs exclusive, m05 spot lease) cannot
	// be saved by prompt alone.
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "t", Content: "c"},
	}, false, false)
	system := messages[0].Content

	for _, want := range []string{
		"不要求资料逐字复现用户问题",
		"同一主题下的规则 / 步骤 / 限制 / 入口",
		"先回答资料能确认的部分",
		"禁止以 \"资料不是为这个具体问题写的\" 为由拒答",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("non-byte-exact topic match clause %q missing in system prompt:\n%s", want, system)
		}
	}
}

func TestBuildRAGMessagesPromptEncodesThirdPartyToolIntegrationPattern(t *testing.T) {
	// Stage 5 round 2 (2026-05-19): the round-1 23-Q subset smoke showed
	// ds-v4-flash refused r02 MCP / r10 LangBot / r12 RAGFlow / m08 ComfyUI
	// despite high-score modelverse-quick-start hits, because the model
	// treats "third-party tool internal UI steps" as evidence-external and
	// bails to Tier 3 under anti-fab anchor #4. Round 2 explicitly carves
	// out the third-party-tool-integration sub-pattern: answer the
	// platform-side facts (protocol / Base URL / API Key / endpoint),
	// disclose the tool-side gap, and only emit tool-side claims as
	// conditional statements ("if the tool supports OpenAI-compat ...").
	// This must NOT weaken anti-fab anchor #4 — the rule forbids asserting
	// that the tool supports a protocol or fabricating its UI steps.
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "t", Content: "c"},
	}, false, false)
	system := messages[0].Content

	for _, want := range []string{
		// Sub-pattern trigger naming the concrete tools observed in misfire.
		"第三方工具接入场景",
		"Dify / RAGFlow / LangBot / AnythingLLM / MCP / ComfyUI / n8n",
		// Platform-side facts that must be answered + cited.
		"资料能确认的平台侧配置",
		"OpenAI / Anthropic / Gemini 兼容协议",
		"API Key 获取入口",
		"模型列表 endpoint",
		// Tool-side gap must be explicitly disclosed.
		"资料未覆盖该工具内部的按钮路径或字段名称",
		// Conditional-statement constraint on tool-side claims.
		"工具侧只能用条件句",
		"OpenAI-compatible / 自定义 OpenAI 接口配置",
		// Anti-fab guards — must not assert tool support or fabricate UI.
		"禁止断言",
		"该工具一定支持 OpenAI 兼容",
		"该工具通常支持",
		"禁止编造该工具的具体 UI 步骤",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("third-party-tool integration pattern %q missing in system prompt:\n%s", want, system)
		}
	}
}

func TestBuildRAGMessagesRetryModeUnchanged(t *testing.T) {
	// Stage 5 (2026-05-19): retry mode encodes the cited_guard contract
	// (retry-no-cite tail). The Stage 5 prompt rewrite explicitly does
	// not touch retry — modifying retry would change runtime cited
	// behaviour which is not in scope (memory feedback_eval_must_reflect_
	// runtime_no_coercion).
	messages := BuildRAGMessages("test q", []RAGReference{
		{Number: 1, Title: "test", Content: "test content"},
	}, false, true /*retry*/)
	system := messages[0].Content

	for _, want := range []string{
		"上一次回答缺少引用",
		"必须带 [1]、[2] 这样的引用编号",
		"如果无法引用，直接拒答",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("retry-mode clause %q must remain intact:\n%s", want, system)
		}
	}
}
