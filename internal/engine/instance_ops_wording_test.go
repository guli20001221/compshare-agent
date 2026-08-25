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

// The lane has one production contract: inspect immediately, propose the exact repair command, run
// it only after that command is approved, and verify the result. A deployment-wide read-only switch
// made the prompt, confirmation card and harness disagree; keeping one contract makes that drift
// unrepresentable while the command classifier still auto-runs only positively proven reads.
func TestInstanceOpsDescriptionOffersConfirmationGatedRepair(t *testing.T) {
	desc := findInstanceOpsTool(centralAgentToolWindow(false, true))
	if desc == nil {
		t.Fatal("DiagnoseInstanceInternals missing from the window with the lane on")
	}
	if strings.Contains(*desc, "只执行只读命令") {
		t.Fatalf("single repair contract regressed to the removed read-only product mode: %q", *desc)
	}
	if !strings.Contains(*desc, "可以直接执行修复命令") {
		t.Fatalf("description does not offer confirmation-gated repair: %q", *desc)
	}
	if !strings.Contains(*desc, "绝不能从列表自行挑选") {
		t.Fatalf("description omits the target-selection boundary: %q", *desc)
	}
	if !strings.Contains(*desc, "不能把它视为已授权") {
		t.Fatalf("description omits the expired-selection reauthorization boundary: %q", *desc)
	}
	// Still has to name the hard limits, or the model plans around commands it can never run.
	if !strings.Contains(*desc, "始终会被拒绝") {
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
