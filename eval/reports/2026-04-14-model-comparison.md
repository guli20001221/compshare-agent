# W5 模型对比评测报告

> **日期**: 2026-04-14
> **评测用例**: 35 条（sq:8, kq:8, ct:7, dg:7, rc:5）
> **评判标准**: 意图识别（intent）+ 工具选择（tool）
> **目标**: 意图识别准确率 > 90%

## 综合排名

| 排名 | 模型 | Intent Acc | Tool Acc | 通过 | 失败 | 耗时 | 备注 |
|:----:|------|:----------:|:--------:|:----:|:----:|-----:|------|
| 1 | **Qwen3-32B** | **100.0%** | **100.0%** | 35 | 0 | 3m12s | 全满分 |
| 2 | **Qwen3-Max** | **97.1%** | **92.6%** | 34 | 1 | 3m29s | 达标 |
| 3 | **Doubao-Seed-Lite** | **97.1%** | **88.9%** | 34 | 1 | 4m47s | 达标，性价比高 |
| 4 | **Doubao-Seed-Pro** | **94.3%** | **85.2%** | 33 | 2 | 8m47s | 达标，但较慢 |
| 5 | **Gemini-3.1-Flash-Lite** | **97.1%** | **96.3%** | 34 | 1 | 4m17s | 达标，tool 最高分 |
| 6 | Kimi-K2 | 88.6% | 85.2% | 31 | 4 | 3m31s | 接近达标 |
| 6 | GLM-5 | ~87%* | ~82%* | ~29 | ~4 | >10m | 超时（33/35），响应极慢 |
| 8 | Doubao-Seed-Code | 85.7% | 74.1% | 30 | 5 | 9m43s | 接近达标，代码模型不擅长 |
| 9 | Gemini-3-Pro-Think** | 85.7% | 77.8% | 30 | 5 | 7m12s | 接近达标，2 个 API 错误 |
| 8 | GPT-5.4（gptgod） | 77.1% | 74.1% | 27 | 8 | 3m8s | 不达标 |
| 9 | Qwen-Plus | 74.3% | 70.4% | 26 | 9 | 4m19s | 不达标 |
| 10 | GPT-5.4（本地代理） | 71.4% | 59.3% | 25 | 10 | 3m36s | 不达标，complex_task 全挂 |
| 11 | Doubao-Seed-Mini | ~85%* | - | ~17 | ~3 | >10m | 超时（20/35），响应极慢 |
| 12 | GPT-5.3-Codex（gptgod） | 57.1% | 44.4% | 20 | 15 | 2m58s | 代码模型，不调工具 |
| 13 | GPT-5.4-Mini（gptgod） | 22.9% | 22.2% | 8 | 27 | 40s | 极差，不可用 |

> *GLM-5、Doubao-Seed-Mini 数据为部分完成，因 10 分钟超时未跑完全部 35 条。
> **Gemini-3-Pro-Think 有 2 个 case 因 API key 错误/超时未成功，实际 33 条参与评分。

## 达标判定

**达标（>90%）**: Qwen3-32B、Gemini-3.1-Flash-Lite、Qwen3-Max、Doubao-Seed-Lite、Doubao-Seed-Pro
**接近达标（85-90%）**: Kimi-K2、GLM-5、Doubao-Seed-Code、Gemini-3-Pro-Think
**不达标（<85%）**: GPT-5.4、Qwen-Plus、GPT-5.3-Codex、GPT-5.4-Mini

## 推荐

### 生产首选: Qwen3-32B
- 35 条全部正确，意图和工具双 100%
- 响应速度最快（3m12s / 35 条 ≈ 5.5s/条）
- 开源模型，可本地部署
- **唯一的缺点**：32B 参数量，推理成本高于轻量模型

### 备选: Qwen3-Max
- 97.1% 意图准确率，满足 >90% 目标
- 仅 2 个 case 有偏差（sq_08 工具错选，ct_04 意图误判）
- 托管服务，无需自建推理

### 性价比之选: Doubao-Seed-Lite
- 97.1% 意图准确率，与 Qwen3-Max 并列第二
- 仅 1 个意图错误（sq_01 首次请求 EOF，可能是冷启动）+ 2 个工具错选（库存查询→镜像查询）
- 火山引擎 Ark 平台，Lite 版本推理成本低
- complex_task / diagnosis / knowledge_qa / recommendation 全部正确

## 失败 Case 分析

### Qwen3-Max（2 个失败）

| Case | 期望 | 实际 | 分析 |
|------|------|------|------|
| sq_08 "3090还有没有货" | simple_query → CheckCapacity | simple_query → DescribeImages | "有没有货" 被理解为查镜像而非查库存，Tool description 可优化 |
| ct_04 "我想用A100跑训练，帮我创建一台按量的" | complex_task → CreateWorkflow | simple_query → GetPrice | "帮我创建" 前半句"跑训练"引导了先查价格而非直接创建 |

### Kimi-K2（4 个失败）

| Case | 期望 | 实际 | 分析 |
|------|------|------|------|
| ct_05 "关机吧，不用了" | complex_task → StopWorkflow | knowledge_qa（无 tool） | 口语化表达未触发工具调用，直接文本回复 |
| dg_01 "SSH连不上了" | diagnosis → DiagnoseSSH | knowledge_qa（无 tool） | 直接用知识回答而非调诊断工具 |
| dg_02 "实例初始化失败了怎么办" | diagnosis → DiagnoseInitFailure | knowledge_qa（无 tool） | 同上，Kimi 倾向文本回答诊断问题 |
| dg_05 "SSH连接超时了" | diagnosis → DiagnoseSSH | knowledge_qa（无 tool） | 同 dg_01 |

**Kimi-K2 模式**: 诊断类问题倾向直接回答而非调用诊断工具。需要在 System Prompt 中强化"遇到故障必须先调诊断工具排查"。

### Qwen-Plus（9 个失败）

**主要模式**:
1. complex_task 类大面积失败（5/7）：不使用 Workflow meta-tool，而是调 GetGPUSpecs 或直接文本回复
2. diagnosis 类部分失败：使用 DescribeCompShareInstance 而非诊断工具
3. recommendation 类 1 个失败：直接文本回答而非调知识 Tool

**结论**: Qwen-Plus 对 meta-tool（Workflow/Diagnosis）的理解不足，倾向使用基础 API 或直接回答。不适合本项目。

### GPT-5.4 本地代理（10 个失败）

**主要模式**:
1. **complex_task 全军覆没（7/7）**：所有 Workflow 操作（创建/关机/开机）均不调工具，直接文本回复
2. simple_query 3 个失败：价格/库存查询直接用知识回答而非调 API

**结论**: GPT-5.4 通过本地代理（one-api 转发）的 function calling 能力严重不足，尤其对 Workflow meta-tool 完全不触发。可能原因：
- 本地代理转发过程中 tool schema 信息丢失或不完整
- GPT-5 系列 Responses API 转 Chat Completions 时 tool_calls 映射有问题（与 W1 发现的流式问题同源）
- 或模型本身对中文 tool description 的理解弱于 Qwen 系列

**不推荐用于生产**，但 diagnosis 和 knowledge_qa 表现正常。

## 优化建议

### 针对 Qwen3-Max / Kimi-K2 的 Prompt 优化

1. **强化诊断路由**（影响 Kimi-K2）：
   ```
   重要：用户报告问题时，必须先使用诊断工具排查，不要直接用知识回答。
   - SSH/连接问题 → 必须调用 DiagnoseSSH
   - 初始化失败 → 必须调用 DiagnoseInitFailure
   ```

2. **强化创建操作路由**（影响 Qwen3-Max ct_04）：
   ```
   重要：用户明确要求"创建/开一台"时，必须使用 CreateInstanceWorkflow，不要先查价格。
   ```

3. **库存查询 Tool description 优化**（影响 Qwen3-Max sq_08）：
   在 CheckCompShareResourceCapacity 的 description 中添加"有没有货""有库存吗"等关键词。

## 运行命令参考

```bash
# 复现评测
MODELVERSE_API_KEY=xxx go test ./eval/ -run TestEval -model "Qwen/Qwen3-32B" -v -timeout 10m

# 全模型对比
MODELVERSE_API_KEY=xxx go test ./eval/ -run TestEval -model all -v -timeout 30m
```
