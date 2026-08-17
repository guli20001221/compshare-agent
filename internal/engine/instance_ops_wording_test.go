package engine

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/tools"
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

// The model plans around the description, not around the guardrails. Told it may only read, it
// stops at a verdict; told it may repair, it gathers evidence and then acts. So the description is
// not cosmetic — leaving the read-only text in place while the harness accepts writes produces a
// lane that is authorized to fix and declines to, which reads as a model capability limit.
//
// The inverse is the dangerous one: promising repair while the harness refuses means every fix the
// model proposes is rejected mid-run, burning the turn budget on retries.
func TestInstanceOpsDescriptionFollowsTheWriteGate(t *testing.T) {
	defer tools.SetInstanceOpsWritesEnabled(tools.InstanceOpsWritesEnabled())

	tools.SetInstanceOpsWritesEnabled(false)
	ro := findInstanceOpsTool(centralAgentToolWindow(false, true))
	if ro == nil {
		t.Fatal("DiagnoseInstanceInternals missing from the window with the lane on")
	}
	if !strings.Contains(*ro, "只执行只读命令") {
		t.Fatalf("read-only description lost its read-only promise: %q", *ro)
	}
	if strings.Contains(*ro, "可以直接执行修复命令") {
		t.Fatalf("read-only description promises repair: %q", *ro)
	}
	if !strings.Contains(*ro, "绝不能从列表自行挑选") {
		t.Fatalf("read-only description omits the target-selection boundary: %q", *ro)
	}
	if !strings.Contains(*ro, "不能把它视为已授权") {
		t.Fatalf("read-only description omits the expired-selection reauthorization boundary: %q", *ro)
	}

	tools.SetInstanceOpsWritesEnabled(true)
	rw := findInstanceOpsTool(centralAgentToolWindow(false, true))
	if rw == nil {
		t.Fatal("DiagnoseInstanceInternals missing from the window with the lane on")
	}
	if !strings.Contains(*rw, "可以直接执行修复命令") {
		t.Fatalf("write description does not offer repair: %q", *rw)
	}
	if !strings.Contains(*rw, "绝不能从列表自行挑选") {
		t.Fatalf("write description omits the target-selection boundary: %q", *rw)
	}
	if !strings.Contains(*rw, "不能把它视为已授权") {
		t.Fatalf("write description omits the expired-selection reauthorization boundary: %q", *rw)
	}
	// Still has to name the hard limits, or the model plans around commands it can never run.
	if !strings.Contains(*rw, "始终会被拒绝") {
		t.Fatalf("write description omits the destructive refusal: %q", *rw)
	}
	if *ro == *rw {
		t.Fatal("description did not change with the write gate")
	}
}

// With the lane off the tool is absent entirely (INV-10) — the write gate must not resurrect it.
func TestWriteGateDoesNotExposeTheToolWhenTheLaneIsOff(t *testing.T) {
	defer tools.SetInstanceOpsWritesEnabled(tools.InstanceOpsWritesEnabled())
	tools.SetInstanceOpsWritesEnabled(true)
	if findInstanceOpsTool(centralAgentToolWindow(false, false)) != nil {
		t.Fatal("lane off but DiagnoseInstanceInternals is in the window (INV-10)")
	}
}
