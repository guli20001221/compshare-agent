package prompt

import (
	"strings"
	"testing"
)

func TestBuildSystemContext(t *testing.T) {
	ctx := "您有 2 个实例（1 个运行中）"
	got := BuildSystemWithOptions(ctx, BuildOptions{MutatingToolsEnabled: true})
	if !strings.Contains(got, ctx) || !strings.Contains(got, "Compshare Copilot") {
		t.Fatalf("prompt lost identity or user context: %q", got)
	}

	empty := BuildSystemWithOptions("", BuildOptions{MutatingToolsEnabled: true})
	if !strings.Contains(empty, "暂无用户信息") {
		t.Fatal("empty context should use the first-turn placeholder")
	}
}

func TestFormatInstanceContextEmpty(t *testing.T) {
	for _, input := range []map[string]any{{}, {"UHostSet": nil}} {
		if got := FormatInstanceContext(input); got != "用户当前没有实例。" {
			t.Fatalf("empty instance context = %q", got)
		}
	}
}

func TestFormatInstanceContextWithInstances(t *testing.T) {
	apiResult := map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-abc", "Name": "my-gpu", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Postpay"},
		map[string]any{"UHostId": "uhost-def", "Name": "test", "State": "Stopped", "GpuType": "3080Ti", "GPU": float64(1), "ChargeType": "Month"},
	}}
	got := FormatInstanceContext(apiResult)
	for _, want := range []string{"2 个实例", "1 个运行中", "uhost-abc", "运行中"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instance context missing %q: %s", want, got)
		}
	}
}

func TestTranslateState(t *testing.T) {
	for input, want := range map[string]string{
		"Running": "运行中", "Stopped": "关机", "Starting": "启动中",
		"Install": "初始化中", "Install Fail": "初始化失败", "UnknownState": "UnknownState",
	} {
		if got := translateState(input); got != want {
			t.Errorf("translateState(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestReadOnlyPromptStatesTheActualBoundary(t *testing.T) {
	got := BuildSystemWithOptions("context", BuildOptions{MutatingToolsEnabled: false})
	for _, forbidden := range []string{"CreateInstanceWorkflow", "StopInstanceWorkflow", "sudo apt", "ollama serve"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("read-only prompt contains mutating guidance %q", forbidden)
		}
	}
	for _, want := range []string{"当前工具只允许查询和诊断", "不要声称已经代为执行"} {
		if !strings.Contains(got, want) {
			t.Fatalf("read-only prompt missing %q", want)
		}
	}
}

func TestPromptDoesNotEmbedStaticFAQ(t *testing.T) {
	for _, mutating := range []bool{true, false} {
		got := BuildSystemWithOptions("context", BuildOptions{MutatingToolsEnabled: mutating})
		for _, stale := range []string{"平台常见问题", "### 7. 无卡模式", "四种计费模式", "主流大模型已预下载"} {
			if strings.Contains(got, stale) {
				t.Fatalf("mutating=%v: prompt embeds FAQ text %q", mutating, stale)
			}
		}
		if strings.Count(got, "## 知识来源与检索规则") != 1 {
			t.Fatalf("mutating=%v: knowledge policy must appear once", mutating)
		}
	}
}

func TestRenderPromptSectionsRejectsDuplicateID(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate section id must fail prompt construction")
		}
	}()
	renderPromptSections([]PromptSection{{ID: "policy", Text: "first"}, {ID: "policy", Text: "second"}})
}

func TestPromptTraceReportsUniqueSectionIDsWithoutContent(t *testing.T) {
	text, ids := BuildSystemWithOptionsAndTrace("sensitive user context", BuildOptions{MutatingToolsEnabled: false})
	if !strings.Contains(text, "sensitive user context") {
		t.Fatal("rendered prompt lost user context")
	}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if strings.Contains(id, "sensitive") {
			t.Fatalf("section metadata leaked prompt content: %q", id)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate prompt section id: %q", id)
		}
		seen[id] = struct{}{}
	}
}
