package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_SealTamperFailsBeforeMutatingStep pins P4 acceptance #3: once the user
// confirms, a business param rewritten by a later step must not silently reach a
// mutating call. A post-confirm step tampers GpuType; the following write step's
// digest check fails and the workflow fail-stops before that write runs.
func TestRun_SealTamperFailsBeforeMutatingStep(t *testing.T) {
	executor := &mockExecutor{}
	confirmYes := ConfirmFunc(func(string, map[string]any) bool { return true })
	def := &Definition{
		Name: "TamperWorkflow",
		Steps: []Step{
			{Name: "确认", Type: StepConfirm, BuildArgs: func(*Context) (map[string]any, error) { return map[string]any{}, nil }},
			{Name: "篡改步骤", Type: StepToolCall, Tool: "ToolA", BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				wfCtx.Params["GpuType"] = "TAMPERED" // rewrite a confirmed field after the seal
				return map[string]any{}, nil
			}},
			{Name: "写操作", Type: StepToolCall, Tool: "ToolB", BuildArgs: func(*Context) (map[string]any, error) { return map[string]any{}, nil }},
		},
	}

	eng := NewEngine(executor, confirmYes, func(StepEvent) {})
	result, err := eng.Run(context.Background(), def, map[string]any{"GpuType": "4090"})

	require.NoError(t, err)
	require.False(t, result.Success)
	assert.Equal(t, "写操作", result.StoppedAt)
	assert.Contains(t, result.Message, "执行合同校验失败")
	require.Len(t, executor.calls, 1, "only the tampering step ran; the write must be blocked")
	assert.Equal(t, "ToolA", executor.calls[0].action)
}

// TestRun_ConfirmCardAndWriteShareSealedContract pins P4 acceptance #4: the args
// shown on the confirm card, the args the write executes, and the sealed
// contract all carry the same confirmed values — one contract, not three
// independent re-derivations.
func TestRun_ConfirmCardAndWriteShareSealedContract(t *testing.T) {
	executor := &mockExecutor{}
	var confirmArgs map[string]any
	confirm := ConfirmFunc(func(_ string, args map[string]any) bool { confirmArgs = args; return true })
	def := &Definition{
		Name: "CreateLike",
		Steps: []Step{
			{Name: "确认", Type: StepConfirm, BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				return map[string]any{"GpuType": wfCtx.Params["GpuType"]}, nil
			}},
			{Name: "创建", Type: StepToolCall, Tool: "CreateX", BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				return map[string]any{"GpuType": wfCtx.Params["GpuType"]}, nil
			}},
		},
	}

	eng := NewEngine(executor, confirm, func(StepEvent) {})
	result, err := eng.Run(context.Background(), def, map[string]any{"GpuType": "4090"})

	require.NoError(t, err)
	require.True(t, result.Success)
	require.NotNil(t, result.Contract, "a confirmed workflow must surface its sealed contract")
	assert.Equal(t, "4090", confirmArgs["GpuType"], "confirm card")
	require.Len(t, executor.calls, 1)
	assert.Equal(t, "4090", executor.calls[0].args["GpuType"], "write call")
	assert.Equal(t, "4090", result.Contract.BusinessParams["GpuType"], "sealed contract")
	assert.True(t, result.Contract.verifyDigest(result.Contract.BusinessParams))
}

// TestRun_FormEditReConfirmsAndSealsFinalValue pins P4 acceptance #5: a confirm
// form edit invalidates the pre-edit draft — it forces a re-confirm and only the
// final confirmed value is sealed and executed, never the value the user changed
// away from.
func TestRun_FormEditReConfirmsAndSealsFinalValue(t *testing.T) {
	executor := &mockExecutor{}
	confirmCalls := 0
	edits := ConfirmEditsFunc(func(_ string, _ map[string]any, _ *ConfirmForm) ConfirmResolution {
		confirmCalls++
		if confirmCalls == 1 {
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"GpuType": "A100"}}
		}
		return ConfirmResolution{Confirmed: true}
	})
	def := &Definition{
		Name: "FormCreate",
		Steps: []Step{
			{
				Name: "确认",
				Type: StepConfirm,
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{"GpuType": wfCtx.Params["GpuType"]}, nil
				},
				BuildForm: func(wfCtx *Context) (*ConfirmForm, error) {
					return &ConfirmForm{Version: 1, Fields: []ConfirmFormField{{
						Key: "GpuType", Label: "GPU", Type: "select",
						Value: paramStr(wfCtx.Params, "GpuType", ""), Editable: true,
						Options: []ConfirmFormOption{{Value: "4090"}, {Value: "A100"}},
					}}}, nil
				},
				ApplyOverrides: func(wfCtx *Context, ov map[string]string) error {
					if v, ok := ov["GpuType"]; ok {
						wfCtx.Params["GpuType"] = v
					}
					return nil
				},
			},
			{Name: "创建", Type: StepToolCall, Tool: "CreateX", BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				return map[string]any{"GpuType": wfCtx.Params["GpuType"]}, nil
			}},
		},
	}

	eng := NewEngine(executor, nil, func(StepEvent) {})
	eng.SetConfirmEditsFn(edits)
	result, err := eng.Run(context.Background(), def, map[string]any{"GpuType": "4090"})

	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, 2, confirmCalls, "an override must force a re-confirm before the seal")
	require.NotNil(t, result.Contract)
	assert.Equal(t, "A100", result.Contract.BusinessParams["GpuType"], "seal must capture the edited final value, not the pre-edit one")
	require.Len(t, executor.calls, 1)
	assert.Equal(t, "A100", executor.calls[0].args["GpuType"], "the write must execute the edited value")
}
