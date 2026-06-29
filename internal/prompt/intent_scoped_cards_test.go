package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/workflow"
)

func TestBuildSystemWithOptions_IntentScopedBaseOmitsWorkflowCatalog(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{
		MutatingToolsEnabled:    true,
		IntentScopedReActPrompt: true,
	})
	for _, name := range workflow.RegisteredWorkflowActions() {
		if strings.Contains(p, name) {
			t.Fatalf("intent-scoped base prompt should not contain workflow catalog action %s", name)
		}
	}
	if !strings.Contains(p, "本轮 ReAct 会按 planner 意图临时注入对应操作卡片") {
		t.Fatalf("intent-scoped base prompt should explain runtime card injection:\n%s", p)
	}
}

func TestRenderIntentScopedReActCard_DiagnosisExcludesWorkflowCatalog(t *testing.T) {
	card := RenderIntentScopedReActCard(intent.IntentDiagnosis, true)
	if !strings.Contains(card, "本轮 ReAct 诊断卡片") {
		t.Fatalf("diagnosis card missing heading:\n%s", card)
	}
	if !strings.Contains(card, "DiagnoseSSH") {
		t.Fatalf("diagnosis card missing diagnosis catalog:\n%s", card)
	}
	for _, name := range workflow.RegisteredWorkflowActions() {
		if strings.Contains(card, name) {
			t.Fatalf("diagnosis card must not contain workflow %s:\n%s", name, card)
		}
	}
}

func TestRenderIntentScopedReActCard_ReadOnlyOperationExcludesWorkflowCatalog(t *testing.T) {
	card := RenderIntentScopedReActCard(intent.IntentOperationLifecycle, false)
	if !strings.Contains(card, "本轮 ReAct 只读操作卡片") {
		t.Fatalf("read-only operation card missing heading:\n%s", card)
	}
	for _, name := range workflow.RegisteredWorkflowActions() {
		if strings.Contains(card, name) {
			t.Fatalf("read-only operation card must not contain workflow %s:\n%s", name, card)
		}
	}
}

func TestRenderIntentScopedReActCard_OperationOmitsCreateOnlyRules(t *testing.T) {
	card := RenderIntentScopedReActCard(intent.IntentOperationLifecycle, true)
	for _, text := range []string{
		"CreateInstanceWorkflow",
		"PyTorch/CUDA/vLLM",
		"ImageName",
		"ComfyUI、SD WebUI、Stable Diffusion、Dify、Ollama",
		`ImageSource="community"`,
	} {
		if strings.Contains(card, text) {
			t.Fatalf("operation card must not contain create-only rule fragment %q:\n%s", text, card)
		}
	}
	for _, text := range []string{
		"StopInstanceWorkflow",
		"ResizeInstanceWorkflow",
		"CreateDiskWorkflow",
		"ResizeDiskWorkflow",
	} {
		if !strings.Contains(card, text) {
			t.Fatalf("operation card missing lifecycle workflow fragment %q:\n%s", text, card)
		}
	}
}

// TestRenderIntentScopedReActCard_NeverEmptyFallback covers the flag-on empty/
// unknown intent gap: the slim base prompt promises a card will be injected this
// turn, and the engine only inserts non-empty cards. The planner can also jitter
// a real on-platform operation to "unknown". So every intent — including unknown,
// empty, and uncovered values — must yield a non-empty conservative card.
func TestRenderIntentScopedReActCard_NeverEmptyFallback(t *testing.T) {
	for _, in := range []intent.Intent{
		intent.IntentUnknown,
		intent.Intent(""),
		intent.Intent("some_unmapped_future_intent"),
	} {
		card := RenderIntentScopedReActCard(in, true)
		if strings.TrimSpace(card) == "" {
			t.Fatalf("intent %q produced an empty card; flag-on base promises a card every turn", in)
		}
		for _, text := range []string{"兜底", "Workflow", "Diagnose", "范围边界拒答"} {
			if !strings.Contains(card, text) {
				t.Fatalf("fallback card for intent %q missing %q:\n%s", in, text, card)
			}
		}
		if strings.Contains(card, "创建实例") || strings.Contains(card, "CreateInstanceWorkflow") {
			t.Fatalf("fallback card for intent %q must not duplicate create workflow rules already owned by operation cards:\n%s", in, card)
		}
	}
}

// TestOperationBoundaryRules_SingleSource proves the operation boundary rules are
// rendered from one source into BOTH the full flag-off prompt and the flag-on
// operation card, so editing operationBoundaryRuleLines updates both and the two
// paths cannot drift.
func TestOperationBoundaryRules_SingleSource(t *testing.T) {
	fullOff := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	cardOn := RenderIntentScopedReActCard(intent.IntentOperationLifecycle, true)
	for _, line := range operationBoundaryRuleLines() {
		if !strings.Contains(fullOff, line) {
			t.Fatalf("flag-off full prompt missing single-sourced operation rule:\n%s", line)
		}
		if !strings.Contains(cardOn, line) {
			t.Fatalf("flag-on operation card missing single-sourced operation rule:\n%s", line)
		}
	}
	// State-refresh and vague-failure shared fragments must also appear in both.
	for _, frag := range []string{sharedStateRefreshBeforeMutationRule, sharedVagueFailureRule} {
		if !strings.Contains(fullOff, frag) {
			t.Fatalf("flag-off full prompt missing shared fragment:\n%s", frag)
		}
	}
}
