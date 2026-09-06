package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func customImageInstanceResult(state string) map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId": "uhost-src",
			"Name":    "train-env",
			"State":   state,
			"Region":  "cn-sh2",
			"Zone":    "cn-sh2-02",
		},
	}}
}

func customImageInstanceResultWithoutZoneRegion() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId": "uhost-src",
			"Name":    "train-env",
			"State":   "Running",
		},
	}}
}

func customImageContainerInstanceResult(state string) map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":      "cpod-src",
			"Name":         "container-env",
			"State":        state,
			"Region":       "cn-pod",
			"Zone":         "cn-pod-01",
			"InstanceType": "Container",
		},
	}}
}

func customImageMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":       customImageInstanceResult("Stopped"),
		"DescribeCompShareSupportZone":    customImageSupportZoneResult(),
		"CreateCompShareCustomImage":      {"CompShareImageId": "cimg-custom-001"},
		"GetCompShareImageCreateProgress": {"Process": float64(65.5), "TotalDuration": float64(300), "RemainingDuration": float64(105)},
		"DescribeCompShareCustomImages": {"ImageSet": []any{map[string]any{
			"CompShareImageId": "cimg-custom-001", "Status": "Making",
		}}},
		"TerminateCompShareCustomImage":    {"RetCode": 0},
		"PublishCompShareCustomImage":      {"RetCode": 0},
		"ModifyCompShareImageShareAccount": {"RetCode": 0},
		"UpdateCompShareImage":             {"RetCode": 0},
		"CreateAndAttachCompshareDisk":     {"RetCode": 0},
		"DeleteCompShareStopScheduler":     {"RetCode": 0},
		"ReinstallCompShareInstance":       {"RetCode": 0},
		"TerminateCompShareInstance":       {"RetCode": 0},
		"DeleteCompshareDisk":              {"RetCode": 0},
		"DeleteCompShareTeam":              {"RetCode": 0},
	}}
}

func customImageSupportZoneResult() map[string]any {
	return map[string]any{"ZoneInfo": []any{map[string]any{
		"Zone": "cn-sh2-02", "Region": "cn-sh2",
		"ZoneId": float64(8200), "RegionId": float64(1000009),
	}}}
}

func findExecutorCall(calls []executorCall, action string) (executorCall, bool) {
	for _, call := range calls {
		if call.action == action {
			return call, true
		}
	}
	return executorCall{}, false
}

func TestCreateCustomImage_DefinitionIsRegistered(t *testing.T) {
	def, ok := GetWorkflow("CreateCustomImageWorkflow")
	require.True(t, ok)
	require.NotNil(t, def)
	assert.Equal(t, "CreateCustomImageWorkflow", def.Name)

	var sawConfirm, sawCreate, sawStopOrWait, sawDelete bool
	for i, step := range def.Steps {
		if step.Type == StepConfirm {
			sawConfirm = true
		}
		tool := step.Tool
		if step.ToolFunc != nil {
			tool = step.ToolFunc(NewContext(map[string]any{}))
		}
		if tool == "CreateCompShareCustomImage" {
			sawCreate = true
			assert.True(t, sawConfirm, "create step must appear after confirmation; step index %d", i)
		}
		if tool == "StopCompShareInstance" || step.Name == "等待源实例关机" {
			sawStopOrWait = true
		}
		if tool == "TerminateCompShareCustomImage" || tool == "TerminateCompShareInstance" || tool == "DeleteCompshareDisk" {
			sawDelete = true
		}
	}
	assert.True(t, sawConfirm, "workflow must have a confirmation step")
	assert.True(t, sawCreate, "workflow must create a custom image")
	assert.False(t, sawStopOrWait, "custom-image creation must not stop or wait for a normal VM")
	assert.False(t, sawDelete, "workflow must not include destructive steps")
}

func TestCreateCustomImage_MissingUHostIdFailsBeforeAnyAPICall(t *testing.T) {
	executor := customImageMockExecutor()
	def := CreateCustomImageDef()
	eng := NewEngine(executor, nil, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"Name": "snapshot-v1",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "UHostId")
	assert.Empty(t, executor.calls)
}

func TestCreateCustomImage_MissingNameAsksBeforeMutatingCall(t *testing.T) {
	executor := customImageMockExecutor()
	def := CreateCustomImageDef()
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("confirmation must not fire when image name is missing")
		return false
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-src",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "创建自制镜像需要指定名称")
	assert.Equal(t, []string{"name"}, result.MissingSlots)
	assert.NotContains(t, executor.calls, executorCall{action: "CreateCompShareCustomImage"})
	_, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	assert.False(t, created)
}

func TestCreateCustomImage_InvalidNameStopsBeforeConfirmation(t *testing.T) {
	executor := customImageMockExecutor()
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("upstream-invalid image name must be rejected before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateCustomImageDef(), map[string]any{
		"UHostId": "uhost-src", "Name": "snapshot with spaces",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "只能包含")
	_, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	assert.False(t, created)
}

func TestCreateCustomImage_ConfirmDeniedStopsBeforeMutatingCall(t *testing.T) {
	executor := customImageMockExecutor()
	def := CreateCustomImageDef()
	confirmCalls := 0
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		confirmCalls++
		assert.Equal(t, "CreateCustomImageWorkflow", action)
		assert.Equal(t, "snapshot-v1", args["Name"])
		assert.Equal(t, "uhost-src", args["UHostId"])
		assert.Equal(t, "cn-sh2", args["Region"])
		assert.Equal(t, "cn-sh2-02", args["Zone"])
		return false
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-src",
		"Name":         "snapshot-v1",
		"Description":  "training environment",
		"Softwares":    "must-not-pass",
		"SoftwarePort": "must-not-pass",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, 1, confirmCalls)
	_, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	assert.False(t, created)
}

func TestCreateCustomImage_HappyPathThreadsSourceZoneRegionAndQueriesProgress(t *testing.T) {
	executor := customImageMockExecutor()
	def := CreateCustomImageDef()
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":       "uhost-src",
		"Name":          "snapshot-v1",
		"Description":   "training environment",
		"Region":        "must-not-pass",
		"Zone":          "must-not-pass",
		"Softwares":     "must-not-pass",
		"SoftwarePorts": "must-not-pass",
		"FirewallPorts": "must-not-pass",
	})

	require.NoError(t, err)
	assert.True(t, result.Success)
	require.NotNil(t, result.Data)
	assert.Equal(t, "cimg-custom-001", result.Data["CompShareImageId"])
	progress, ok := result.Data["Progress"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(65.5), progress["Process"])

	createCall, ok := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	require.True(t, ok)
	assert.Equal(t, "uhost-src", createCall.args["UHostId"])
	assert.Equal(t, "snapshot-v1", createCall.args["Name"])
	assert.Equal(t, "training environment", createCall.args["Description"])
	assert.Equal(t, "cn-sh2", createCall.args["Region"])
	assert.Equal(t, "cn-sh2-02", createCall.args["Zone"])
	assert.Equal(t, uint32(1000009), createCall.args["az_group"])
	assert.Equal(t, uint32(8200), createCall.args["zone_id"])
	for _, key := range []string{"Softwares", "SoftwarePorts", "FirewallPorts"} {
		assert.NotContains(t, createCall.args, key, "v1 must not pass %s", key)
	}

	progressCall, ok := findExecutorCall(executor.calls, "GetCompShareImageCreateProgress")
	require.True(t, ok)
	assert.Equal(t, "cimg-custom-001", progressCall.args["CompShareImageId"])
	assert.Equal(t, "cn-sh2", progressCall.args["Region"])
	assert.Equal(t, "cn-sh2-02", progressCall.args["Zone"])
}

func TestCreateCustomImage_RunningVMCreatesWithoutStopping(t *testing.T) {
	executor := customImageMockExecutor()
	executor.results["DescribeCompShareInstance"] = customImageInstanceResult("Running")
	def := CreateCustomImageDef()
	eng := NewEngine(executor, func(_ string, args map[string]any) bool {
		assert.Contains(t, args["warning"], "不会关闭源实例")
		assert.Contains(t, args["warning"], "Making")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-src",
		"Name":    "snapshot-v1",
	})

	require.NoError(t, err)
	require.True(t, result.Success)
	_, stopped := findExecutorCall(executor.calls, "StopCompShareInstance")
	require.False(t, stopped, "a running VM must not be stopped before image creation")
	_, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	require.True(t, created, "image creation must proceed from a running VM")
	assert.Len(t, executor.calls, 4, "query instance + query zone + create + optional progress")
}

func TestCreateCustomImage_StatusReadbackMatchesSourceShape(t *testing.T) {
	for _, tc := range []struct {
		name         string
		id           string
		instanceType string
		wantCatalog  bool
	}{
		{"pod", "cpod-src", "Container", true},
		{"uhost-container", "uhost-src", "Container", true},
		{"vm", "uhost-src", "UHost", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := customImageMockExecutor()
			source := customImageInstanceResult("Running")
			if tc.name == "pod" {
				source = customImageContainerInstanceResult("Running")
				executor.results["DescribeCompShareSupportZone"] = map[string]any{"ZoneInfo": []any{map[string]any{
					"Zone": "cn-pod-01", "Region": "cn-pod",
					"ZoneId": float64(8300), "RegionId": float64(1000010),
				}}}
			}
			firstUHost(source)["InstanceType"] = tc.instanceType
			executor.results["DescribeCompShareInstance"] = source
			eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

			result, err := eng.Run(context.Background(), CreateCustomImageDef(), map[string]any{
				"UHostId": tc.id,
				"Name":    "snapshot-v1",
			})

			require.NoError(t, err)
			require.True(t, result.Success)
			require.NotNil(t, result.Data)
			assert.Equal(t, "cimg-custom-001", result.Data["CompShareImageId"])
			vmProgress, vmProgressCalled := findExecutorCall(executor.calls, "GetCompShareImageCreateProgress")
			catalog, catalogCalled := findExecutorCall(executor.calls, "DescribeCompShareCustomImages")
			assert.Equal(t, tc.wantCatalog, catalogCalled)
			assert.Equal(t, !tc.wantCatalog, vmProgressCalled)
			if tc.wantCatalog {
				assert.Equal(t, "Making", result.Data["Status"])
				assert.NotContains(t, result.Data, "Progress")
				assert.Equal(t, "cimg-custom-001", catalog.args["CompShareImageId"])
				assert.Equal(t, 1, catalog.args["Limit"])
			} else {
				progress, ok := result.Data["Progress"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, float64(65.5), progress["Process"])
				assert.NotContains(t, result.Data, "Status")
				assert.Equal(t, "cimg-custom-001", vmProgress.args["CompShareImageId"])
			}
		})
	}
}

func TestCreateCustomImage_MissingSourceZoneRegionStopsBeforeConfirmation(t *testing.T) {
	executor := customImageMockExecutor()
	executor.results["DescribeCompShareInstance"] = customImageInstanceResultWithoutZoneRegion()
	def := CreateCustomImageDef()
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("confirmation must not fire without source Zone/Region")
		return false
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-src",
		"Name":    "snapshot-v1",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "源实例缺少可用区")
	_, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	assert.False(t, created)
}

func TestCreateCustomImage_StoppedContainerSourceBlockedBeforeConfirmation(t *testing.T) {
	executor := customImageMockExecutor()
	executor.results["DescribeCompShareInstance"] = customImageContainerInstanceResult("Stopped")
	def := CreateCustomImageDef()
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("容器来源实例未运行时不应进入确认")
		return false
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "cpod-src",
		"Name":    "snapshot-v1",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "容器")
	assert.Contains(t, result.Message, "开机")
	_, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	assert.False(t, created)
}

func TestCreateCustomImage_2C4GWithoutGpuVMBlockedBeforeConfirmation(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-no-gpu", "InstanceType": "UHost", "State": "Stopped",
				"Region": "cn-sh2", "Zone": "cn-sh2-02", "GPU": float64(0),
				"CPU": float64(2), "Memory": float64(4096),
				"WithoutGpuSpec": map[string]any{"Spec": "A", "Cpu": float64(2), "Memory": float64(4096)},
			}},
		},
	}}
	confirmCalls := 0
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool {
		confirmCalls++
		return true
	}, nil)
	result, err := eng.Run(context.Background(), CreateCustomImageDef(), map[string]any{
		"UHostId": "uhost-no-gpu", "Name": "snapshot",
	})
	require.NoError(t, err)
	require.False(t, result.Success)
	assert.Contains(t, result.Message, "2C/4GB 无卡模式")
	assert.Zero(t, confirmCalls)
	_, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	assert.False(t, created)
}

func TestCreateCustomImage_8C16GWithoutGpuVMMayReachConfirmation(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-no-gpu-b", "InstanceType": "UHost", "State": "Stopped",
				"Region": "cn-sh2", "Zone": "cn-sh2-02", "GPU": float64(0),
				"CPU": float64(8), "Memory": float64(16384),
				"WithoutGpuSpec": map[string]any{"Spec": "B", "Cpu": float64(8), "Memory": float64(16384)},
			}},
		},
	}}
	confirmCalls := 0
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool {
		confirmCalls++
		return false
	}, nil)
	result, err := eng.Run(context.Background(), CreateCustomImageDef(), map[string]any{
		"UHostId": "uhost-no-gpu-b", "Name": "snapshot",
	})
	require.NoError(t, err)
	require.False(t, result.Success)
	assert.Equal(t, 1, confirmCalls)
}

func TestCreateCustomImage_ProgressFailureDoesNotHideCreatedImage(t *testing.T) {
	executor := customImageMockExecutor()
	executor.failOn = "GetCompShareImageCreateProgress"
	def := CreateCustomImageDef()
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-src",
		"Name":    "snapshot-v1",
	})

	require.NoError(t, err)
	assert.True(t, result.Success, "image creation succeeded, so optional progress failure must not flip workflow failure")
	require.NotNil(t, result.Data)
	assert.Equal(t, "cimg-custom-001", result.Data["CompShareImageId"])
	assert.NotContains(t, result.Data, "Progress")
	createCall, created := findExecutorCall(executor.calls, "CreateCompShareCustomImage")
	require.True(t, created)
	assert.Equal(t, "snapshot-v1", createCall.args["Name"])
	_, progressQueried := findExecutorCall(executor.calls, "GetCompShareImageCreateProgress")
	assert.True(t, progressQueried)
}

// The confirmation card must state the ImageMaking consequences BEFORE the write.
// Driven through the real step rather than asserting the constant, because the
// constant being right is worthless if the card stops composing it in.
func TestCustomImageConfirmCardWarnsAboutTheSourceInstanceGoingIntoImageMaking(t *testing.T) {
	def := CreateCustomImageDef()
	var confirm *Step
	for i := range def.Steps {
		if def.Steps[i].Type == StepConfirm {
			confirm = &def.Steps[i]
			break
		}
	}
	require.NotNil(t, confirm, "premise: the custom-image flow still has a confirmation step")

	// The confirm step reads the two query results the flow already ran.
	wfCtx := NewContext(map[string]any{"UHostId": "uhost-src", "Name": "my-image"})
	wfCtx.StepResults["查询源实例"] = customImageInstanceResult("Stopped")
	wfCtx.StepResults["查询源实例可用区"] = customImageSupportZoneResult()
	args, err := confirm.BuildArgs(wfCtx)
	require.NoError(t, err)

	warning, _ := args["warning"].(string)
	require.NotEmpty(t, warning, "the card carries a warning; without one this test asserts nothing")

	assert.Contains(t, warning, CustomImageSourceInstanceNote,
		"the card stopped composing in the source-instance note, so the user confirms "+
			"without being told the machine loses its public address and refuses 开关机")
	assert.Contains(t, warning, "不会关闭源实例",
		"the earlier correction must survive: 制作 genuinely does not shut the instance down")
}

func TestPodCustomImageCardAndResultDoNotClaimUHostSideEffects(t *testing.T) {
	executor := customImageMockExecutor()
	executor.results["DescribeCompShareInstance"] = customImageContainerInstanceResult("Running")
	executor.results["DescribeCompShareSupportZone"] = map[string]any{"ZoneInfo": []any{map[string]any{
		"Zone": "cn-pod-01", "Region": "cn-pod", "ZoneId": float64(5001), "RegionId": float64(1000001),
	}}}
	warning := ""
	eng := NewEngine(executor, func(_ string, args map[string]any) bool {
		warning, _ = args["warning"].(string)
		return true
	}, nil)
	result, err := eng.Run(context.Background(), CreateCustomImageDef(), map[string]any{
		"UHostId": "cpod-src", "Name": "container-snapshot",
	})
	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	note, ok := result.Data["SourceInstanceNote"].(string)
	require.True(t, ok)
	require.NotEmpty(t, note)
	assert.Contains(t, warning, note)
	assert.Contains(t, note, "ImageMaking")
	assert.NotContains(t, warning, "公网地址会被释放")
	assert.NotContains(t, warning, "都会被拒绝")
}
