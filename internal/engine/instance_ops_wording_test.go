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

// The lane has one production contract: deployment-authorized autonomous guest-local diagnosis,
// recoverable repair and verification on a user-selected target, with no UI cards.
func TestInstanceOpsDescriptionOffersCardFreeAutonomousRepair(t *testing.T) {
	desc := findInstanceOpsTool(centralAgentToolWindow(true, true))
	if desc == nil {
		t.Fatal("DiagnoseInstanceInternals missing from the window with the lane on")
	}
	if strings.Contains(*desc, "只执行只读命令") {
		t.Fatalf("single repair contract regressed to the removed read-only product mode: %q", *desc)
	}
	if !strings.Contains(*desc, "不再额外弹授权卡") || !strings.Contains(*desc, "不逐命令请求确认") {
		t.Fatalf("description does not offer the card-free autonomous repair contract: %q", *desc)
	}
	if !strings.Contains(*desc, "下载模型/文件到指定磁盘") || !strings.Contains(*desc, "不要只给用户手工命令") {
		t.Fatalf("description does not route explicit guest-local operations into the lane: %q", *desc)
	}
	if !strings.Contains(*desc, "绝不能从列表自行挑选") {
		t.Fatalf("description omits the target-selection boundary: %q", *desc)
	}
	if !strings.Contains(*desc, "同一会话") || !strings.Contains(*desc, "不因时间间隔失效") {
		t.Fatalf("description omits long-pause target continuity: %q", *desc)
	}
	// Still has to name the hard limits, or the model plans around commands it can never run.
	if !strings.Contains(*desc, "会被拒绝") {
		t.Fatalf("description omits the destructive refusal: %q", *desc)
	}
	if !strings.Contains(*desc, "本轮消息中明确给出 Authorization") ||
		!strings.Contains(*desc, "不要索要、复制或猜测凭据值") {
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
