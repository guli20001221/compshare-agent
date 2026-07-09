package intent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildSystemPromptIncludesRouteDispatchSchemaFields(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"resource_info",
		"monitor_query",
		"confidence",
		"target_refs",
		"source_span",
		"knowledge_qa",
		// Routing Registry v1 enum labels (PR A 2026-05-18) must appear in
		// the system prompt enum line so the LLM can emit them as intents.
		"gpu_specs_query",
		"stock_availability",
		"image_list",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing %q:\n%s", fragment, prompt)
		}
	}
	for _, deprecated := range []string{"required_tools", "retrieval.enabled", "hard_block_hint"} {
		if strings.Contains(prompt, deprecated) {
			t.Fatalf("system prompt still asks planner for deprecated field %q:\n%s", deprecated, prompt)
		}
	}
	for _, staleLabel := range staleNonASCIIPlannerLabels() {
		if strings.Contains(prompt, staleLabel) {
			t.Fatalf("system prompt contains stale non-ASCII label %q:\n%s", staleLabel, prompt)
		}
	}
}

func TestBuildSystemPromptIntentEnumMatchesRuntimeIntents(t *testing.T) {
	got := allowedIntentEnumFromPrompt(buildSystemPrompt())
	want := make(map[string]bool, len(RuntimeIntents()))
	for _, i := range routerRuntimeIntents(false) {
		want[string(i)] = true
	}
	if len(got) != len(want) {
		t.Fatalf("prompt intent enum size = %d, want %d; got=%v", len(got), len(want), got)
	}
	for label := range want {
		if !got[label] {
			t.Fatalf("prompt intent enum missing runtime intent %q; got=%v", label, got)
		}
	}
	for label := range got {
		if !want[label] {
			t.Fatalf("prompt intent enum contains non-runtime intent %q; got=%v", label, got)
		}
	}
}

func TestBuildSystemPromptUnifiedCreateIncludesCreateInstance(t *testing.T) {
	got := allowedIntentEnumFromPrompt(buildSystemPromptWithUnifiedCreate(true))
	assert.True(t, got[string(IntentCreateInstance)], "unified prompt must expose create_instance")
	assert.False(t, allowedIntentEnumFromPrompt(buildSystemPrompt())[string(IntentCreateInstance)], "default prompt must not expose gated create_instance")
	examples := promptExampleJSONLines(buildSystemPromptWithUnifiedCreate(true))
	found := false
	for _, example := range examples {
		plan, err := parsePlanJSON(example)
		if err == nil && plan.Intent == IntentCreateInstance {
			found = true
			break
		}
	}
	assert.True(t, found, "unified prompt must teach at least one create_instance example")
}

func TestBuildSystemPromptKeepsDeployAdviceOutOfDeployModel(t *testing.T) {
	for _, prompt := range []string{buildSystemPrompt(), buildSystemPromptWithUnifiedCreate(true)} {
		for _, fragment := range []string{
			"Route recommendation, how-to, price, configuration-sizing, comparison, or feasibility questions about deployment to knowledge_qa, pricing_query, or gpu_specs_query as appropriate; do not use deploy_model.",
			"Classify execution requests that need a create confirmation flow as deploy_model.",
		} {
			if !strings.Contains(prompt, fragment) {
				t.Fatalf("system prompt missing deploy command-only boundary %q:\n%s", fragment, prompt)
			}
		}
	}
}

func TestBuildSystemPromptDoesNotEmitRemovedIntentLabels(t *testing.T) {
	prompt := buildSystemPrompt()
	enum := allowedIntentEnumFromPrompt(prompt)
	for _, legacy := range []string{"recommendation", "mixed_diagnosis_kb", "mixed_billing_kb"} {
		if enum[legacy] {
			t.Fatalf("system prompt enum should not ask planner to emit removed intent %q:\n%s", legacy, prompt)
		}
	}
	for _, legacy := range []string{"mixed_diagnosis_kb", "mixed_billing_kb"} {
		if strings.Contains(prompt, legacy) {
			t.Fatalf("system prompt should not mention removed intent label %q:\n%s", legacy, prompt)
		}
	}
}

func allowedIntentEnumFromPrompt(prompt string) map[string]bool {
	const prefix = "Allowed intent enum:"
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		rest = strings.TrimSuffix(rest, ".")
		out := map[string]bool{}
		for _, part := range strings.Split(rest, ",") {
			label := strings.TrimSpace(part)
			if label != "" {
				out[label] = true
			}
		}
		return out
	}
	return nil
}

func TestBuildSystemPromptExamplesParse(t *testing.T) {
	examples := promptExampleJSONLines(buildSystemPrompt())
	// 33 grouped base examples (20 legacy + 3 added by #34a 2026-05-18 for
	// comparison / yes-no feasibility / procedure-description knowledge_qa
	// coverage + 2 added by #60 2026-05-20 for concept-Q-with-monitor-trigger-
	// word and third-party-tool-config-jargon knowledge_qa coverage; two old
	// stock-to-unknown examples were replaced when stock_availability became
	// a route + 2 added 2026-05-20 for personal billing complaint
	// (billing_instance) stable routing — see Q04 N=5 jitter check + 6 added
	// by R3-A1 2026-05-24 for modelverse model-API coverage so planner routes
	// "Suno/Vidu/flux/gpt-image/minimax-speech API 怎么调" to knowledge_qa
	// instead of unknown, unblocking RAG for the 46 modelverse chunks shipped
	// in PR #165) + the sum of `planner_examples` across all routes/*.md
	// frontmatter (PR A Registry v1 + later). A route may declare 1+
	// examples — early routes used exactly 1; PR #3 (pricing_query) uses
	// 2 to anchor the public-product-pricing vs personal-billing boundary
	// against the billing_instance one-shots. The example count is computed
	// below so adding a new route OR extending an existing one's examples
	// auto-updates.
	routeExampleCount := 0
	for _, m := range skillRegistryRouteMetadata() {
		routeExampleCount += len(m.PlannerExamples)
	}
	// PR1 hotfix Bug 1 (2026-05-28): bumped from 19 → 20 with the ZERO-target
	// operation_lifecycle anchor for bare "帮我关机" classification.
	// PR2.5 (2026-05-28): bumped from 20 → 24 with 4 Chinese resource_info
	// anchors (我有哪些实例 / 列出我的机器 / 正在运行的实例 / 我有几台机器)
	// to close the bare-inventory ZERO-target gap.
	// disk_info (2026-05-29): bumped from 24 → 28 with 4 disk_info anchors
	// (我有哪些数据盘 / 我的磁盘列表 / uhost-X 挂了哪些盘 / 我账号下有哪些云盘)
	// — upstream API has no list-disk action, reuse DescribeCompShareInstance.DiskSet.
	// deploy_model (B8.3, 2026-05-31): bumped from 28 → 32 with 4 workload-first
	// deploy anchors (部署 Qwen2.5-32B / 跑数字人 / 搭 ComfyUI 环境 / 部署 Llama3 推理).
	// custom-image workflow (Phase 3, 2026-06-02): bumped from 32 → 34 with
	// 2 operation_lifecycle anchors for saving an instance/environment as a
	// custom image.
	// diagnosis recall fix (2026-06-03): bumped from 34 → 37 with 3 no-target
	// symptom anchors for port unreachable, GPU not found, and SSH timeout.
	// G1 router consolidation (2026-07-09): +2 SSH boundary anchors
	// (cannot-connect -> diagnosis, disconnect/how-to -> knowledge_qa);
	// only the diagnosis anchor changes this rendered JSON count because
	// knowledge_qa is a compact example group.
	// R2b Phase B (2026-06-29): removed the 2 spec-first create anchors from
	// operation_lifecycle after create_instance became the default create entry.
	if got, want := len(examples), 38+routeExampleCount; got != want {
		t.Fatalf("prompt examples count = %d, want %d; examples=%v", got, want, examples)
	}
	for _, example := range examples {
		plan, err := parsePlanJSON(example)
		if err != nil {
			t.Fatalf("prompt example does not parse as IntentPlan JSON: %v\n%s", err, example)
		}
		if plan.SchemaVersion != SchemaVersion {
			t.Fatalf("prompt example schema_version = %q, want %q", plan.SchemaVersion, SchemaVersion)
		}
		if plan.Intent == "" {
			t.Fatalf("prompt example missing intent: %s", example)
		}
		if plan.Confidence <= 0 || plan.Confidence > 1 {
			t.Fatalf("prompt example confidence = %v, want (0,1]: %s", plan.Confidence, example)
		}
		for _, deprecated := range []string{"required_tools", "retrieval", "hard_block_hint"} {
			if strings.Contains(example, deprecated) {
				t.Fatalf("prompt example still contains deprecated field %q: %s", deprecated, example)
			}
		}
	}
}

func TestPlannerPromptExamplesGroupedByIntentWithSource(t *testing.T) {
	groups := routerPromptExampleGroups()
	if len(groups) < 5 {
		t.Fatalf("expected planner examples to be split into intent groups, got %d groups", len(groups))
	}
	total := 0
	seen := map[Intent]bool{}
	counts := map[Intent]int{}
	for _, group := range groups {
		if group.Intent == "" {
			t.Fatalf("planner example group missing intent: %+v", group)
		}
		if strings.TrimSpace(group.Source) == "" {
			t.Fatalf("planner example group %q missing PR/source note", group.Intent)
		}
		if len(group.Examples) == 0 {
			t.Fatalf("planner example group %q has no examples", group.Intent)
		}
		seen[group.Intent] = true
		counts[group.Intent] = len(group.Examples)
		total += len(group.Examples)
		for _, example := range group.Examples {
			if strings.TrimSpace(example.Source) == "" {
				t.Fatalf("planner example %q in group %q missing source note", example.Question, group.Intent)
			}
			plan, err := parsePlanJSON(example.PlanJSON)
			if err != nil {
				t.Fatalf("planner example %q does not parse: %v", example.Question, err)
			}
			if plan.Intent != group.Intent {
				t.Fatalf("planner example %q is in group %q but JSON intent is %q", example.Question, group.Intent, plan.Intent)
			}
			for _, deprecated := range []string{"required_tools", "retrieval", "hard_block_hint"} {
				if strings.Contains(example.PlanJSON, deprecated) {
					t.Fatalf("planner example %q still contains deprecated field %q", example.Question, deprecated)
				}
			}
		}
	}
	for _, intent := range []Intent{
		IntentResourceInfo,
		IntentMonitorQuery,
		IntentKnowledgeQA,
		IntentBillingAccountUnsupported,
		IntentBillingInstance,
		IntentOperationLifecycle,
		IntentDiagnosis,
		IntentDiskInfo,
		IntentDeployModel,
		IntentUnknown,
	} {
		if !seen[intent] {
			t.Fatalf("planner examples missing group for intent %q", intent)
		}
	}
	if total != 60 {
		t.Fatalf("legacy planner example count = %d, want 60", total)
	}
	expectedCounts := map[Intent]int{
		IntentResourceInfo:              8,
		IntentUnknown:                   2,
		IntentMonitorQuery:              2,
		IntentKnowledgeQA:               23,
		IntentBillingAccountUnsupported: 2,
		IntentBillingInstance:           2,
		// PR1 hotfix Bug 1 (2026-05-28): 6 = 5 Batch 1 anchors + new
		// ZERO-target sample for bare "帮我关机" classification.
		// Phase 3 (2026-06-02): +2 custom-image workflow anchors.
		IntentOperationLifecycle: 8,
		// Diagnosis recall fix (2026-06-03): +3 no-target symptom anchors.
		// G1 router consolidation (2026-07-09): +1 cannot-connect boundary anchor.
		IntentDiagnosis: 5,
		// disk_info (2026-05-29): 4 anchors — 我有哪些数据盘 / 我的磁盘列表 /
		// uhost-X 挂了哪些盘 / 我账号下有哪些云盘
		IntentDiskInfo: 4,
		// deploy_model (B8.3, 2026-05-31): 4 workload-first anchors —
		// 部署 Qwen2.5-32B / 跑数字人 / 搭 ComfyUI 环境 / 部署 Llama3 推理
		IntentDeployModel: 4,
	}
	for intent, want := range expectedCounts {
		if got := counts[intent]; got != want {
			t.Fatalf("planner example count for %q = %d, want %d", intent, got, want)
		}
	}
	renderedJSONCount := 0
	for _, group := range groups {
		if group.compact {
			renderedJSONCount++
		} else {
			renderedJSONCount += len(group.Examples)
		}
	}
	rendered := strings.Join(renderRouterPromptExampleGroups(groups), "\n")
	if got := len(promptExampleJSONLines(rendered)); got != renderedJSONCount {
		t.Fatalf("rendered example JSON count = %d, want %d", got, renderedJSONCount)
	}
}

func TestRenderPlannerPromptExampleGroupsUsesDelimitedBlocks(t *testing.T) {
	rendered := strings.Join(renderRouterPromptExampleGroups([]routerPromptExampleGroup{{
		Intent: IntentResourceInfo,
		Source: `source "with" chars`,
		Examples: []routerPromptExample{{
			Question: `show <instance> & gpu`,
			PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}`,
			Source:   `example "source"`,
		}},
	}}), "\n")

	if !strings.Contains(rendered, `<examples intent="resource_info">`) {
		t.Fatalf("rendered examples missing XML-like group delimiter:\n%s", rendered)
	}
	if !strings.Contains(rendered, "<example>") || !strings.Contains(rendered, "</example>") {
		t.Fatalf("rendered examples missing per-example delimiter:\n%s", rendered)
	}
	// Provenance (source="...") is dev-facing metadata; it must NOT be rendered
	// into the prompt the model sees. The Source field stays in the data model
	// (frontmatter-validated, used for authoring/review) but is omitted here.
	if strings.Contains(rendered, "source=") {
		t.Fatalf("example provenance source= must not be rendered into the planner prompt:\n%s", rendered)
	}
	if !strings.Contains(rendered, "<user>show &lt;instance&gt; &amp; gpu</user>") {
		t.Fatalf("rendered examples did not escape user text:\n%s", rendered)
	}
	if got := len(promptExampleJSONLines(rendered)); got != 1 {
		t.Fatalf("rendered JSON count = %d, want 1:\n%s", got, rendered)
	}
}

// TestBuildSystemPromptIncludesOperationLifecycleAnchor locks the Batch 1
// (2026-05-28) jitter fix: planner must classify UHostId+action-verb chats
// (帮我关机 uhost-xxx / uhost-test 停了 / 给 uhost-xxx 加 200G 数据盘) as
// operation_lifecycle. Pre-fix CLI trace at N=6 showed 67% drift to unknown
// (or schema_valid=false). The directive plus the new one-shot example
// group anchor the intent so the classifier doesn't have to infer the
// pattern from first principles.
func TestBuildSystemPromptIncludesOperationLifecycleAnchor(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Classify resource operation commands",
		"as operation_lifecycle",
		// PR1 hotfix Bug 1 (2026-05-28): bare action verb (no target) must
		// still classify as operation_lifecycle so "帮我关机" doesn't fall
		// to unknown.
		"regardless of whether the user specifies a target instance",
		"target_refs:[]",
		// One-shot anchors that the prompt MUST keep as concrete examples.
		"启动 train-gpu",
		"给 uhost-xxx 加 200G 数据盘",
		// Disambiguation from resource_info — without this clause the
		// classifier can fall back to "list-style" reading of the same words.
		"Do NOT route bare action verbs to resource_info",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing operation_lifecycle anchor fragment %q:\n%s", fragment, prompt)
		}
	}
	if strings.Contains(prompt, "Action verbs include 创建 / 开一台 / 搞台 / 抢一台 / 部署一台") {
		t.Fatalf("system prompt must not classify every 部署一台 phrase as operation_lifecycle:\n%s", prompt)
	}
	for _, fragment := range []string{
		"创建 / 开一台 / 搞台 / 抢一台",
		"Spec-first creation where the user dictates exact hardware",
		"帮我搞台 4090",
		"部署一台 4090",
	} {
		if strings.Contains(prompt, fragment) {
			t.Fatalf("base prompt must not preserve old hardware-create rescue fragment %q:\n%s", fragment, prompt)
		}
	}
	unifiedPrompt := buildSystemPromptWithUnifiedCreate(true)
	for _, fragment := range []string{
		"Use create_instance for new GPU instance creation",
		"where the user dictates exact hardware or says to create/open/buy an instance",
	} {
		if !strings.Contains(unifiedPrompt, fragment) {
			t.Fatalf("unified prompt missing create_instance routing fragment %q:\n%s", fragment, unifiedPrompt)
		}
	}
}

// TestBuildSystemPromptIncludesBillingInstanceDiagnosticGuard locks the
// system-prompt directive that makes "充值 10 块就被扣完了 我啥也没干啊"-class
// personal billing complaints route to billing_instance instead of jittering
// between billing_account_unsupported and knowledge_qa. The N=5 jitter check
// on 2026-05-20 showed 3/5 went to billing_account_unsupported (correct hard
// block, but engine ReAct fallthrough that was a lucky path) and 2/5 went to
// knowledge_qa (refusal because corpus has no chunks for personal billing
// complaints). Trace: F:/compshare-agent-runs/q04-jitter-20260520-165129.
func TestBuildSystemPromptIncludesBillingInstanceDiagnosticGuard(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Personal billing complaints with vague cause",
		"emit billing_instance",
		"NOT billing_account_unsupported",
		"NOT knowledge_qa",
		"充值 10 块就被扣完了", // 充值 10 块就被扣完了
		"我账单怎么这么高",     // 我账单怎么这么高
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing billing_instance diagnostic guard fragment %q:\n%s", fragment, prompt)
		}
	}
}

func TestBuildSystemPromptRoutesInventoryAvailabilityToRoute(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Classify inventory availability questions",
		"as stock_availability",
		"Do not route them to resource_info",
		"resource_info is only for the user's own CompShare instances",
		"generic resource-capacity semantics questions",
		"Normal or SoldOut means",
		"unless the user asks for live stock of a named GPU",
		"4090 现在有没有货",
		"\u6211\u8d26\u53f7\u4e0b\u6709\u54ea\u4e9b 4090 \u5b9e\u4f8b",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing inventory boundary fragment %q:\n%s", fragment, prompt)
		}
	}
	if strings.Contains(prompt, "Platform stock questions should emit unknown") {
		t.Fatalf("system prompt still contains stale stock-to-unknown routing:\n%s", prompt)
	}
}

func TestBuildSystemPromptDistinguishesFinanceFAQAndRealtimeAccountData(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Route finance policy/how-to questions to knowledge_qa: invoice issuance, refund rules, arrears handling, why am I still charged after shutdown, billing mode differences, or package expiry.",
		"删除/取消/退订 Coding Plan",
		"not pricing_query or operation_lifecycle",
		"account realtime finance/status questions about THE USER'S OWN ACCOUNT data",
		"Classify instance-scoped billing questions as billing_instance",
		"why am I still charged after shutdown",
		"how do I issue an invoice",
		"what is my invoice status",
		"refund rules",
		"my refund progress",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing finance routing rule %q:\n%s", fragment, prompt)
		}
	}
}

// TestBuildSystemPromptRoutesRuntimePriceQueriesToRoute locks the
// PR #151 directive shift: "4090 \u591a\u5c11\u94b1" used to emit unknown (free LLM
// tool loop), now emits pricing_query (deterministic route handler).
// Also asserts the personal-billing boundary is preserved so complaints
// like \u6211\u8d26\u5355\u600e\u4e48\u8fd9\u4e48\u9ad8 still stay in billing_instance, not pricing.
func TestBuildSystemPromptRoutesRuntimePriceQueriesToRoute(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Classify direct runtime/list/user price questions",
		"as pricing_query",
		"route handler runs DescribeAvailableCompShareInstanceTypes plus the account/catalog price APIs deterministically",
		"4090 \u591a\u5c11\u94b1",
		"H20 \u6309\u6708\u5305\u591a\u5c11\u94b1",
		"\u76ee\u5f55\u4ef7\u591a\u5c11",
		"\u6807\u51c6\u4ef7\u591a\u5c11",
		"Route personal-billing complaints",
		"\u6211\u8d26\u5355\u600e\u4e48\u8fd9\u4e48\u9ad8",
		"to billing_instance",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing price-route routing fragment %q:\n%s", fragment, prompt)
		}
	}
	stale := []string{
		"4090 \u591a\u5c11\u94b1, H20 \u6309\u6708\u5305\u591a\u5c11\u94b1, \u6298\u540e\u4ef7\u591a\u5c11, or actual purchase price should emit unknown",
		"normal tool loop can choose price tools",
	}
	for _, fragment := range stale {
		if strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt still contains stale price-to-unknown routing %q:\n%s", fragment, prompt)
		}
	}
}

// TestBuildSystemPromptPR52FAQProcessVsPersonalStatus locks the #52
// rules added to disambiguate FAQ/process questions from personal-status
// queries. Lane B.5c surfaced two 4-mode hard-block false positives:
// h03 ("我的发票什么时候开") and mq05 ("下载速度突然变慢 是欠费了吗 还是
// 网络高峰"). The engine guard fix alone (isFinanceFAQProcessQuestion)
// is not sufficient when the question reaches the planner; the planner
// prompt must also disambiguate or it falls back to billing_account_
// unsupported under the previous wording.
//
// Lock the four new rule fragments and the ambiguity tie-breaker so a
// future planner prompt edit cannot silently revert them.
func TestBuildSystemPromptPR52FAQProcessVsPersonalStatus(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		// Rule 1: personal account data explicitly enumerated with
		// 我的 X / 我账单 patterns so the LLM keys on the personal pronoun.
		"我的发票开好了吗",
		"我账单还剩多少",
		// Rule 2: FAQ/process schedule questions emit knowledge_qa
		// explicitly contrasted with billing_account_unsupported.
		"FAQ/process questions about HOW the system works",
		"什么时候开发票",
		"欠费几天回收",
		"emit knowledge_qa, not billing_account_unsupported",
		// Rule 3: ambiguity tie-breaker for h03-style "我的 X 什么时候 Y".
		"When ambiguous between process-question and personal-status",
		"我的发票什么时候开",
		"default to knowledge_qa unless the user explicitly asks for the realtime state",
		// Rule 4: diagnostic phrasing (mq05) — finance topic paired with
		// non-finance symptom must route to knowledge_qa, not be tricked
		// by the bare 欠费 keyword.
		"Diagnostic phrasings that pair a finance topic with non-finance symptoms",
		"下载速度突然变慢 是欠费了吗 还是网络高峰",
		"the user is asking for root-cause checklist, not their own balance amount",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing PR-52 finance disambiguation rule %q:\n%s", fragment, prompt)
		}
	}
	// Negative: the legacy "invoice status, refund progress, arrears amount, ..."
	// blanket rule was the root cause of h03 misrouting (any 'invoice' word
	// triggered the unsupported intent). It MUST be replaced by the more
	// specific personal-account version.
	forbidden := []string{
		"account realtime finance/status questions like invoice status, refund progress, arrears amount, payable bills, balance, total bills, transaction records, charge records, package expiry time, or recharge amount should emit billing_account_unsupported",
	}
	for _, fragment := range forbidden {
		if strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt still contains pre-PR-52 blanket rule %q which causes h03-style misrouting:\n%s", fragment, prompt)
		}
	}
}

func TestBuildSystemPromptIncludesKnowledgeQARules(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Classify clear platform usage / FAQ questions",
		"knowledge_qa",
		"Classify diagnosis questions that also reference platform FAQ or usage docs as diagnosis",
		"Route billing-specific FAQ plus instance facts to billing_instance",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing knowledge QA rule %q:\n%s", fragment, prompt)
		}
	}
}

func TestBuildSystemPromptIncludesKnowledgeQABoundaryRules(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Classify platform how-to, config, and error-code questions",
		"remote desktop audio setup",
		"error code 226601",
		"Route 'how do I do X on the platform' to knowledge_qa",
		"runtime failure reports such as cannot connect/open/access",
		"target_refs:[]. When target_refs is empty",
		"Default to knowledge_qa only for pure usage/config/error-code/how-to questions",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing knowledge_qa boundary fragment %q:\n%s", fragment, prompt)
		}
	}
}

func TestBuildSystemPromptIncludesHowToExamples(t *testing.T) {
	prompt := buildSystemPrompt()
	requiredExamples := []string{
		"\u8fdc\u7a0b\u684c\u9762\u6ca1\u58f0\u97f3\u8be5\u600e\u4e48\u5904\u7406",
		"\u9519\u8bef\u7801 226601 \u662f\u4ec0\u4e48\u610f\u601d",
		"Linux \u600e\u4e48\u88c5 NVIDIA \u9a71\u52a8",
		"Coding Plan \u7684 BaseURL \u5e94\u8be5\u586b\u4ec0\u4e48",
		"\u600e\u4e48\u5728 VSCode \u91cc\u8fde GPU \u5b9e\u4f8b",
		"uhost-abc123 \u8fd9\u53f0\u542f\u52a8\u5931\u8d25\u4e86\u5e2e\u6211\u67e5",
	}
	for _, example := range requiredExamples {
		if !strings.Contains(prompt, example) {
			t.Fatalf("system prompt missing example %q:\n%s", example, prompt)
		}
	}
}

func TestBuildSystemPromptClassifiesPerformanceQuestionsAsMonitor(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"CPU high",
		"GPU busy/idle",
		"machine is idle",
		"monitor_query first",
		"CPU is high, what should I do",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing performance monitor rule %q:\n%s", fragment, prompt)
		}
	}
}

func TestBuildSystemPromptTreatsClockRangesAsHistoricalMonitor(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Historical monitor phrases",
		"yesterday",
		"today morning",
		"X点到Y点",
		"never preset now/today",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing historical monitor rule %q:\n%s", fragment, prompt)
		}
	}
}

func TestBuildUserPromptUsesReadableLabels(t *testing.T) {
	prompt := buildUserPrompt(IntentRouterInput{
		UserText:               "show monitor",
		PriorText:              "assistant: prior answer",
		LastIntent:             "monitor_query",
		LastSelectedInstanceID: "uhost-1qy6d8tkfrl4",
		LastAssistantSnippet:   "Your CPU usage is 12% and GPU usage is 65% right now.",
	}, "retry now")
	if !strings.Contains(prompt, "User question: show monitor") {
		t.Fatalf("user prompt missing readable user label: %q", prompt)
	}
	// PR1 hotfix Bug 2 (2026-05-28): the planner USER prompt no longer dumps
	// PriorText verbatim. Multi-turn input_tok growth was the schema_valid=
	// false avalanche driver — see memory:priortext-avalanche-invalidates-
	// planner. PriorText is still passed via IntentRouterInput for the validator's
	// source:prior_turn span check, but buildUserPrompt must emit ONLY the
	// structured signals.
	if strings.Contains(prompt, "Prior turns:") {
		t.Fatalf("user prompt must not include legacy Prior turns block: %q", prompt)
	}
	if !strings.Contains(prompt, "Last selected instance: uhost-1qy6d8tkfrl4") {
		t.Fatalf("user prompt missing LastSelectedInstanceID structured field: %q", prompt)
	}
	if !strings.Contains(prompt, "Last assistant snippet: Your CPU usage is 12% and GPU usage is 65% right now.") {
		t.Fatalf("user prompt missing LastAssistantSnippet structured field: %q", prompt)
	}
	if !strings.Contains(prompt, "Last intent: monitor_query") {
		t.Fatalf("user prompt missing LastIntent structured field: %q", prompt)
	}
	for _, staleLabel := range staleNonASCIIPlannerLabels() {
		if strings.Contains(prompt, staleLabel) {
			t.Fatalf("user prompt contains stale non-ASCII label %q: %q", staleLabel, prompt)
		}
	}
}

func TestBuildUserPrompt_SnippetTruncated(t *testing.T) {
	// PR1 hotfix Bug 2: long assistant replies are capped at
	// lastAssistantSnippetCap runes so cumulative prompt size stays bounded
	// across multi-turn sessions.
	long := strings.Repeat("我", lastAssistantSnippetCap+50)
	prompt := buildUserPrompt(IntentRouterInput{
		UserText:             "再来一次",
		LastAssistantSnippet: long,
	}, "")
	idx := strings.Index(prompt, "Last assistant snippet: ")
	if idx < 0 {
		t.Fatalf("expected Last assistant snippet label, got %q", prompt)
	}
	snippet := prompt[idx+len("Last assistant snippet: "):]
	if got := len([]rune(snippet)); got != lastAssistantSnippetCap {
		t.Fatalf("snippet rune length = %d, want %d", got, lastAssistantSnippetCap)
	}
}

func promptExampleJSONLines(prompt string) []string {
	var examples []string
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			examples = append(examples, line)
		}
	}
	return examples
}

func staleNonASCIIPlannerLabels() []string {
	return []string{
		string([]byte{0xe7, 0x94, 0xa8, 0xe6, 0x88, 0xb7, 0xe9, 0x97, 0xae, 0xe9, 0xa2, 0x98}),
		string([]byte{0xe5, 0xbc, 0x95, 0xe7, 0x94, 0xa8, 0xe5, 0x8e, 0x86, 0xe5, 0x8f, 0xb2}),
	}
}
