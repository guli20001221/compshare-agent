package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildSystem_WithContext(t *testing.T) {
	ctx := "您有 2 个实例（1 个运行中）"
	result := BuildSystemWithOptions(ctx, BuildOptions{MutatingToolsEnabled: true})

	if !strings.Contains(result, ctx) {
		t.Error("BuildSystem should inject user context into prompt")
	}
	if !strings.Contains(result, "Compshare Copilot") {
		t.Error("BuildSystem should contain product brand (Compshare Copilot)")
	}
	for _, legacy := range []string{"拒答模板", "创作：", "天气、翻译"} {
		if strings.Contains(result, legacy) {
			t.Errorf("BuildSystem should not retain patch-like refusal inventory %q", legacy)
		}
	}
}

func TestBuildSystem_EmptyContext(t *testing.T) {
	result := BuildSystemWithOptions("", BuildOptions{MutatingToolsEnabled: true})
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

func TestBuildSystem_ContainsCentralAgentContract(t *testing.T) {
	prompt := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	for _, text := range []string{
		"本轮唯一的业务判断者",
		"先阅读完整对话",
		"只有用户明确要求实际改变资源",
		"动作建议本身不会执行操作",
		"相同条件没有新信息时不要重复调用",
	} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("system prompt should contain central-agent contract %q", text)
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
		"当前工具只允许查询和诊断",
		"不要声称已经代为执行",
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
				"完整对话、统一上下文或稳定通用知识足以回答时直接回答",
				"无关或空结果不能推翻已有上下文",
			} {
				if !strings.Contains(system, text) {
					t.Fatalf("system prompt should contain knowledge-source boundary %q:\n%s", text, system)
				}
			}
		})
	}
}

func TestKnowledgePolicyDoesNotCompeteWithScopeOrToolDescriptions(t *testing.T) {
	prompt := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	if strings.Contains(prompt, "优先用知识库") {
		t.Fatal("范围说明不得另行决定是否检索")
	}
	for _, required := range []string{
		"稳定通用知识足以回答时直接回答",
		"检索结果只是补充观察",
		"可能的使用场景和排查方向",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("统一检索契约缺少 %q", required)
		}
	}
}

func TestRenderPromptSectionsRejectsDuplicatePolicyID(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate section id must fail prompt construction")
		}
	}()
	renderPromptSections([]PromptSection{
		{ID: "knowledge_turn_policy", Text: "first"},
		{ID: "knowledge_turn_policy", Text: "second"},
	})
}

func TestKnowledgeTurnPolicyAppearsExactlyOnce(t *testing.T) {
	for _, mutating := range []bool{true, false} {
		got := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: mutating})
		if count := strings.Count(got, "## 知识来源与检索规则"); count != 1 {
			t.Fatalf("mutating=%v: knowledge policy count=%d, want 1", mutating, count)
		}
	}
}

func TestBuildSystemWithOptionsAndTraceReportsExactUniqueSectionIDs(t *testing.T) {
	text, ids := BuildSystemWithOptionsAndTrace("sensitive user context", BuildOptions{MutatingToolsEnabled: false})
	if !strings.Contains(text, "sensitive user context") {
		t.Fatal("rendered prompt lost user context")
	}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if strings.Contains(id, "sensitive") {
			t.Fatalf("section metadata leaked prompt content: %q", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate section id in trace metadata: %q", id)
		}
		seen[id] = struct{}{}
	}
	if _, ok := seen["knowledge_turn_policy"]; !ok {
		t.Fatalf("knowledge policy section absent from metadata: %v", ids)
	}
}

func TestBuildSystemWithOptions_MutatingModeDoesNotEmbedWorkflowRoutingTable(t *testing.T) {
	prompt := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	for _, text := range []string{
		"CreateInstanceWorkflow",
		"StopInstanceWorkflow",
		"ResetPasswordWorkflow",
	} {
		if strings.Contains(prompt, text) {
			t.Fatalf("central prompt must not embed workflow routing entry %q", text)
		}
	}
}

func TestBuildSystemWithOptions_UsesOneKnowledgePolicyInBothModes(t *testing.T) {
	for name, prompt := range map[string]string{
		"mutating":  BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true}),
		"read_only": BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false}),
	} {
		t.Run(name, func(t *testing.T) {
			if count := strings.Count(prompt, "## 知识来源与检索规则"); count != 1 {
				t.Fatalf("knowledge policy count=%d, want 1", count)
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

// TestFormatToolResult_ValidJSON was an empty gate: it is named for parseability
// and its comment says "should produce parseable output", but it only asserted
// the ABSENCE of the string "...(truncated)" — it never called json.Unmarshal.
// A byte-cut fragment satisfied it, which is why the hard-cut branch could ship.
// It now asserts the property it is named for.
func TestFormatToolResult_ValidJSON(t *testing.T) {
	// Even large results should produce parseable output
	items := make([]any, 50)
	for i := range items {
		items[i] = strings.Repeat("大", 100) // Chinese chars
	}
	large := map[string]any{"list": items, "count": 50}
	result := FormatToolResult(large)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("FormatToolResult must return parseable JSON, got %v\n%s", err, result)
	}

	// Verify it doesn't cut mid-string
	if strings.Contains(result, "...(truncated)") {
		t.Error("should not use old-style truncation")
	}
}

// TestFormatToolResult_GiantScalarStaysBounded guards the rune cap for inputs
// that array-shrink cannot help: a single huge scalar field. An oversized tool
// result silently masquerades as the per-turn token-budget refusal, which is why
// the cap must hold here, not just for array-heavy results.
//
// Bounded is necessary but was never sufficient — the old code met this bound by
// cutting bytes, which is what produced the invalid JSON in the first place.
// TestFormatToolResult_GiantScalarStaysParseable (tool_result_truncation_test.go)
// is the gate that says what the model actually needs.
func TestFormatToolResult_GiantScalarStaysBounded(t *testing.T) {
	large := map[string]any{"log": strings.Repeat("x", 10000)}
	result := FormatToolResult(large)
	if n := len([]rune(result)); n > 4000 {
		t.Errorf("FormatToolResult must cap at 4000 runes, got %d", n)
	}
}

// TestFormatToolResult_DeepNestedArrayStaysBounded guards the same cap for an
// array buried one level below the top. The shrink used to be one level deep and
// could not reach this at all, so the result fell straight through to the
// byte-cut; truncateArrays now recurses, so the elements are dropped and the
// document stays whole.
func TestFormatToolResult_DeepNestedArrayStaysBounded(t *testing.T) {
	items := make([]any, 100)
	for i := range items {
		items[i] = strings.Repeat("y", 200)
	}
	large := map[string]any{"outer": map[string]any{"items": items}}
	result := FormatToolResult(large)
	if n := len([]rune(result)); n > 4000 {
		t.Errorf("FormatToolResult must cap nested arrays at 4000 runes, got %d", n)
	}
}
