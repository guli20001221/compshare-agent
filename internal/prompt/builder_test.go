package prompt

import (
	"strings"
	"testing"
)

func renderPromptSectionsOnly(sections []PromptSection) string {
	text, _ := renderPromptSectionsWithIDs(sections)
	return text
}

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
	renderPromptSectionsOnly([]PromptSection{{ID: "policy", Text: "first"}, {ID: "policy", Text: "second"}})
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
