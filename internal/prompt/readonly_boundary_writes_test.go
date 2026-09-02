package prompt

import (
	"strings"
	"testing"
)

// A read-only deployment cannot expose the autonomous repair lane even if a
// caller accidentally sets the secondary flag.
func TestReadOnlyBoundaryNeverAdvertisesTheInstanceRepairLane(t *testing.T) {
	plain := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: false})
	withLane := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: false, InstanceOpsEnabled: true})

	if !strings.Contains(plain, "不执行资源变更") {
		t.Fatal("plain read-only boundary lost its blanket claim")
	}
	if withLane != plain {
		t.Fatal("InstanceOpsEnabled must not weaken a read-only prompt without the deployment write grant")
	}

	// With platform writes on there is no read-only boundary at all; the lane flag must not add one.
	both := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: true, InstanceOpsEnabled: true})
	if strings.Contains(both, "当前只读边界") {
		t.Fatal("mutating tools enabled but a read-only boundary was still injected")
	}
}

// Mutating mode omits the read-only section, but must still describe the independently authorized
// repair lane without falsely claiming that platform writes are unavailable.
func TestInstanceRepairLaneIsNamedWhenMutatingToolsAreOn(t *testing.T) {
	both := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: true, InstanceOpsEnabled: true})

	if !strings.Contains(both, "DiagnoseInstanceInternals") {
		t.Fatal("lane authorized with mutating tools on, but no prompt section names it")
	}
	if !strings.Contains(both, "不要回答自己没有权限") {
		t.Fatal("the mutating-mode copy must answer the observed refusal directly, like the read-only one")
	}
	if !strings.Contains(both, "不弹实例内授权卡") || !strings.Contains(both, "不要逐命令") {
		t.Fatal("mutating mode must expose the card-free autonomous repair contract")
	}
	if !strings.Contains(both, "下载") || !strings.Contains(both, "不要只给手工命令") {
		t.Fatal("the lane must cover explicit guest-local operations instead of handing shell commands back to the user")
	}
	if !strings.Contains(both, "文件、目录、日志、进程") ||
		!strings.Contains(both, "不得用公共模型/镜像目录或知识库替代实例内观察") {
		t.Fatal("guest-state reads must route to the instance lane instead of a public catalog or a hand-written command")
	}
	if !strings.Contains(both, "同一会话") || !strings.Contains(both, "不因时间间隔失效") {
		t.Fatal("a long pause must not revoke the conversation's user-selected SSH target")
	}
	if !strings.Contains(both, "高危操作") {
		t.Fatal("the destructive refusals must stay named, or the agent plans around commands the harness rejects")
	}
	if strings.Contains(both, "平台侧操作") {
		t.Fatal("mutating tools are ON; claiming platform writes are unavailable is the same class of false sentence")
	}

	// The lane flag is what gates it. With writes off, the tool description alone describes the lane
	// and the prompt must not promise repair the harness will refuse.
	noLane := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: true})
	if strings.Contains(noLane, "DiagnoseInstanceInternals") {
		t.Fatal("lane not authorized but the prompt still promises in-instance repair")
	}
}

func TestInstanceRepairLanePreservesUserLimitsWithoutSelectingAMode(t *testing.T) {
	system := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: true, InstanceOpsEnabled: true})
	if !strings.Contains(system, "自主执行与目标直接相关的可恢复修复并验证") ||
		!strings.Contains(system, "遵守完整用户请求") ||
		!strings.Contains(system, "明确只检查、不修改时仅观察") {
		t.Fatal("one instance lane must carry autonomous repair and the user's inspection-only constraint")
	}
	for _, obsolete := range []string{"Mode=", "repair_scope_authorized", "inspection_scope", "运行时会拒绝写入"} {
		if strings.Contains(system, obsolete) {
			t.Fatalf("assembled system prompt still advertises an obsolete instance authorization gate: %q", obsolete)
		}
	}
}
