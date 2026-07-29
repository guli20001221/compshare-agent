package prompt

import (
	"strings"
	"testing"
)

// The read-only boundary predates the in-instance repair lane and asserts a blanket "不执行资源变更".
// With the lane authorized that sentence is false, and it is not merely inaccurate — the agent acts
// on it. Measured against a stopped ollama with the lane on and the tool in its window, the reply was
// "我目前没有可直接执行实例内终端命令、修改服务或重启进程的权限，无法替你进实例修复". The prompt
// disabled the feature the operator had turned on, and nothing failed or logged.
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
