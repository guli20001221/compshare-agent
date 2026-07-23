package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

func guidedIntakeExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}}},
		}},
		"DescribeCompShareSupportZone": {"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
		}},
	}}
}

// An incomplete create ("创建一台虚机", no GpuType) whose only Missing field is
// guided-collectable opens the guided intake form instead of a prose
// back-and-forth — when a guided form is available this turn.
func TestIncompleteCreateRoutesIntoGuidedIntakeForm(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, guidedIntakeExecutor(), func(string, map[string]any) bool { return true })
	eng.guidedCreate = true
	formShown := false
	eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		if form != nil {
			formShown = true
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}
	eng.lastUserMsg = "创建一台虚机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-intake", time.Now())
	eng.turnContextViewReady = true
	onStep, events := collectSteps()

	_ = eng.executeActionProposal(context.Background(), map[string]any{
		"turn_id": "turn-intake", "operation": "CreateInstanceWorkflow", "slots": []any{},
	}, onStep)

	routed := false
	for _, ev := range *events {
		if strings.Contains(ev.Message, "提案进入引导式表单收集") {
			routed = true
		}
	}
	require.True(t, routed, "an incomplete-but-collectable create must route into the guided intake form, not bounce")
	require.True(t, formShown, "the guided form is rendered for the user to pick the missing fields")
}

// Without a guided form available (the client did not opt in), the same
// incomplete create bounces the resolved action back to the model: ReadyForIntake
// changes nothing when there is no form to render.
func TestIncompleteCreateWithoutGuidedBouncesToModel(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, guidedIntakeExecutor(), func(string, map[string]any) bool { return true })
	// guidedCreate stays false; confirmEditsFn stays nil.
	eng.lastUserMsg = "创建一台虚机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-nobounce", time.Now())
	eng.turnContextViewReady = true
	onStep, events := collectSteps()

	out := eng.executeActionProposal(context.Background(), map[string]any{
		"turn_id": "turn-nobounce", "operation": "CreateInstanceWorkflow", "slots": []any{},
	}, onStep)

	require.Contains(t, out, "ready_for_confirmation", "the resolved action is handed back to the model")
	require.Contains(t, out, "GpuType", "the bounce names the missing field the model should ask about")
	for _, ev := range *events {
		require.NotContains(t, ev.Message, "提案进入引导式表单收集", "no guided form → no intake routing")
	}
}
