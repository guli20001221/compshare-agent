package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateInstanceCommandV100EntersGuidedV100SCard(t *testing.T) {
	var seenGPUArgs []string
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "Describe": "华北二A", "IsPod": false},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04", "Status": "Available"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "V100S", "Zone": "cn-wlcb-01", "Status": "Normal",
					"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(10), "Memory": []any{float64(64)}},
					}}}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentCreateInstance,
			SpeechAct:     intent.SpeechActCommand,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react path must not be reached"}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentCreateInstance},
		Model:          "test-planner-model",
	})
	confirm := func(_ string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		if gt, _ := args["GpuType"].(string); gt != "" {
			seenGPUArgs = append(seenGPUArgs, gt)
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	reply, err := eng.ChatWithOptions(context.Background(), "为我创一台v100", noopStep, ChatOptions{
		GuidedCreate:     true,
		ConfirmEditsFunc: confirm,
	})

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.Empty(t, mock.calls, "create_instance command must not depend on ReAct choosing a tool")
	assert.Contains(t, seenGPUArgs, "V100S")
}

func TestCreateInstanceNonCommandDoesNotOpenCreateCard(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  intent.SpeechAct
		msg  string
	}{
		{name: "advice", act: intent.SpeechActQuestion, msg: "4090 适合训练大模型吗"},
		{name: "comparison", act: intent.SpeechActComparison, msg: "我应该选 4090 还是 A100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
				return map[string]any{"RetCode": float64(0)}, nil
			}}
			planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
				Plan: intent.IntentRoute{
					SchemaVersion: intent.SchemaVersion,
					Intent:        intent.IntentCreateInstance,
					SpeechAct:     tc.act,
					Retrieval:     intent.Retrieval{Enabled: false},
					Confidence:    0.9,
				},
			}}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.SetIntentPlanner(planner, IntentPlannerOptions{
				EnabledIntents: []intent.Intent{intent.IntentCreateInstance},
				Model:          "test-planner-model",
			})

			reply, err := eng.ChatWithOptions(context.Background(), tc.msg, noopStep, ChatOptions{GuidedCreate: true})

			require.NoError(t, err)
			assert.Contains(t, reply, "不会直接")
			assert.NotContains(t, executor.calls, "CreateCompShareInstance")
		})
	}
}

func TestCreatePreferenceExtractorPrefillsCreateImageOnlyAfterCommand(t *testing.T) {
	extractor := &fakeCreatePreferenceExtractor{result: CreatePreferenceExtractionResult{
		GPUPref:   "4090",
		ImagePref: "PyTorch 最新镜像",
	}}
	var imageArgs map[string]any
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "Describe": "华北二A", "IsPod": false},
			}}, nil
		case "DescribeCompShareImages":
			imageArgs = args
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-torch", "Name": "cuda128_torch291_py312", "Status": "Available"},
				map[string]any{"CompShareImageId": "img-win", "Name": "Windows 2022", "Status": "Available"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
					"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}}}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentCreateInstance,
			SpeechAct:     intent.SpeechActCommand,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "react path must not be reached"}}}, executor, nil)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentCreateInstance},
		Model:          "test-planner-model",
	})
	eng.SetCreatePreferenceExtractor(extractor)
	eng.SetCreatePreferenceExtractionEnabled(true)
	confirm := func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_, err := eng.ChatWithOptions(context.Background(), "用 PyTorch 最新镜像创建一台 4090", noopStep, ChatOptions{
		GuidedCreate:     true,
		ConfirmEditsFunc: confirm,
	})

	require.NoError(t, err)
	require.Len(t, extractor.calls, 1)
	assert.Equal(t, intent.IntentCreateInstance, extractor.calls[0].Intent)
	assert.Equal(t, intent.SpeechActCommand, extractor.calls[0].SpeechAct)
	assert.Equal(t, "torch", imageArgs["Name"])
}

func TestCreatePreferenceExtractorFailureKeepsParsedCreateArgs(t *testing.T) {
	extractor := &fakeCreatePreferenceExtractor{err: errors.New("extract failed")}
	var seenGPUArgs []string
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "Describe": "华北二A", "IsPod": false},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04", "Status": "Available"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "V100S", "Zone": "cn-wlcb-01", "Status": "Normal",
					"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(10), "Memory": []any{float64(64)}},
					}}}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentCreateInstance,
			SpeechAct:     intent.SpeechActCommand,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "react path must not be reached"}}}, executor, nil)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentCreateInstance},
		Model:          "test-planner-model",
	})
	eng.SetCreatePreferenceExtractor(extractor)
	eng.SetCreatePreferenceExtractionEnabled(true)
	confirm := func(_ string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		if gt, _ := args["GpuType"].(string); gt != "" {
			seenGPUArgs = append(seenGPUArgs, gt)
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	reply, err := eng.ChatWithOptions(context.Background(), "为我创一台v100", noopStep, ChatOptions{
		GuidedCreate:     true,
		ConfirmEditsFunc: confirm,
	})

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.Contains(t, seenGPUArgs, "V100S")
}

func TestCreatePreferenceExtractorFailureDoesNotFallbackToImageWordlist(t *testing.T) {
	extractor := &fakeCreatePreferenceExtractor{err: errors.New("extract failed")}
	var imageArgs map[string]any
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "Describe": "华北二A", "IsPod": false},
			}}, nil
		case "DescribeCompShareImages":
			imageArgs = args
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04", "Status": "Available"},
				map[string]any{"CompShareImageId": "img-torch", "Name": "cuda128_torch291_py312", "Status": "Available"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
					"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}}}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentCreateInstance,
			SpeechAct:     intent.SpeechActCommand,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "react path must not be reached"}}}, executor, nil)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentCreateInstance},
		Model:          "test-planner-model",
	})
	eng.SetCreatePreferenceExtractor(extractor)
	eng.SetCreatePreferenceExtractionEnabled(true)
	confirm := func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_, err := eng.ChatWithOptions(context.Background(), "为我用 PyTorch 最新镜像创建一台 4090", noopStep, ChatOptions{
		GuidedCreate:     true,
		ConfirmEditsFunc: confirm,
	})

	require.NoError(t, err)
	require.NotNil(t, imageArgs)
	assert.NotEqual(t, "torch", imageArgs["Name"], "failed stage-2 extraction must not silently fall back to the old image keyword table")
}

func TestCreatePreferenceExtractorFailureDoesNotBlockDeployDispatch(t *testing.T) {
	extractor := &fakeCreatePreferenceExtractor{err: errors.New("extract failed")}
	eng := NewWithDeps(&mockLLM{}, &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": float64(0)}, nil
	}}, nil)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{}}, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentDeployModel},
		Model:          "test-planner-model",
	})
	eng.SetCreatePreferenceExtractor(extractor)
	eng.SetCreatePreferenceExtractionEnabled(true)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentDeployModel,
		SpeechAct:     intent.SpeechActCommand,
		Retrieval:     intent.Retrieval{Enabled: false},
		Confidence:    0.9,
	}}}

	reply, handled := eng.tryCreateFamilySpeechActDispatch(context.Background(), dispatch, "部署 DeepSeek R1 32B", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	require.Len(t, extractor.calls, 1)
	assert.Nil(t, eng.createPreferenceThisTurn)
}

type fakeCreatePreferenceExtractor struct {
	result CreatePreferenceExtractionResult
	err    error
	calls  []CreatePreferenceExtractionInput
}

func (f *fakeCreatePreferenceExtractor) ExtractCreatePreferences(_ context.Context, input CreatePreferenceExtractionInput) (CreatePreferenceExtractionResult, error) {
	f.calls = append(f.calls, input)
	if f.err != nil {
		return CreatePreferenceExtractionResult{}, f.err
	}
	return f.result, nil
}
