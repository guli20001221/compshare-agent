# 6 类失败回归 eval 套件

部署前真流量复盘（637+129 session）归纳出 6 类失败。本套件把每类失败固化成
可复跑的确定性 CLI 回归用例，让后续每个修复 **有 gate、不靠手感**。

复用现成 runner `eval/real_cli_golden_runner.py`（驱动 `agent cli`、处理确认门、
md+json 报告）——本套件只新增 cases 文件，不另造 harness。

用例分两层：
- **blocking（确定性）= `regression_6cat_cases.json`**：断言只看路由 / 确认门 /
  诚实文案 / no-misroute 负向串——不依赖答案的具体措辞，可作为修复的硬 gate。
- **advisory（抖动易感）= `regression_6cat_advisory_cases.json`**：知识类**答案质量**
  用例（cat2/4/6），pass 依赖 RAG 检索命中 + agent 不拒答。实测 `cat4_codex_apikey`
  ≈2/3 通过（语料里有、偶发「知识库未覆盖」拒答），是 knowledge_qa **拒答抖动**的
  canary，不作硬 gate；答案保真应归 LLM-judge 层（backlog P2）。

## blocking 用例 → 类映射

| 类 | 用例 id | baseline | gate |
|---|---|---|---|
| 1 创建/关机/变配状态机 | `cat1_create_decline_honest` | green | 拒绝确认 → 诚实「未执行」，绝不假「已取消」(#298) |
| 1 | `cat1_create_named_image_zone` | green（修复前 red） | 点名平台镜像 → 不得甩裸 `RetCode=230`；engine 自动恢复到该区可用镜像（create-image-recovery 修复，N=4 live 全绿）|
| 1 | `cat1_resetpw_clarify_no_target` | green | 无目标的变更类 → 追问，不 P0-abort/不误执行 (RC037) |
| 3 资源不足与库存解释 | `cat3_soldout_followup_no_misroute` | green | 「没库存了怎么办」不误路由到 Coding-Plan/Codex (RC021) |
| 3 | `cat3_stock_ellipsis_referent` | **red** | 省略主语「现在还有库存吗」应复用上轮机型指代，不重列全机型 (RC017，下一个 P1 修复目标) |
| 5 存储/镜像/云盘计费 | `cat5_disk_billing_no_misroute` | green | 「磁盘怎么收费」不误转 GPU 定价 (RC032) |

**baseline=red** 的 `cat3_stock_ellipsis_referent` 是当前 main 上仍失败的下一个修复目标——
修好后翻绿；其余 **green** 是近期已修集群（#298/#300/误路由/create-zone）的回归护栏，
必须保持绿。类 2/4/6 是纯知识答案类，确定性信号有限，移入 advisory（见上）。

## 跑法（生产同款栈）

CLI 子命令 boot 默认即生产栈（agentic SearchKnowledge / knowledge_qa agent-loop /
external knowledge / qwen3_rrf 全 default-on）。只需补 STS 凭据 + mutating-ON +
project_id，再调 runner：

```
python eval/real_cli_golden_runner.py \
  --binary agent.exe --config deploy/conf/agent.yaml \
  --cases eval/regression_6cat_cases.json \
  --out-md <throwaway>.md --title "6cat regression baseline"
```

确认门用例一律 `confirm: n`（拒绝）——不产生任何真实变更；用例从不点名
`勿删/勿删除` 实例，创建类用例新建后也不真正下单（拒绝确认）。

## 断言口径（防 flaky）

- 路由正确性用 `expect_first_tool` / `reject_tool_calls`（确定性强）。
- 诚实文案用精确串：诚实=`未执行`；禁假取消=`已取消`/`操作已取消`；禁裸错误码=`RetCode=230`。
- 知识类答案质量用高召回 `reply_contains_any` 同义词组，避免好答案被串匹配误判红。
  知识类**不**断言 `expect_no_tool_call`——agent-loop 会强制 `SearchKnowledge` 首跳，
  那是预期工具调用，不是误路由。
- 答案**保真/语料覆盖**类（US3CLI 命令、uptime 等纯语料缺口）需 LLM judge，
  不在本确定性套件内（另起 judge 层，对应 backlog P2）。

扩展：新失败模式直接往 `regression_6cat_cases.json` 追用例，沿用同 runner。
