package engine

import (
	"strings"
	"testing"

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

// The lane has one target and transport contract with two runtime scopes: observation-only inspect
// and autonomous recoverable repair. Neither scope creates UI confirmation cards.
func TestInstanceOpsDescriptionOffersExplicitInspectAndRepairScopes(t *testing.T) {
	desc := findInstanceOpsTool(centralAgentToolWindow(true, true))
	if desc == nil {
		t.Fatal("DiagnoseInstanceInternals missing from the window with the lane on")
	}
	if !strings.Contains(*desc, "Mode=inspect") || !strings.Contains(*desc, "Mode=repair") {
		t.Fatalf("description omits the typed inspection/repair boundary: %q", *desc)
	}
	if !strings.Contains(*desc, "不弹实例内或逐命令确认") {
		t.Fatalf("description does not offer the card-free autonomous repair contract: %q", *desc)
	}
	if !strings.Contains(*desc, "下载") || !strings.Contains(*desc, "不只给手工命令") {
		t.Fatalf("description does not route explicit guest-local operations into the lane: %q", *desc)
	}
	if !strings.Contains(*desc, "不从列表自选") {
		t.Fatalf("description omits the target-selection boundary: %q", *desc)
	}
	if !strings.Contains(*desc, "会话最后一次") || !strings.Contains(*desc, "不因时间失效") {
		t.Fatalf("description omits long-pause target continuity: %q", *desc)
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
