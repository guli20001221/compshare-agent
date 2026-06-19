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

func customImageMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":        customImageInstanceResult("Running"),
		"CreateCompShareCustomImage":       {"CompShareImageId": "cimg-custom-001"},
		"GetCompShareImageCreateProgress":  {"Process": float64(65.5), "TotalDuration": float64(300), "RemainingDuration": float64(105)},
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

	var sawConfirm, sawCreate, sawDelete bool
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
		if tool == "TerminateCompShareCustomImage" || tool == "TerminateCompShareInstance" || tool == "DeleteCompshareDisk" {
			sawDelete = true
		}
	}
	assert.True(t, sawConfirm, "workflow must have a confirmation step")
	assert.True(t, sawCreate, "workflow must create a custom image")
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
	assert.Contains(t, result.Message, "Name")
	assert.NotContains(t, executor.calls, executorCall{action: "CreateCompShareCustomImage"})
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
	for _, key := range []string{"Softwares", "SoftwarePorts", "FirewallPorts"} {
		assert.NotContains(t, createCall.args, key, "v1 must not pass %s", key)
	}

	progressCall, ok := findExecutorCall(executor.calls, "GetCompShareImageCreateProgress")
	require.True(t, ok)
	assert.Equal(t, "cimg-custom-001", progressCall.args["CompShareImageId"])
	assert.Equal(t, "cn-sh2", progressCall.args["Region"])
	assert.Equal(t, "cn-sh2-02", progressCall.args["Zone"])
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
