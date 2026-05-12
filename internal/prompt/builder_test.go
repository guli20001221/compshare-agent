package prompt

import (
	"strings"
	"testing"
)

func TestBuildSystem_WithContext(t *testing.T) {
	ctx := "您有 2 个实例（1 个运行中）"
	result := BuildSystem(ctx)

	if !strings.Contains(result, ctx) {
		t.Error("BuildSystem should inject user context into prompt")
	}
	if !strings.Contains(result, "优云算力共享平台") {
		t.Error("BuildSystem should contain platform identity")
	}
}

func TestBuildSystem_EmptyContext(t *testing.T) {
	result := BuildSystem("")
	if !strings.Contains(result, "暂无用户信息") {
		t.Error("empty context should use default placeholder")
	}
}

func TestFormatInstanceContext_Empty(t *testing.T) {
	result := FormatInstanceContext(map[string]any{})
	if result != "用户当前没有实例。" {
		t.Errorf("empty result = %q, want no-instance message", result)
	}
}

func TestFormatInstanceContext_NilUHostSet(t *testing.T) {
	result := FormatInstanceContext(map[string]any{"UHostSet": nil})
	if result != "用户当前没有实例。" {
		t.Errorf("nil UHostSet = %q, want no-instance message", result)
	}
}

func TestFormatInstanceContext_WithInstances(t *testing.T) {
	apiResult := map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-abc",
				"Name":       "my-gpu",
				"State":      "Running",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Postpay",
			},
			map[string]any{
				"UHostId":    "uhost-def",
				"Name":       "test",
				"State":      "Stopped",
				"GpuType":    "3080Ti",
				"GPU":        float64(1),
				"ChargeType": "Month",
			},
		},
	}

	result := FormatInstanceContext(apiResult)

	if !strings.Contains(result, "2 个实例") {
		t.Error("should report 2 instances")
	}
	if !strings.Contains(result, "1 个运行中") {
		t.Error("should report 1 running")
	}
	if !strings.Contains(result, "uhost-abc") {
		t.Error("should contain instance ID")
	}
	if !strings.Contains(result, "运行中") {
		t.Error("should translate Running state")
	}
}

func TestTranslateState(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Running", "运行中"},
		{"Stopped", "关机"},
		{"Starting", "启动中"},
		{"Install", "初始化中"},
		{"Install Fail", "初始化失败"},
		{"UnknownState", "UnknownState"},
	}
	for _, tt := range tests {
		got := translateState(tt.input)
		if got != tt.want {
			t.Errorf("translateState(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildSystem_ContainsDiagnosis(t *testing.T) {
	prompt := BuildSystem("test context")
	for _, tool := range []string{"DiagnoseSSH", "DiagnoseInitFailure", "DiagnoseGPU", "DiagnoseBilling"} {
		if !strings.Contains(prompt, tool) {
			t.Errorf("system prompt should contain %s routing", tool)
		}
	}
}

func TestBuildSystemWithOptions_ReadOnlyHidesMutatingWorkflowGuidance(t *testing.T) {
	prompt := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false})
	for _, text := range []string{
		"CreateInstanceWorkflow",
		"StopInstanceWorkflow",
		"StartInstanceWorkflow",
		"RebootInstanceWorkflow",
		"RenameInstanceWorkflow",
		"ResetPasswordWorkflow",
		"SetStopSchedulerWorkflow",
		"CancelStopSchedulerWorkflow",
		"必须使用 CreateInstanceWorkflow",
		"使用工作流 Tool",
		"变更类操作必须展示参数让用户确认后再执行",
		"/start.d/",
		"sudo apt",
		"ollama serve",
		"systemctl",
		"ldconfig",
	} {
		if strings.Contains(prompt, text) {
			t.Fatalf("read-only prompt should not contain mutating guidance %q", text)
		}
	}
	for _, text := range []string{
		"当前阶段不直接执行开机、关机、重启",
		"可以提供控制台操作步骤",
		"诊断仅限云侧只读检查",
		"DiagnoseSSH",
	} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("read-only prompt should contain %q", text)
		}
	}
}

func TestReadOnlyFAQContentOmitsInstanceInternalCommands(t *testing.T) {
	for _, text := range []string{
		"/start.d/",
		"sudo apt",
		"ollama serve",
		"systemctl",
		"ldconfig",
		"StartInstanceWorkflow",
	} {
		if strings.Contains(ReadOnlyFAQContent, text) {
			t.Fatalf("read-only FAQ should not contain %q", text)
		}
	}
	for _, text := range []string{
		"计费/回收规则",
		"连接实例",
		"控制台",
		"不登录实例、不执行远程命令",
	} {
		if !strings.Contains(ReadOnlyFAQContent, text) {
			t.Fatalf("read-only FAQ should contain %q", text)
		}
	}
}

func TestBuildSystemWithOptions_MutatingModeKeepsWorkflowGuidance(t *testing.T) {
	prompt := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	for _, text := range []string{
		"CreateInstanceWorkflow",
		"StopInstanceWorkflow",
		"ResetPasswordWorkflow",
	} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("mutating-enabled prompt should contain %q", text)
		}
	}
}

func TestFormatToolResult_Truncation(t *testing.T) {
	// Build a large result with an array field
	items := make([]any, 100)
	for i := range items {
		items[i] = map[string]any{
			"id":   strings.Repeat("x", 50),
			"data": strings.Repeat("y", 200),
		}
	}
	large := map[string]any{"items": items}
	result := FormatToolResult(large)

	// Must still be valid JSON
	if !strings.HasPrefix(result, "{") || !strings.HasSuffix(result, "}") {
		t.Errorf("truncated result should be valid JSON structure, got: %s...%s",
			result[:20], result[len(result)-20:])
	}

	// Should contain truncation notice
	if strings.Contains(result, "...(truncated)") {
		t.Error("should NOT use old-style string truncation")
	}
}

func TestFormatToolResult_SmallResult(t *testing.T) {
	result := FormatToolResult(map[string]any{"key": "value"})
	if result != `{"key":"value"}` {
		t.Errorf("small result = %q, want exact JSON", result)
	}
}

func TestFormatToolResult_ValidJSON(t *testing.T) {
	// Even large results should produce parseable output
	items := make([]any, 50)
	for i := range items {
		items[i] = strings.Repeat("大", 100) // Chinese chars
	}
	large := map[string]any{"list": items, "count": 50}
	result := FormatToolResult(large)

	// Verify it doesn't cut mid-string
	if strings.Contains(result, "...(truncated)") {
		t.Error("should not use old-style truncation")
	}
}
