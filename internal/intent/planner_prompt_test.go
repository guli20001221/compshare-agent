package intent

import (
	"strings"
	"testing"
)

func TestBuildSystemPromptIncludesPhase1CutoverSchemaFields(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"resource_info",
		"monitor_query",
		"confidence",
		"target_refs",
		"source_span",
		"hard_block_hint",
		"retrieval",
		"knowledge_qa",
		// Capability Registry v1 enum labels (PR A 2026-05-18) must appear in
		// the system prompt enum line so the LLM can emit them as intents.
		"gpu_specs_query",
		"stock_availability",
		"platform_image_list",
		"custom_image_list",
		"community_image_list",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing %q:\n%s", fragment, prompt)
		}
	}
	for _, staleLabel := range staleNonASCIIPlannerLabels() {
		if strings.Contains(prompt, staleLabel) {
			t.Fatalf("system prompt contains stale non-ASCII label %q:\n%s", staleLabel, prompt)
		}
	}
}

func TestBuildSystemPromptDoesNotEmitMixedIntents(t *testing.T) {
	prompt := buildSystemPrompt()
	for _, legacy := range []string{"mixed_diagnosis_kb", "mixed_billing_kb"} {
		if strings.Contains(prompt, legacy) {
			t.Fatalf("system prompt should not ask planner to emit legacy mixed intent %q:\n%s", legacy, prompt)
		}
	}
}

func TestBuildSystemPromptExamplesParse(t *testing.T) {
	examples := promptExampleJSONLines(buildSystemPrompt())
	// 23 legacy examples (20 + 3 added by #34a 2026-05-18 for comparison /
	// yes-no feasibility / procedure-description knowledge_qa coverage) +
	// N capability one-shots (PR A Registry v1) appended by CapabilityPromptFragments.
	// New capabilities here should bump this number; the regression value is
	// `23 + len(capabilityRegistry)`.
	if got, want := len(examples), 23+len(capabilityRegistry); got != want {
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
		if plan.Retrieval.Enabled {
			t.Fatalf("prompt example unexpectedly enables retrieval: %s", example)
		}
	}
}

func TestBuildSystemPromptKeepsInventoryAvailabilityOutOfResourceInfo(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"Inventory availability questions",
		"are not resource_info",
		"resource_info is only for the user's own CompShare instances",
		"Platform stock questions should emit unknown",
		"\u4e0a\u6d77\u673a\u623f\u8fd8\u5269\u6ca1\u5269 H100 \u5e93\u5b58",
		"4090 \u8fd8\u6709\u6ca1\u6709\u8d27",
		"\u6211\u8d26\u53f7\u4e0b\u6709\u54ea\u4e9b 4090 \u5b9e\u4f8b",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing inventory boundary fragment %q:\n%s", fragment, prompt)
		}
	}
}

func TestBuildSystemPromptDistinguishesFinanceFAQAndRealtimeAccountData(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"finance policy/how-to questions like invoice issuance, refund rules, arrears handling, why am I still charged after shutdown, billing mode differences, or package expiry should emit knowledge_qa",
		"account realtime finance/status questions about THE USER'S OWN ACCOUNT data",
		"instance-scoped billing questions should emit billing_instance",
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
		"clear platform usage / FAQ questions",
		"knowledge_qa",
		"diagnosis questions that also reference platform FAQ or usage docs should still emit diagnosis",
		"billing-specific FAQ plus instance facts should emit billing_instance",
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
		"Platform how-to/config/error-code questions",
		"how to configure remote desktop audio",
		"what does error code 226601 mean",
		"how do I do X on the platform' = knowledge_qa",
		"my specific instance has problem X",
		"Without a concrete instance target, default to knowledge_qa",
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
	prompt := buildUserPrompt(PlannerInput{
		UserText:  "show monitor",
		PriorText: "assistant: prior answer",
	}, "retry now")
	if !strings.Contains(prompt, "User question: show monitor") {
		t.Fatalf("user prompt missing readable user label: %q", prompt)
	}
	if !strings.Contains(prompt, "Prior turns: assistant: prior answer") {
		t.Fatalf("user prompt missing readable prior label: %q", prompt)
	}
	for _, staleLabel := range staleNonASCIIPlannerLabels() {
		if strings.Contains(prompt, staleLabel) {
			t.Fatalf("user prompt contains stale non-ASCII label %q: %q", staleLabel, prompt)
		}
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
