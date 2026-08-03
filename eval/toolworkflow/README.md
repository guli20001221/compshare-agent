# P3 工具窗口回放

该清单固定了 53 个 2026-06-26 至 2026-07-24 的生产 `agent_traces.tool_calls` 事件。原始导出位于被 `.gitignore` 保护的 `eval/reports/`，绝不提交。

每行只保存 `SHA-256(export | trace id | request uuid | turn index)`、导出窗口和动作族；不保存用户文本、租户、实例 ID、请求参数或工具结果。动作族及数量为：平台镜像目录 26、社区目录 15、自制镜像目录 6、自制镜像创建 3、共享镜像可见性 3。

`internal/engine/tool_window_scope_replay_test.go` 将这些历史执行动作族回放到 P3 的确定性工具窗口计划器。它验证的是控制面行为，不伪称可重放历史 LLM token；原始用户文本不进入仓库或测试日志。目录查询一律保持完整窗口：一次浏览不代表用户已经选择该镜像来源，推荐仍可联查平台与社区目录。

P3 只在同一 ReAct turn 内、Agent 已调用实际可见的 `Request<Workflow>` 后收窄后续工具：所有读工具保留，只留下该工作流对应的唯一写提案。每个新用户 turn 都恢复完整窗口；`ContextFrame` 不是 UI 卡片续接凭证，真实确认卡在原 durable turn 内处理，因此不会被 P3 改写。若以后需要跨 turn 的卡片收窄，必须另行引入服务端签发、绑定 owner/session/expiry 的 continuation token。完成 trace 的 `tool_scope_phase=last_outbound_agent_tool_window` 明确表示实际发给主 Agent 的最后一个工具窗口；没有主 Agent 工具窗口时为 `no_model_window`，不会把仅计划而未发送的范围误记为暴露面。

上游的跨可用区同步动作是 `SyncCompShareCustomImage`，并不存在“社区镜像同步”动作。社区目录仍可接续现有的创建/重装工作流；共享目录列出被共享的自制镜像，当前项目只允许其进入重装（`ReinstallInstanceWorkflow` 接受 `shared`/`sharing`），不宣称可用于现有的创建实例工作流。
