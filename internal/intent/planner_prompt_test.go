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
		"mixed_diagnosis_kb",
		"mixed_billing_kb",
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

func TestBuildSystemPromptExamplesParse(t *testing.T) {
	examples := promptExampleJSONLines(buildSystemPrompt())
	if len(examples) != 7 {
		t.Fatalf("prompt examples count = %d, want 7; examples=%v", len(examples), examples)
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

func TestBuildSystemPromptIncludesKnowledgeQARules(t *testing.T) {
	prompt := buildSystemPrompt()
	required := []string{
		"clear platform usage / FAQ questions",
		"knowledge_qa",
		"diagnosis questions that also reference platform FAQ",
		"mixed_diagnosis_kb",
		"billing-specific FAQ plus instance facts",
		"mixed_billing_kb",
	}
	for _, fragment := range required {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("system prompt missing knowledge QA rule %q:\n%s", fragment, prompt)
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
