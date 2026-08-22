package workflow

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func containerRunningMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":      "uhost-xxx",
				"Name":         "my-container",
				"State":        "Running",
				"InstanceType": "Container",
				"Region":       "cn-wlcb",
				"Zone":         "cn-wlcb-01",
				"GpuType":      "4090",
				"GPU":          float64(1),
				"ChargeType":   "Dynamic",
			},
		}},
		"DescribeCompShareSupportZone":   cfsSupportZone("cn-wlcb-01", "cn-wlcb", "华北二A", 9001, 3001, true),
		"ResetCompShareInstancePassword": {"UHostId": "uhost-xxx", "RetCode": float64(0)},
	}}
}

func vmStoppedMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":      "uhost-yyy",
				"Name":         "my-vm",
				"State":        "Stopped",
				"InstanceType": "Normal",
				"Region":       "cn-wlcb",
				"Zone":         "cn-wlcb-01",
				"GpuType":      "A100",
				"GPU":          float64(1),
				"ChargeType":   "Month",
			},
		}},
		"ResetCompShareInstancePassword": {"UHostId": "uhost-yyy", "RetCode": float64(0)},
	}}
}

func vmRunningMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":      "uhost-yyy",
				"Name":         "my-vm",
				"State":        "Running",
				"InstanceType": "Normal",
				"Region":       "cn-wlcb",
				"Zone":         "cn-wlcb-01",
				"GpuType":      "A100",
				"GPU":          float64(1),
				"ChargeType":   "Month",
			},
		}},
	}}
}

func TestResetPassword_ContainerRunning_HappyPath(t *testing.T) {
	executor := containerRunningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "NewPass123!",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "工作流执行完成", result.Message)

	assert.Len(t, result.Steps, 5)
	expectedNames := []string{"查询实例", "查询支持区", "确认重置", "重置密码", "确认完成"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	// Check password was base64 encoded
	resetCall, found := findExecutorCall(executor.calls, "ResetCompShareInstancePassword")
	require.True(t, found)
	assert.Equal(t, uint32(9001), resetCall.args["zone_id"], "Container reset-password must carry the catalog-resolved zone_id")
	assert.Equal(t, uint32(3001), resetCall.args["az_group"], "Container reset-password must carry the catalog-resolved az_group")
	encoded := resetCall.args["Password"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	assert.NoError(t, err)
	assert.Equal(t, "NewPass123!", string(decoded))
}

func TestResetPassword_VMStopped_HappyPath(t *testing.T) {
	executor := vmStoppedMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-yyy",
		"Password": "SecureP@ss1",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
}

func TestResetPassword_UHostContainerStopped_HappyPath(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":      "uhost-xxx",
				"Name":         "my-container",
				"State":        "Stopped",
				"InstanceType": "Container",
				"Region":       "cn-wlcb",
				"Zone":         "cn-wlcb-01",
				"GpuType":      "4090",
				"GPU":          float64(1),
				"ChargeType":   "Dynamic",
			},
		}},
		"DescribeCompShareSupportZone":   cfsSupportZone("cn-wlcb-01", "cn-wlcb", "华北二A", 9001, 3001, false),
		"ResetCompShareInstancePassword": {"UHostId": "uhost-xxx", "RetCode": float64(0)},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "NewPass123!",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	_, reset := findExecutorCall(executor.calls, "ResetCompShareInstancePassword")
	assert.True(t, reset)
}

// Pod reset executes chpasswd inside the running guest. Unlike a UHost
// container, a stopped Pod cannot merely persist the credential for next boot.
func TestResetPassword_PodStopped_Rejected(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "cpod-xxx", "State": "Stopped", "InstanceType": "Container",
				"Region": "cn-wlcb", "Zone": "cn-wlcb-01", "ChargeType": "Postpay",
			},
		}},
	}}
	result, err := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("stopped Pod must be rejected before confirmation")
		return true
	}, nil).Run(context.Background(), ResetPasswordDef(), map[string]any{
		"UHostId": "cpod-xxx", "Password": "NewPass123!",
	})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "Pod 实例需要处于 Running")
	assert.Len(t, executor.calls, 1)
}

func TestResetPassword_VMRunning_Rejected(t *testing.T) {
	executor := vmRunningMockExecutor()
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-yyy",
		"Password": "NewPass123!",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "关机")
	assert.Len(t, executor.calls, 1)
}

func TestResetPassword_ConfirmMasksPassword(t *testing.T) {
	executor := containerRunningMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "MySecret123!",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	// Password must be masked in confirmation
	assert.Equal(t, "[已设置,不显示]", capturedArgs["Password"])

	// Warning about password rules present
	warning, ok := capturedArgs["warning"].(string)
	assert.True(t, ok)
	assert.Contains(t, warning, "8-32")
}

func TestResetPassword_PasswordTooShort(t *testing.T) {
	executor := containerRunningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "Ab1!", // 4 chars, too short
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "重置密码", result.StoppedAt)
	assert.Contains(t, result.Message, "8-32")
}

func TestResetPassword_PasswordSingleClass(t *testing.T) {
	executor := containerRunningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "abcdefghij", // 10 chars, only lowercase
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "重置密码", result.StoppedAt)
	assert.Contains(t, result.Message, "2 种")
}

func TestResetPassword_PasswordEmpty(t *testing.T) {
	executor := containerRunningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "不能为空")
}

func TestResetPassword_ConfirmDenied(t *testing.T) {
	executor := containerRunningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "NewPass123!",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认重置", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)
}

func TestResetPassword_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-nonexistent",
		"Password": "NewPass123!",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
}

// --- Password validation split by instance type ---

func TestResetPassword_VM_SkipsLocalValidation(t *testing.T) {
	// VM password with only one char class — should NOT be rejected locally.
	// The UHost API handles VM password validation.
	executor := vmStoppedMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-yyy",
		"Password": "alllowercase", // only lowercase — would fail container rules
	})

	assert.NoError(t, err)
	assert.True(t, result.Success, "VM password should not be locally validated")

	// Verify the API was called with base64-encoded password
	var resetArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "ResetCompShareInstancePassword" {
			resetArgs = call.args
			break
		}
	}
	assert.NotNil(t, resetArgs)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("alllowercase")), resetArgs["Password"])
}

func TestResetPassword_Container_WhitelistRejectsBackslash(t *testing.T) {
	// Backslash is NOT in the container special char whitelist.
	executor := containerRunningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": `Hello123\`,
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "重置密码", result.StoppedAt)
	assert.Contains(t, result.Message, "不允许的字符")
}

func TestResetPassword_Container_WhitelistAcceptsValidSpecial(t *testing.T) {
	// All chars from the documented whitelist should be accepted.
	executor := containerRunningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResetPasswordDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "Pass!@#$1",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
}
