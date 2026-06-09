package workflow

import (
	"testing"

	"github.com/compshare-agent/internal/security"
)

// TestExecutionContract_ParityWithLiveSteps pins ExecutionContract as a faithful
// projection of every registered workflow's step shape. Per step it recomputes
// the expected binding/risk directly from the raw Step and checks the projection
// against it. Iterating the whole registry guards drift: a new workflow, or a
// step whose shape the projection stops tracking, fails here.
func TestExecutionContract_ParityWithLiveSteps(t *testing.T) {
	for _, action := range RegisteredWorkflowActions() {
		def, ok := GetWorkflow(action)
		if !ok {
			t.Fatalf("registered workflow %q has no definition", action)
		}
		contracts := ExecutionContract(def)
		if len(contracts) != len(def.Steps) {
			t.Fatalf("%s: contract has %d steps, definition has %d", action, len(contracts), len(def.Steps))
		}
		for i, step := range def.Steps {
			got := contracts[i]
			if got.Name != step.Name || got.Type != step.Type {
				t.Errorf("%s step %d: contract {Name:%q Type:%v} != step {Name:%q Type:%v}",
					action, i, got.Name, got.Type, step.Name, step.Type)
			}
			assertBindingMatchesStep(t, action, i, step, got)
		}
	}
}

// assertBindingMatchesStep is the executable statement of ExecutionContract's
// documented precedence: StepConfirm → none; else ToolFunc → dynamic; else Tool
// → static with risk = security.Check(Tool).
func assertBindingMatchesStep(t *testing.T, action string, i int, step Step, got ExecutionStepContract) {
	t.Helper()
	switch {
	case step.Type == StepConfirm:
		if got.ToolBinding != ToolBindingNone || got.RiskKnown || got.Tool != "" {
			t.Errorf("%s step %d (StepConfirm): want none/!RiskKnown/no-tool, got binding=%v RiskKnown=%v Tool=%q",
				action, i, got.ToolBinding, got.RiskKnown, got.Tool)
		}
	case step.ToolFunc != nil:
		if got.ToolBinding != ToolBindingDynamic || got.RiskKnown {
			t.Errorf("%s step %d (dynamic ToolFunc): want dynamic/!RiskKnown, got binding=%v RiskKnown=%v",
				action, i, got.ToolBinding, got.RiskKnown)
		}
	case step.Tool != "":
		if got.ToolBinding != ToolBindingStatic || got.Tool != step.Tool {
			t.Errorf("%s step %d (static): want static Tool=%q, got binding=%v Tool=%q",
				action, i, step.Tool, got.ToolBinding, got.Tool)
		}
		lvl, err := security.Check(step.Tool)
		if err == nil {
			if !got.RiskKnown || got.Risk != lvl {
				t.Errorf("%s step %d (static %q): want RiskKnown Risk=%v, got RiskKnown=%v Risk=%v",
					action, i, step.Tool, lvl, got.RiskKnown, got.Risk)
			}
		} else if got.RiskKnown {
			t.Errorf("%s step %d (static %q, not in security whitelist): want !RiskKnown, got Risk=%v",
				action, i, step.Tool, got.Risk)
		}
	default:
		if got.ToolBinding != ToolBindingNone || got.RiskKnown {
			t.Errorf("%s step %d (unbound StepToolCall): want none/!RiskKnown, got binding=%v RiskKnown=%v",
				action, i, got.ToolBinding, got.RiskKnown)
		}
	}
}

// TestExecutionContract_CreateInstanceExercisesAllBindings pins the canonical
// mixed workflow: CreateInstanceDef is the reason the contract names the binding
// explicitly instead of assuming a static Tool. It must contain all three
// bindings — a dynamic ToolFunc image query (查询镜像), a StepConfirm gate
// (确认创建), and a static CreateCompShareInstance (创建实例, L1) — and the static
// step's risk must be the real security level, never fabricated for the
// dynamic/confirm steps.
func TestExecutionContract_CreateInstanceExercisesAllBindings(t *testing.T) {
	def, ok := GetWorkflow("CreateInstanceWorkflow")
	if !ok {
		t.Fatal("CreateInstanceWorkflow not registered")
	}

	var sawDynamic, sawConfirm, sawStaticCreate bool
	for _, c := range ExecutionContract(def) {
		switch {
		case c.ToolBinding == ToolBindingDynamic:
			sawDynamic = true
			if c.RiskKnown {
				t.Errorf("dynamic step %q must not claim known risk", c.Name)
			}
		case c.Type == StepConfirm:
			sawConfirm = true
			if c.ToolBinding != ToolBindingNone || c.RiskKnown {
				t.Errorf("confirm step %q must be none/!RiskKnown, got binding=%v RiskKnown=%v", c.Name, c.ToolBinding, c.RiskKnown)
			}
		case c.ToolBinding == ToolBindingStatic && c.Tool == "CreateCompShareInstance":
			sawStaticCreate = true
			if !c.RiskKnown || c.Risk != security.L1 {
				t.Errorf("static CreateCompShareInstance must be RiskKnown L1, got RiskKnown=%v Risk=%v", c.RiskKnown, c.Risk)
			}
		}
	}
	if !sawDynamic || !sawConfirm || !sawStaticCreate {
		t.Errorf("CreateInstance must exercise all three bindings: dynamic=%v confirm=%v staticCreate=%v",
			sawDynamic, sawConfirm, sawStaticCreate)
	}
}

// TestExecutionContract_NeverFabricatesRisk is the cross-workflow honesty
// invariant — the first audit consumer of the projection. Over every registered
// workflow, RiskKnown may be true ONLY for a static tool binding; dynamic and
// confirm steps must never report a risk. This is exactly the property
// confirm-card / a future evaluator rely on: a reported Risk is real, not a guess.
func TestExecutionContract_NeverFabricatesRisk(t *testing.T) {
	for _, action := range RegisteredWorkflowActions() {
		def, ok := GetWorkflow(action)
		if !ok {
			t.Fatalf("registered workflow %q has no definition", action)
		}
		for _, c := range ExecutionContract(def) {
			if c.RiskKnown && c.ToolBinding != ToolBindingStatic {
				t.Errorf("%s: step %q reports RiskKnown with non-static binding %v — fabricated risk",
					action, c.Name, c.ToolBinding)
			}
			if c.ToolBinding != ToolBindingStatic && c.Tool != "" {
				t.Errorf("%s: step %q has Tool=%q but non-static binding %v",
					action, c.Name, c.Tool, c.ToolBinding)
			}
		}
	}
}
