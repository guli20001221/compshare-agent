package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/prompt"
	openai "github.com/sashabaranov/go-openai"
)

// findTool returns the DiagnoseInstanceInternals entry of a built window, or nil.
func findInstanceOpsTool(window []openai.Tool) *string {
	for _, t := range window {
		if t.Function != nil && t.Function.Name == "DiagnoseInstanceInternals" {
			d := t.Function.Description
			return &d
		}
	}
	return nil
}

func TestInstanceOpsDescriptionUsesOneTaskScopedRepairContract(t *testing.T) {
	desc := findInstanceOpsTool(centralAgentToolWindow(true, true))
	if desc == nil {
		t.Fatal("DiagnoseInstanceInternals missing from the window with the lane on")
	}
	if strings.Contains(*desc, "Mode=") || !strings.Contains(*desc, "明确只检查、不修改时仅观察") {
		t.Fatalf("description must preserve user constraints without a model-selected mode: %q", *desc)
	}
	if !strings.Contains(*desc, "不弹实例内或逐命令确认") {
		t.Fatalf("description does not offer the card-free autonomous repair contract: %q", *desc)
	}
	if !strings.Contains(*desc, "下载") || !strings.Contains(*desc, "不只给手工命令") {
		t.Fatalf("description does not route explicit guest-local operations into the lane: %q", *desc)
	}
	if !strings.Contains(*desc, "根据完整对话确定目标实例 ID") || !strings.Contains(*desc, "账号归属") {
		t.Fatalf("description must separate Agent selection from server tenant checking: %q", *desc)
	}
	if strings.Contains(*desc, "user_selected") {
		t.Fatalf("description restores the removed lexical selection gate: %q", *desc)
	}
	// Still has to name the hard limits, or the model plans around commands it can never run.
	if !strings.Contains(*desc, "会被拒绝") {
		t.Fatalf("description omits the destructive refusal: %q", *desc)
	}
	if !strings.Contains(*desc, "Authorization 仅在本轮") ||
		!strings.Contains(*desc, "不复制或猜值") {
		t.Fatalf("description does not tell the planner about the request-local auth capability: %q", *desc)
	}
}

// With the lane off the tool is absent entirely (INV-10).
func TestDisabledLaneDoesNotExposeTheTool(t *testing.T) {
	if findInstanceOpsTool(centralAgentToolWindow(false, false)) != nil {
		t.Fatal("lane off but DiagnoseInstanceInternals is in the window (INV-10)")
	}
}

func TestReadOnlyDeploymentDoesNotExposeTheInstanceRepairLane(t *testing.T) {
	if findInstanceOpsTool(centralAgentToolWindow(false, true)) != nil {
		t.Fatal("runner is wired but mutating_tools=false; the autonomous repair lane must stay hidden")
	}
}

func TestInstanceOpsAssembledPromptAndToolWindowDoNotRestoreTheModeSelector(t *testing.T) {
	system := prompt.BuildSystemWithOptions("", prompt.BuildOptions{
		MutatingToolsEnabled: true, InstanceOpsEnabled: true,
	})
	window, err := json.Marshal(centralAgentToolWindow(true, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{system, string(window)} {
		for _, obsolete := range []string{"Mode=", `"Mode"`, "repair_scope_authorized", "inspection_scope", "运行时会拒绝写入"} {
			if strings.Contains(surface, obsolete) {
				t.Fatalf("active model input still contains the removed mode selector: %q", obsolete)
			}
		}
	}
}
