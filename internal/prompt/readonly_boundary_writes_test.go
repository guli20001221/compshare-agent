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

// The assertion above is one-sided: it checks that no read-only boundary LEAKS into mutating mode,
// and never checks that the lane is still described there. It isn't. The sentence naming the lane
// lives inside the read-only boundary, and that whole section is skipped when mutating tools are on.
// deploy/conf/config.prod.yaml ships mutating_tools: true with ssh_ops.enabled: false, so this is latent
// rather than live — and it fires on the first deployment that enables the lane, i.e. on rollout. The
// fix for the measured "我目前没有…权限，无法替你进实例修复" refusal was wired into read-only mode only.
//
// Measured cost on the enabled-lane shape (mutating_tools: true + allow_writes: true, terra, N=15/arm
// through the real WS path, question = VS Code forwarding fails / plain ssh works / no instance named):
// asked which instance 0/15, offered to go in 0/15. A turn that never asks which instance can never
// enter the lane at all, because UHostId is a required parameter of the tool.
//
// This asserts the lane is named in BOTH modes, and that the mutating-mode copy does not smuggle in
// the read-only claim — in that mode platform writes really are available, so "平台侧操作当前不可
// 执行" would be a false sentence the agent has been measured to act on.
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
