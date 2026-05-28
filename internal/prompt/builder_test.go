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
	if !strings.Contains(result, "Compshare Copilot") {
		t.Error("BuildSystem should contain product brand (Compshare Copilot)")
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

func TestBuildSystem_ContainsDiagnosisCommandBoundary(t *testing.T) {
	prompt := BuildSystem("test context")
	for _, text := range []string{
		"实例内只读自查命令",
		"修改实例环境的命令必须标为可选修复",
	} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("system prompt should contain diagnosis command boundary %q", text)
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
		"ldconfig",
	} {
		if strings.Contains(prompt, text) {
			t.Fatalf("read-only prompt should not contain mutating guidance %q", text)
		}
	}
	for _, text := range []string{
		"当前阶段不直接执行开机、关机、重启",
		"可以提供控制台操作步骤",
		"诊断工具本身仅做云侧只读检查",
		"可以给用户实例内只读自查命令",
		"systemctl status",
		"修改实例环境的命令必须标为可选修复",
		"DiagnoseSSH",
	} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("read-only prompt should contain %q", text)
		}
	}
}

func TestBuildSystemWithOptions_DoesNotInjectStaticFAQContent(t *testing.T) {
	cases := map[string]string{
		"mutating":  BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true}),
		"read_only": BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false}),
	}
	for name, system := range cases {
		t.Run(name, func(t *testing.T) {
			for _, text := range []string{
				"平台常见问题",
				"### 7. 无卡模式",
				"关机后以无卡模式启动",
				"四种计费模式",
				"主流大模型已预下载",
			} {
				if strings.Contains(system, text) {
					t.Fatalf("system prompt should not inject static FAQ content %q:\n%s", text, system)
				}
			}
			for _, text := range []string{
				"平台知识类问题必须通过知识库/RAG资料回答",
				"不要凭内置 FAQ 或模型记忆补全平台规则",
			} {
				if !strings.Contains(system, text) {
					t.Fatalf("system prompt should contain knowledge-source boundary %q:\n%s", text, system)
				}
			}
		})
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

func TestBuildSystemWithOptions_CompleteListingRuleInBothModes(t *testing.T) {
	for name, prompt := range map[string]string{
		"mutating":  BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true}),
		"read_only": BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false}),
	} {
		t.Run(name, func(t *testing.T) {
			for _, text := range []string{
				"未显示全",
				"剩余 N 台",
				"还有 X 个",
				"DescribeCompShareInstance",
				"UHostSet",
			} {
				if !strings.Contains(prompt, text) {
					t.Fatalf("system prompt should contain complete-listing rule fragment %q", text)
				}
			}
		})
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
