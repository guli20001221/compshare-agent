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
	if len(examples) != 15 {
		t.Fatalf("prompt examples count = %d, want 15; examples=%v", len(examples), examples)
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

func TestBuildSystemPromptDistinguishesFinanceFAQAndRealtimeAccountData(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"finance policy/how-to questions like invoice issuance, refund rules, arrears handling, why am I still charged after shutdown, billing mode differences, or package expiry should emit knowledge_qa",
		"account realtime finance/status questions like invoice status, refund progress, arrears amount, payable bills, balance, total bills, transaction records, charge records, package expiry time, or recharge amount should emit billing_account_unsupported",
		"instance-scoped billing questions should emit billing_instance",
		"why am I still charged after shutdown",
		"how do I issue an invoice",
		"what is my invoice status",
		"refund rules",
		"refund progress",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing finance routing rule %q:\n%s", fragment, prompt)
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
