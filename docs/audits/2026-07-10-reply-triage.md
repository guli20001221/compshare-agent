# 用户可见固定回复审计（2026-07-10）

## 基线与范围

- 基线：`origin/main @ a2309e3030d20da04996c33f69e1af6992171892`
- 扫描范围：`internal/engine`、`internal/intent`、`internal/workflow`、`internal/renderer` 下全部 75 个非测试 Go 文件（engine 31、intent 20、workflow 21、renderer 3）。
- 收录标准：字符串必须能追到 `Engine.Chat` 返回值、`HandlerResult.Reply`、可渲染 envelope 字段，或 workflow 的 `Result.Message` / 确认表单说明。字段标签、步骤标题、按钮、枚举值及仅用于拼接的界面状态片段不作为独立回复。每条保留源码中的原始字面量，不合并相同文案。
- 明细：`eval/realism/out/reply_triage.jsonl`。

## 数量

| 分类 | 条数 |
|---|---:|
| `guardrail_refusal` | 147 |
| `dead_end_canned_menu` | 11 |
| `slot_fill_prompt` | 73 |
| `confident_false_negative` | 47 |
| `output_footer` | 25 |
| `post_hoc_rewrite` | 17 |
| `unknown` | 43 |
| **总计** | **363** |

- `pre_llm=true` 且 `terminates_turn=true`：**138** 条。先按真实调用链清除 57 条路由模型之后才可能执行的 CFS、重装、自制镜像、磁盘扩容和网络加速回复，205 降为 148；再剔除两条资源选择数量标签及八条写操作状态拼接半句，最终为 138。
- `is_guardrail=true`：**249** 条，包括 147 条 `guardrail_refusal`、58 条写操作缺参保护、43 条保守标记的 `unknown`，以及 1 条承担事实约束的 post-hoc 改写。
- `reverse_locked_by != ""`：**68** 条。只保留 `Contains` / `Equal` 在引用行正向断言回复或结果消息、且断言字面量确为 `verbatim` 子串的记录；跨模块或一对多引用还逐个运行对应测试并核对覆盖源行。普通 fixture、`NotContains`、以 `Contains` 命中即报错的反向检查、步骤名断言、解析器测试和仅有词元重叠的引用均留空。

`pre_llm` 的 workflow 白名单来自 `lifecycleWorkflowName`：`StopInstanceWorkflow`、`StartInstanceWorkflow`、`RebootInstanceWorkflow`、`ResizeInstanceWorkflow`、`RenameInstanceWorkflow`、`ResetPasswordWorkflow`、`CreateDiskWorkflow`；另有 scheduler 的 `SetStopSchedulerWorkflow`、`CancelStopSchedulerWorkflow`。其余被点名的五类 workflow 只能在 router 已返回后进入。

分类按行为而非否定词判断：写操作 workflow 在执行前因状态缺失或输入无效而 fail closed 的分支，以及写操作目标出现多个候选、必须等待用户选定目标的分支，均归为 `guardrail_refusal`；只读或确定性终端答复中断言资源不存在的条目才保留为 `confident_false_negative`。每条 `reason` 都记录具体源码位置、文案与判断依据，不再按分类复用同一句理由。

## Unknown

43 条 `unknown` 主要来自资源选择后的固定信息回答、部署成功/中止状态模板、库存正常状态行和 workflow 引擎的通用执行错误。这些文本可以到达用户，但不符合六类定义中的菜单、缺参、假否定、尾注、安全拒绝或模型后改写；因此没有强行归类。按“拿不准即保留”的规则，43 条全部设为 `is_guardrail=true`。

## 未纳入与范围外

- `internal/prompt/**` 按任务要求整体排除；`internal/renderer/prompt.go` 和 `internal/renderer/validator.go` 中仅供模型生成/重试的指令也因无法直接到达用户而排除。
- 日志、trace、普通 StepEvent 进度词（如“正在搜索知识库”“调用成功”）按日志/追踪字段排除；workflow 最终错误、确认文案和表单提示仍纳入。
- 表格列名、字段标签、步骤名、确认表单标题、确认/取消按钮、选项状态片段、动态值两侧的半句、枚举值、正则和关键词表排除。此次从原 420 条中移出 57 条此类片段；确认表单中承担完整用户说明的描述仍纳入。
- `internal/refusal/**` 的 jailbreak、off-topic、人工客服和账户账单常量，以及 `internal/httpapi/**` 的确认卡外层格式不在本次四目录范围内；调用点虽在 `internal/engine`，原文字面量不在审计范围内。
- 上游动态 `UserMessage()`、`error.Error()`、API 返回内容和模型自由生成文本不是本地固定字面量，未进入 population。
- 没有跳过范围内文件；无法归类的可达字面量全部保留为 `unknown`，没有静默截断。

## 种子表核对

- `engine.go:2443` 的已选实例菜单存在并已收录；同文案还实际存在于 `resource_selection.go:222` / `:227`，也分别收录，没有合并。
- `correctFalseInstanceNotFoundReply` 本身没有回复字面量；它调用的实际用户文案位于 `renderInstanceFoundCorrection`（`deterministic_targets.go:1004`），已按 `post_hoc_rewrite` 收录。
- `containsInstanceNotFoundClaim` 只是识别模型文本的关键词函数，不产生用户回复，因此未伪造为 population 条目。
- `annotateHandlerResultForUserQuestion` 在该基线函数体只有 `return`，不包含或追加任何回复，因此未收录。
- `monitorTroubleshootingFallbackReply` 未发现；`monitorHistoryNeedSingleInstanceMessage` 只有定义、没有生产调用，均未收录。
- 实际 loop-ceiling 文案存在于 `renderMissingInstanceLoopCeiling`、`renderInstanceLoopCeilingSummary` 和 `renderInstanceListLoopCeilingSummary`，已按真实符号和源码行收录。
- `noImageListNoMatchReply`、`modelRepositoryGuidanceFooter`、`communityImageDeployFooter`、`create_disk.go:45` 均已收录；社区镜像尾注的反向锁定定位到 `internal/intent/routing_registry_test.go:1551`。
