package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_SealTamperFailsBeforeMutatingStep pins P4 acceptance #3: once the user
// confirms, a business param rewritten by a later step must not silently reach a
// mutating call. A post-confirm step tampers GpuType inside its own BuildArgs.
//
// The inputs below are unchanged; the expectations moved, and that is the fix.
// This test used to assert StoppedAt=="写操作" and executor.calls==["ToolA"] —
// i.e. it recorded that the TAMPERING step's own call went through on the
// tampered params, and only the step AFTER it was blocked. That is weaker than
// the property named on the first line of this comment, and it is not academic:
// in CreateInstanceWorkflow the write is the last mutating step, so "blocked at
// the next step" would mean the instance was already created. Now the check runs
// after BuildArgs, so the tampering step fails-stop on itself and reaches no
// executor at all.
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
	assert.Equal(t, "篡改步骤", result.StoppedAt, "the step that rewrote a confirmed param must fail-stop on ITSELF")
	assert.Contains(t, result.Message, "执行合同校验失败")
	require.Empty(t, executor.calls, "a step that rewrote a confirmed param must reach no executor at all — not even its own call")
}

// TestRun_SealTamperInsideTheWriteStepBlocksThatWrite is the case the previous
// ordering could not catch, and the one that matters: the tampering happens
// inside the LAST mutating step's own BuildArgs, with no later step to notice.
// This is the shape of CreateInstanceWorkflow, whose 创建实例 is followed only by
// an Optional read-back — so a check that defers to "the next step" defers to
// nothing, and the unconfirmed write lands.
func TestRun_SealTamperInsideTheWriteStepBlocksThatWrite(t *testing.T) {
	executor := &mockExecutor{}
	confirmYes := ConfirmFunc(func(string, map[string]any) bool { return true })
	def := &Definition{
		Name: "LastStepTamperWorkflow",
		Steps: []Step{
			{Name: "确认", Type: StepConfirm, BuildArgs: func(*Context) (map[string]any, error) { return map[string]any{}, nil }},
			{Name: "创建", Type: StepToolCall, Tool: "CreateX", BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				// The write rewrites a confirmed field and then builds its call from it.
				wfCtx.Params["GpuType"] = "TAMPERED"
				return map[string]any{"GpuType": wfCtx.Params["GpuType"]}, nil
			}},
		},
	}

	eng := NewEngine(executor, confirmYes, func(StepEvent) {})
	result, err := eng.Run(context.Background(), def, map[string]any{"GpuType": "4090"})

	require.NoError(t, err)
	require.False(t, result.Success)
	assert.Equal(t, "创建", result.StoppedAt)
	assert.Contains(t, result.Message, "执行合同校验失败")
	require.Empty(t, executor.calls, "the unconfirmed create must never be issued; there is no later step to catch it")
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
