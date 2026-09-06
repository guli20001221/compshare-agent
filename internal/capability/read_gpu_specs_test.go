package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gpuSpecsFixture() map[string]any {
	return map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name":           "4090",
				"Zone":           "cn-wlcb-01",
				"GraphicsMemory": map[string]any{"Value": 24},
				"Performance":    map[string]any{"Value": 83},
				"Status":         "Normal",
				"MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(94)}},
						map[string]any{"Cpu": float64(24), "Memory": []any{float64(96)}},
					}},
					map[string]any{"Gpu": float64(2), "Collection": []any{
						map[string]any{"Cpu": float64(32), "Memory": []any{float64(128), float64(192)}},
					}},
				},
			},
			map[string]any{
				"Name":           "A100",
				"GraphicsMemory": map[string]any{"Value": 80},
				"Status":         "Normal",
			},
		},
	}
}

func runGPUSpecs(t *testing.T, exec ReadExecutor, req GPUSpecsRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(gpuSpecsReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec})
}

func TestGPUSpecsRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, GPUSpecsRequest{}.MissingFields())
}

// TestGPUSpecsRender_FilterAndDetail: a summary query filters to the named model
// without expanding machine sizes; a full query expands every combo. Parity with
// intent's TestRenderGPUSpecs_{FilterToMentionedModel,Overview,FullModelRequest}.
func TestGPUSpecsRender_FilterAndDetail(t *testing.T) {
	overview := renderGPUSpecsReply(gpuSpecsFixture(), "A100", platform.DetailLevelSummary)
	assert.Contains(t, overview, "机型=A100")
	assert.NotContains(t, overview, "机型=4090", "summary must filter to the named model")

	summary4090 := renderGPUSpecsReply(gpuSpecsFixture(), "4090", platform.DetailLevelSummary)
	assert.Contains(t, summary4090, "显存=24GB")
	for _, notWant := range []string{"16C/64G", "24C/96G", "32C/192G"} {
		assert.NotContains(t, summary4090, notWant, "overview must not expand full machine-size combos")
	}

	full4090 := renderGPUSpecsReply(gpuSpecsFixture(), "4090", platform.DetailLevelFull)
	for _, want := range []string{"16C/64G", "16C/94G", "24C/96G", "32C/128G", "32C/192G"} {
		assert.Contains(t, full4090, want, "full specs must expand every machine-size combo")
	}
	assert.NotContains(t, full4090, "A100", "full model request still filters unrelated models")
}

func TestGPUSpecsOverviewUsesTheLargestOfferingAcrossZones(t *testing.T) {
	raw := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "5090", "Zone": "cn-wlcb-01", "MachineSizes": []any{map[string]any{"Gpu": float64(4)}}},
		map[string]any{"Name": "5090", "Zone": "cn-sh2-01", "MachineSizes": []any{map[string]any{"Gpu": float64(8)}}},
	}}
	result := runGPUSpecs(t, &fakeReadExec{result: raw}, GPUSpecsRequest{GPUType: "5090"})
	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "最大卡数=8")
	count, ok := resourceFactValue(result.Envelope, "gpu_model:5090", "max_gpu_count")
	require.True(t, ok)
	assert.Equal(t, "8", count)
	// Full detail still shows each zone's own configuration, not the overview merge.
	full := renderGPUSpecsReply(raw, "5090", platform.DetailLevelFull)
	assert.Contains(t, full, "可用区=cn-wlcb-01, 完整配置=4卡")
	assert.Contains(t, full, "可用区=cn-sh2-01, 完整配置=8卡")
}

// TestGPUSpecsRender_NoMatchFallback: a GPU-like token that matches nothing falls
// back to the available list with an explicit prefix.
func TestGPUSpecsRender_NoMatchFallback(t *testing.T) {
	reply := renderGPUSpecsReply(gpuSpecsFixture(), "H100", platform.DetailLevelSummary)
	assert.Contains(t, reply, "未在当前可售机型里找到您提到的型号")
	assert.Contains(t, reply, "机型=4090")
}

// TestGPUSpecsHandle_Empty: an empty catalog is a structured Empty read, not a
// Handled answer whose reply happens to say "no data".
func TestGPUSpecsHandle_Empty(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"AvailableInstanceTypes": []any{}}}

	result := runGPUSpecs(t, exec, GPUSpecsRequest{GPUType: "4090"})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Equal(t, noGPUSpecsReply, result.Reply)
}

func TestGPUSpecsRender_EmptyPayload(t *testing.T) {
	assert.Equal(t, noGPUSpecsReply, renderGPUSpecsReply(map[string]any{}, "", platform.DetailLevelSummary))
}

// TestGPUSpecsEnvelope_SubjectsAndFacts: the envelope is a gpu_specs envelope
// carrying a subject + API facts per matched model and a computed answer_mode,
// with the do-not-invent constraints set (parity with buildGPUSpecsEnvelope).
func TestGPUSpecsEnvelope_SubjectsAndFacts(t *testing.T) {
	env := buildGPUSpecsEnvelope(gpuSpecsFixture(), "A100", platform.DetailLevelSummary)

	assert.Equal(t, envelope.KindGPUSpecsQuery, env.Kind)
	assert.True(t, env.Constraints.DoNotInventInstances)
	assert.True(t, env.Constraints.DoNotAnswerAccountBill)
	require.Len(t, env.Subjects, 1)
	assert.Equal(t, "gpu_model:A100", env.Subjects[0].ID)
	assert.Equal(t, envelope.SubjectGPUModel, env.Subjects[0].Type)

	var answerMode string
	for _, f := range env.Computed {
		if f.Key == "answer_mode" {
			answerMode, _ = f.Value.(string)
		}
	}
	assert.Equal(t, "overview", answerMode)

	facts := map[string]string{}
	for _, f := range env.Facts {
		if s, ok := f.Value.(string); ok {
			facts[f.Key] = s
		}
	}
	assert.Equal(t, "A100", facts["model_name"])
	assert.Equal(t, "80", facts["graphics_memory"])
}

// TestGPUSpecsHandle_RendersAndAttachesEnvelope: end-to-end, one upstream call,
// Handled with the describe action, the reply and a gpu_specs envelope.
func TestGPUSpecsHandle_RendersAndAttachesEnvelope(t *testing.T) {
	exec := &fakeReadExec{result: gpuSpecsFixture()}

	result := runGPUSpecs(t, exec, GPUSpecsRequest{GPUType: "4090", DetailLevel: platform.DetailLevelSummary})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Contains(t, result.Reply, "机型=4090")
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindGPUSpecsQuery, result.Envelope.Kind)
}

func TestGPUSpecsHandle_UpstreamError(t *testing.T) {
	result := runGPUSpecs(t, errReadExec{err: errors.New("boom")}, GPUSpecsRequest{})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, gpuSpecsCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}
