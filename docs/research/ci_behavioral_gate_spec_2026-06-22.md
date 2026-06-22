# CI 行为门规格（轴A → 可机检硬门）— 2026-06-22

> 接 `eval/realism/golden_two_axis_2026-06-22.md`（②两轴重判）。本文件把②的**轴A 强行为门**落成
> **可机检的 CI 断言规格 + 参考校验器**，并在真实回放数据上验证可计算。
> 这是评估 P0 阶段0 与意图审计 §5 收敛的**安全网**——先有它，改路由/收 taxonomy 才不是无网裸改。
>
> 产物：
> - 规格库 + 校验器：`eval/realism/ci_behavioral_gates.py`
> - 生成的断言集：`eval/realism/ci_behavioral_gates_2026-06-22.jsonl`（187 条断言 / 119 case）
> - 信号源：`http_session_replay.go` 的回放输出 JSONL（per-turn `steps[]` / `confirmations[]` / `reply`）

## 1. 为什么能纯代码校验（信号实测）
回放输出每个 turn 自带完整可观测信号（在真实 220 回放里实测）：
- `steps[].type` ∈ `{tool_call, tool_result, confirm_needed, blocked, error}`
- `steps[].action` = **真实工具/工作流名**：`DescribeCompShareInstance` / `DescribeAvailableCompShareInstanceTypes` / `DiagnosePortOrFirewall` / `DiagnoseGPU` / `DiagnoseInitFailure` / `CreateInstanceWorkflow` / `StartInstanceWorkflow` / `StopInstanceWorkflow` / `CreateDiskWorkflow` …
- `confirmations[].action` = 确认帧工作流身份（新版 harness）；**旧版**确认帧身份落在确认前的 `tool_call` step 上（`confirm_needed.action` 为空）——校验器两种都认。
- `reply` / `final_reply` = 文本（拒答/假取消/重问 的文案门）。

**关键结论：轴A 全部强行为门可直接从回放输出校验，无需 trace JSONL、无需扩展 harness。**（评估前以为 tool-call 不可见的判断已被实测推翻——`steps[]` 363/435 turn 有数据。）

## 2. 断言去耦（不与意图路由改动冲突）
每条门**只断言可观测结果**（调了哪个工具/工作流、是否到确认帧、回复文案），**绝不**断言 `intent_router.intent` / `actual_execution_path` / `bucket`。
→ 阶段2/3 任何路由改动（结构化输出 / few-shot 瘦身 / tier / 意图增删改 / image 6→1）**不会误红**；本门反而是这些改动的回归安全网。唯一该改门的情况=**故意**改某 case 的目标行为时人工带理由更新期望。

## 3. 门 → 检查类型 → 信号（契约）
| 轴A 谓词 | check kind | 信号与通过条件 |
|---|---|---|
| `route_not_create` | forbid_step_action | 全程**无** `tool_call/confirm_needed` step.action ∈ {CreateInstanceWorkflow,…}（裸领域词/纯问答不得进创建 saga） |
| `route_no_misroute` | forbid_step_action | 无误触发的 mutating 工作流（部分=回复语义，标 `reply_semantic`，judge 辅助） |
| `route_to_diagnosis` | require_step_action | 存在 step.action == 对应 `Diagnose*`（端口/GPU/init/billing；按断言关键词定，缺省任一 Diagnose*） |
| `reach_confirm` | require_confirm_action | 某 turn 有 `confirm_needed` **且**该 turn 带对应工作流身份（confirmations[].action / 同 turn 的 `*Workflow` tool_call） |
| `must_confirm_mutating` | forbid_step_action | replay confirm=false→**无任何 raw 执行**(StartCompShareInstance/StopCompShareInstance/…)；出现=确认门被绕过 |
| `read_truth` | require_step_action | 存在 step.action == 对应只读 API（DescribeCompShareInstance / DescribeAvailable… / Get…Price / …Monitor） |
| `no_false_cancel` | forbid_reply_substring | 任何 turn 回复**不含**假取消文案（已取消/取消了/已为你取消/…） |
| `slot_retain` | forbid_reply_substring | 回复**不含**重问选择文案（是哪台实例/请选择实例/…）（reply_semantic，judge 可加强） |
| `state_honest` | judge_assisted | 需 ground-truth fixture / judge（如实反映 Running/Stopped），**不进纯代码硬门** |
| `non_empty` | require_nonempty | final_reply 非空 且 无 terminal error/blocked-空回复（通用底线） |

## 4. 断言集计数（`ci_behavioral_gates_2026-06-22.jsonl`）
187 条 / 119 case。按门：read_truth 45、non_empty 51、state_honest 23、route_to_diagnosis 14、route_no_misroute 13、reach_confirm 13、slot_retain 11、no_false_cancel 9、route_not_create 5、must_confirm_mutating 3。
- **纯代码硬门** = 164 条（除 state_honest 23 judge-assisted）。
- 强行为门（除 non_empty 底线）= 113 条，覆盖②的 98 个 strong-gate case。

## 5. 校验器实证（跑在真实 220 回放）
`python ci_behavioral_gates.py check …http_failure_replay_main_20260616_all.jsonl`：
**PASS 139 / FAIL 25 / SKIP_JUDGE 23（state_honest）。**

⚠️ 220 跑在**陈旧 main `7638fe7`**（非当前 `6b90087`）→ FAIL 用于**验证校验器可计算且抓到真信号**，不是当前 main 的 bug 清单。25 个 FAIL 三类（已与人工 42-fail 名单交叉核对）：
1. **复现已知失败（6 断言/5 case：M018/M022/M119/N018/N033）**——校验器正确抓到人工标注的失败。
2. **行为缺口、内容评审漏掉（多数 19 断言/18 case）**——人工只看内容判 pass/partial，但**可观测动作没做**：如答 SSH 问题却没跑 `DiagnosePortOrFirewall`（M014/M088/M094/M105/M116）、给关机确认却没到确认帧（M036/M040/M047/N004/N006/N041）。**这正是行为轴的价值**——内容看着对、行为错。
3. **启发式 tag 过严，待 curate（少数：M109「平台无开机时间 API，应用 shell 命令答」/M135「可结合」=可选）**——`read_truth` 关键字误打，应在 per-case curation 剪掉。

→ 校验器"verifies intent"（Rule 9）成立：它复现已知失败 + 暴露内容评审看不见的行为缺口。

## 6. 落 CI 的流程（下一实现步）
1. **取输入子集**：从 119 case 的回放（或 `manual_failure_replay_2026-06-16.jsonl`）抽 sid+messages 作 replay 输入（按②/637 补 happy-path 见 #2 路由 eval）。
2. **在当前 main 重跑回放**：`http_session_replay` `-confirm=false` + `COMPSHARE_TRACE_ENABLED=1`（confirm=false 才能测 must_confirm_mutating）。
3. **跑校验器**：任一**纯代码硬门** FAIL → 构建失败；`state_honest`/`route_no_misroute(reply_semantic)`/`slot_retain` 走 judge/人工旁路（warn 不 block，直到 fixture 就位）。
4. **Go 化**（可选，长期）：把 check 逻辑移进 `eval/` Go 测试（读回放 JSONL + 断言集），纳入 `go test ./...`，避免 Python 依赖；当前 Python 参考实现先证可行。

## 7. 待办（per-case curation backlog，不阻塞）
- 剪掉过严 `read_truth` tag（M109/M135 等"可选/无 API"case）。
- `reach_confirm` 目标工作流人工校 13 条（部分 confirm 在与工作流 tool_call 不同 turn，需放宽到 case 级或修目标）。
- `route_no_misroute`/`slot_retain` 的 reply_semantic 子集补 judge rubric 或更精确的 step 断言。
- 期望行为按当前 main 行为基线**重跑一次**生成"绿"基线（区分真 bug vs 陈旧 220）。

## 8. 与整改/审计的接口
- 这是评估报告 **P0#2「CI 无准确率门」** 的行为半边（另一半=#2 路由准确率小标签集，需真模型 eval 臂）。
- 是意图审计 §5（image 6→1、长尾合并、域外出口标准化）的**安全网**：收敛 taxonomy 时这 113 条强行为门不动、自动回归。
- `state_honest` + 轴B `B1_judge` 82 条 = judge 半边，接评估阶段4 judge+kappa。
