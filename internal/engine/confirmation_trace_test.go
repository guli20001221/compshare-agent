package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmationTraceDistinguishesGuidedCardsAndRecordsTheApprovedFinalContract(t *testing.T) {
	eng := &Engine{}
	var traces []observability.ConfirmationTrace
	eng.SetConfirmationTraceObserver(func(trace observability.ConfirmationTrace) {
		traces = append(traces, trace)
	})

	eng.recordConfirmationResult("CreateInstanceWorkflow", ConfirmationResult{Confirmed: true}, time.Now(),
		map[string]any{"workflow": "CreateInstanceWorkflow", "GpuType": "H20"},
		&workflow.ConfirmForm{Step: &workflow.ConfirmFormStep{Index: 2, Title: "第二步，选择 GPU"}})
	eng.recordConfirmationResult("CreateInstanceWorkflow", ConfirmationResult{
		TerminalReason: observability.ConfirmationReasonTimeout,
	}, time.Now(), map[string]any{"GpuType": "H20"},
		&workflow.ConfirmForm{Step: &workflow.ConfirmFormStep{Index: 6, Title: "第六步，确认镜像与计费", Final: true}})
	eng.recordConfirmationResult("CreateInstanceWorkflow", ConfirmationResult{Confirmed: true}, time.Now(),
		map[string]any{
			"GpuType": "H20", "Gpu": float64(1), "CPU": float64(16), "Memory": float64(245760),
			"Zone": "cn-wlcb-01", "ZoneLabel": "华北二A", "image": "InfiniteTalk", "SystemDisk": "SSD 云盘 200GB",
			"DataDisk": "SSD 云数据盘 100GB", "ChargeType": "Postpay", "price": "¥7.12/小时（预估）",
			"Name": "not-part-of-the-audit-contract",
		},
		&workflow.ConfirmForm{Step: &workflow.ConfirmFormStep{Index: 6, Title: "第六步，确认镜像与计费", Final: true}})

	require.Len(t, traces, 3)
	require.NotNil(t, traces[0].StepIndex)
	assert.Equal(t, 2, *traces[0].StepIndex)
	assert.Equal(t, "第二步，选择 GPU", traces[0].StepTitle)
	require.NotNil(t, traces[0].Final)
	assert.False(t, *traces[0].Final)
	assert.Nil(t, traces[0].ConfirmedContract)
	data, err := json.Marshal(traces[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), `"final":false`)

	require.NotNil(t, traces[1].Final)
	assert.True(t, *traces[1].Final)
	assert.Equal(t, observability.ConfirmationReasonTimeout, traces[1].TerminalReason)
	assert.Nil(t, traces[1].ConfirmedContract, "an unapproved final card records no contract")

	require.NotNil(t, traces[2].StepIndex)
	assert.Equal(t, 6, *traces[2].StepIndex)
	assert.Equal(t, "第六步，确认镜像与计费", traces[2].StepTitle)
	require.NotNil(t, traces[2].Final)
	assert.True(t, *traces[2].Final)
	require.NotNil(t, traces[2].ConfirmedContract)
	assert.Equal(t, observability.ConfirmedCreateContract{
		GPUType: "H20", GPU: 1, CPU: 16, MemoryMB: 245760,
		Zone: "cn-wlcb-01", ZoneLabel: "华北二A", Image: "InfiniteTalk", SystemDisk: "SSD 云盘 200GB",
		DataDisk: "SSD 云数据盘 100GB", ChargeType: "Postpay", EstimatedPrice: "¥7.12/小时（预估）",
	}, *traces[2].ConfirmedContract)
}

func TestConfirmationTracePlainYNKeepsLegacyEmptyMetadata(t *testing.T) {
	eng := &Engine{}
	var trace observability.ConfirmationTrace
	eng.SetConfirmationTraceObserver(func(got observability.ConfirmationTrace) { trace = got })

	eng.recordConfirmationResult("StopInstanceWorkflow", ConfirmationResult{Confirmed: true}, time.Now(), nil, nil)

	assert.Nil(t, trace.StepIndex)
	assert.Empty(t, trace.StepTitle)
	assert.Nil(t, trace.Final)
	assert.Nil(t, trace.ConfirmedContract)
	data, err := json.Marshal(trace)
	require.NoError(t, err)
	for _, field := range []string{"step_index", "step_title", "final", "confirmed_contract"} {
		assert.NotContains(t, string(data), field)
	}
}

type confirmationTraceProbeLLM struct {
	onChat func()
	called bool
}

func (m *confirmationTraceProbeLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	if !m.called {
		m.called = true
		m.onChat()
	}
	return &llm.ChatResponse{Content: "确认卡审计接线完成。"}, nil
}

// This test enters through ChatWithOptions so removing the production wrapper's
// args/form handoff cannot leave the lower-level projection tests green.
func TestChatWithOptionsConnectsGuidedConfirmationMetadataToTrace(t *testing.T) {
	var eng *Engine
	probe := &confirmationTraceProbeLLM{}
	eng = NewWithDeps(probe, &mockExecutor{}, nil)
	var traces []observability.ConfirmationTrace
	eng.SetConfirmationTraceObserver(func(trace observability.ConfirmationTrace) {
		traces = append(traces, trace)
	})
	probe.onChat = func() {
		resolution := eng.confirmEditsFn("CreateInstanceWorkflow", map[string]any{
			"GpuType": "H20", "Gpu": float64(1), "CPU": float64(16), "Memory": float64(245760),
			"Zone": "cn-wlcb-01", "ZoneLabel": "华北二A", "image": "InfiniteTalk",
			"SystemDisk": "SSD 云盘 200GB", "DataDisk": "SSD 云数据盘 100GB",
			"ChargeType": "Postpay", "price": "¥7.12/小时（预估）",
		}, &workflow.ConfirmForm{Step: &workflow.ConfirmFormStep{
			Index: 6, Title: "第六步，确认镜像与计费", Final: true,
		}})
		require.True(t, resolution.Confirmed)
	}

	_, err := eng.ChatWithOptions(context.Background(), "验证确认卡审计接线", noopStep, ChatOptions{
		ConfirmEditsFunc: func(string, map[string]any, *workflow.ConfirmForm) workflow.ConfirmResolution {
			return workflow.ConfirmResolution{Confirmed: true}
		},
	})
	require.NoError(t, err)
	require.Len(t, traces, 1)
	require.NotNil(t, traces[0].ConfirmedContract)
	assert.Equal(t, "H20", traces[0].ConfirmedContract.GPUType)
	assert.Equal(t, "华北二A", traces[0].ConfirmedContract.ZoneLabel)
}
