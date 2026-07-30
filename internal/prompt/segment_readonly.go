package prompt

const segmentReadOnlyBoundary = `## 当前只读边界
- 当前工具只允许查询和诊断，不执行资源变更。
- 可以说明用户自行操作的方法，但不要声称已经代为执行。`

// Same boundary, minus the one claim that stops being true when the SSH-ops repair lane is
// authorized. It is deliberately still a read-only boundary: the platform-level write tools really
// are absent, and the exception is narrow — inside ONE instance, after the user approves the card.
// Spelling the exception out is what keeps the agent from generalising "只读" into "我没有权限"
// and refusing the very lane the operator turned on.
const segmentReadOnlyBoundaryWithInstanceRepair = `## 当前只读边界
- 平台侧操作（开关机、重启、创建、扩容、改配置等）当前不可执行，只能查询和诊断；可以说明用户自行操作的方法，但不要声称已经代为执行。
- 唯一例外是 DiagnoseInstanceInternals：用户在授权卡上同意后，你可以进入那一台实例内部执行修复命令并验证结果。用户要求“进去看看”“帮我修”时应当使用它，不要回答自己没有权限。
- 该工具内部仍会拒绝高危操作（删除数据、格式化磁盘、重启关机、改密码或账号、关闭 SSH/网络）；被拒绝的命令作为建议返回给用户，不要绕开。`

// The same exception, for the mode that has no read-only boundary to hang it off.
//
// The lane is authorized by agent.ssh_ops.allow_writes, which is independent of the platform mutating
// gate — but the only sentence naming the lane lived inside the boundary above, and builder skips that
// whole section when mutating tools are on. deploy/conf/config.yaml ships mutating_tools: true with
// ssh_ops.enabled: false, so the gap is not live yet: it fires on the first deployment that turns the
// lane on, which is exactly what shipping it means. The fix for the measured "我目前没有…权限，无法替你
// 进实例修复" refusal would have reached read-only deployments only. The lane's own test asserted that
// no read-only boundary LEAKS into mutating mode and never that the lane is still described there.
//
// Measured A/B, same binary pair apart from this section, same config (mutating_tools: true +
// allow_writes: true), terra, N=15/arm through the real WS path. Question: VS Code forwarding fails /
// plain ssh works / no instance named — the shape real users send, since they mostly do not quote an
// instance ID.
//
//	                              without     with      one-tailed p
//	asks which instance             0/15       4/15        0.0498
//	offers to go into the box       0/15       4/15        0.0498
//	names the server-side cause     6/15       6/15        0.64
//
// Read it honestly: 4/15 is not a fixed feature, and the section does NOT help the model identify the
// cause. What it changes is that the floor was exactly zero — a turn that never asks which instance can
// never enter the lane, because UHostId is required — and 0/15 with a 0/7-vs-0/8 split-half is a floor,
// not a low sample. The larger lever on the same metric is elsewhere and is NOT addressed here: with the
// forced knowledge hop off, the same question asked which instance 14/15 versus 2/15 with it on.
//
// Deliberately duplicates the two operative sentences instead of sharing them with the boundary above:
// they are the exact wording measured to undo that refusal, the read-only copy frames them as
// "唯一例外" (true only where platform writes are absent), and this copy must carry no read-only claim
// at all. A shared const would have to drop the framing that makes each correct in its own mode.
const segmentInstanceRepairLane = `## 实例内修复
- DiagnoseInstanceInternals：用户在授权卡上同意后，你可以进入那一台实例内部执行修复命令并验证结果。用户要求“进去看看”“帮我修”时应当使用它，不要回答自己没有权限。
- 该工具内部仍会拒绝高危操作（删除数据、格式化磁盘、重启关机、改密码或账号、关闭 SSH/网络）；被拒绝的命令作为建议返回给用户，不要绕开。`
