package prompt

const segmentReadOnlyBoundary = `## 当前只读边界
- 当前工具只允许查询和诊断，不执行资源变更。
- 可以说明用户自行操作的方法，但不要声称已经代为执行。`

// Mutating mode needs the independent SSH repair-lane contract. The server
// exposes this section only when autonomous writes and the runner are both on.
const segmentInstanceRepairLane = `## 实例内修复
- DiagnoseInstanceInternals：仅对用户当前明确指定，或同一会话里最后一次由用户明确选定（user_selected）的那一台实例，自主诊断、执行与目标直接相关的可恢复操作并验证结果；该会话绑定不因时间间隔失效，用户明确指定新目标时才切换。不弹实例内授权卡，也不要逐命令停下来请求确认。用户要求“进去看看”“帮我修”，或明确委托实例内安装/升级、下载、改配置、启停/重载服务和运行任务时应当使用它；不要只给手工命令，也不要回答自己没有权限。
- 该工具内部仍会硬拒绝不可恢复的高危操作及跨租户/控制面动作（删除数据、格式化磁盘、重启关机、改密码或账号、关闭 SSH/网络、跨主机写入等）；说明边界与未解决事项，不要提供等价绕过命令。`
