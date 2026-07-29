package tools

import "sync/atomic"

// The in-instance ops lane ships read-only. When agent.ssh_ops.allow_writes is on, the SAME tool
// also repairs the box — and every sentence the user reads about it has to change with it. This is
// the single boot-time switch those sentences hang off, because they live in three packages
// (this registry's tool description, httpapi's step/consent label, engine's progress lines) and a
// half-flipped set is worse than either mode: a card that says 只读排查 while the harness writes is
// consent we did not obtain, and a description that promises repair while the harness refuses is a
// model that keeps proposing fixes the lane will reject.
//
// Boot-only by construction: cmd sets it once, before any session exists, from the same config
// field that authorizes the harness. Nothing re-reads config per turn, so there is no per-session
// state here to leak between tenants.
var instanceOpsWritesEnabled atomic.Bool

// InstanceOpsWritesEnabled reports whether the lane may execute the mutating tier. Read it wherever
// user-visible wording depends on the mode; it is NOT an authorization check (the harness's own
// handshake gate is), so never use it to decide whether something may run.
func InstanceOpsWritesEnabled() bool { return instanceOpsWritesEnabled.Load() }

// SetInstanceOpsWritesEnabled is called once at boot by cmd. Tests may set it, but must restore it.
func SetInstanceOpsWritesEnabled(v bool) { instanceOpsWritesEnabled.Store(v) }

// Two descriptions, one tool. The trigger half (when to reach for it) is deliberately identical:
// the symptoms that need someone inside the box do not change with the mode. Only the promise about
// what happens after the user authorizes changes, because that is the part the model plans around —
// told it may repair, it gathers evidence and then acts; told it may not, it stops at the verdict.
const (
	instanceOpsTriggerDesc = "登录到指定实例内部排查问题。适用于根因在实例内部、平台 API 看不到的故障：" +
		"GPU 掉卡 / nvidia-smi 报错 / CUDA 找不到设备、显存被占满、服务或端口起不来（ComfyUI、Jupyter、vLLM 等）、" +
		"磁盘写满、数据盘未挂载、Python 环境与依赖异常、进程卡死或负载异常。执行前会请用户在卡片上授权；"

	instanceOpsReadOnlyDesc = instanceOpsTriggerDesc +
		"只执行只读命令，任何会修改环境的操作都会被拒绝，修复步骤仅作为建议返回。"

	instanceOpsWriteDesc = instanceOpsTriggerDesc +
		"用户授权后可以直接执行修复命令并验证结果；删除数据、格式化磁盘、重启/关机、改密码或账号、" +
		"关闭 SSH/网络这类高危操作始终会被拒绝，遇到被拒绝的命令请作为建议返回，不要绕开。"

	instanceOpsNotForDesc = "不用于：SSH 连不上或登录失败、费用与计费（用 DiagnoseBilling）、" +
		"平台侧安全组与端口开放（不在实例内）、不针对具体实例的通用知识问题。"
)

// InstanceOpsDescription is the tool description for the current mode. Registry holds the read-only
// text as its literal so an un-configured binary reads correctly on inspection; dispatch_window
// substitutes this when building the model's window, which is the only copy the model ever sees.
func InstanceOpsDescription() string {
	if InstanceOpsWritesEnabled() {
		return instanceOpsWriteDesc + instanceOpsNotForDesc
	}
	return instanceOpsReadOnlyDesc + instanceOpsNotForDesc
}
