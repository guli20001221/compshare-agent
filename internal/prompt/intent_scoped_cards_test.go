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

func TestRenderIntentScopedReActCard_OperationPreservesImageSelectionRules(t *testing.T) {
	card := RenderIntentScopedReActCard(intent.IntentOperationLifecycle, true)
	for _, text := range []string{
		"PyTorch/CUDA/vLLM",
		"ImageName",
		"ComfyUI、SD WebUI、Stable Diffusion、Dify、Ollama",
		`ImageSource="community"`,
		"CheckCompShareResourceCapacity",
		"不要推荐后再发现没货",
	} {
		if !strings.Contains(card, text) {
			t.Fatalf("operation card missing image/capacity rule fragment %q:\n%s", text, card)
		}
	}
}
