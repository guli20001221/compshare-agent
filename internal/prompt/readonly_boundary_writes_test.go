package prompt

import (
	"strings"
	"testing"
)

// An authorized in-instance repair lane is the narrow exception to platform read-only mode.
func TestReadOnlyBoundaryYieldsToTheInstanceRepairLane(t *testing.T) {
	plain := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: false})
	withLane := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: false, InstanceOpsWritesEnabled: true})

	if !strings.Contains(plain, "不执行资源变更") {
		t.Fatal("plain read-only boundary lost its blanket claim")
	}
	if strings.Contains(withLane, "当前工具只允许查询和诊断，不执行资源变更") {
		t.Fatal("lane authorized but the prompt still tells the agent it can change nothing")
	}
	if !strings.Contains(withLane, "DiagnoseInstanceInternals") {
		t.Fatal("lane authorized but the boundary never names the exception")
	}
	if !strings.Contains(withLane, "不要回答自己没有权限") {
		t.Fatal("the boundary must answer the observed failure directly, not just soften the wording")
	}
	// The exception is narrow: platform writes stay unavailable, and the hard refusals stay named,
	// or the agent plans around commands the harness will reject and burns the turn.
	if !strings.Contains(withLane, "平台侧操作") {
		t.Fatal("platform-level writes must still be described as unavailable")
	}
	if !strings.Contains(withLane, "高危操作") {
		t.Fatal("the destructive refusals must stay named in the prompt")
	}

	// With platform writes on there is no read-only boundary at all; the lane flag must not add one.
	both := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: true, InstanceOpsWritesEnabled: true})
	if strings.Contains(both, "当前只读边界") {
		t.Fatal("mutating tools enabled but a read-only boundary was still injected")
	}
}

// Mutating mode omits the read-only section, but must still describe the independently authorized
// repair lane without falsely claiming that platform writes are unavailable.
func TestInstanceRepairLaneIsNamedWhenMutatingToolsAreOn(t *testing.T) {
	both := BuildSystemWithOptions("ctx", BuildOptions{MutatingToolsEnabled: true, InstanceOpsWritesEnabled: true})

	if !strings.Contains(both, "DiagnoseInstanceInternals") {
		t.Fatal("lane authorized with mutating tools on, but no prompt section names it")
	}
	if !strings.Contains(both, "不要回答自己没有权限") {
		t.Fatal("the mutating-mode copy must answer the observed refusal directly, like the read-only one")
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
