# W5 设计文档：评测框架 + 模型对比

> **日期**：2026-04-13
> **状态**：已确认
> **目标**：50 条测试用例 + 6 模型对比 + 意图识别准确率 > 90%

---

## 1. 方案概述

混合评测方案：
- **35 条离线评测**：真实调 LLM，测意图识别 + 工具选择准确率
- **15 条 mock 集成测试**：全 mock，测 engine 边界 case

## 2. 目录结构

```
compshare-agent/
├── eval/
│   ├── cases.json              # 35 条离线评测用例
│   ├── models.go               # 6 个模型配置
│   ├── evaluate_test.go        # 离线评测主逻辑
│   ├── report.go               # Markdown 对比报告生成
│   └── testdata/               # mock API 响应数据（预留）
├── internal/engine/
│   └── scenario_test.go        # 扩展：新增 15 条 mock 集成测试
```

## 3. 运行方式

```bash
# 单模型评测
go test ./eval/ -run TestEval -model "Qwen/Qwen3-Max" -v -timeout 10m

# 全模型对比
go test ./eval/ -run TestEval -model all -v -timeout 30m

# 仅 mock 集成测试
go test ./internal/engine/ -run TestScenario -v
```

## 4. 用例格式

```json
{
  "id": "sq_01",
  "category": "simple_query",
  "input": "我有哪些实例",
  "description": "查询用户实例列表",
  "expected_intent": "simple_query",
  "expected_tools": ["DescribeCompShareInstance"],
  "user_context": "您有 2 个实例（1 个运行中、1 个关机）",
  "tags": ["core", "W1"]
}
```

## 5. 评判标准

### 意图判定规则

| LLM 输出 | 判定意图 |
|----------|---------|
| 调 API tool（Describe/Get/Check） | simple_query |
| 纯文本回复（无 tool_call） | knowledge_qa |
| 调 Workflow meta-tool | complex_task |
| 调 Diagnosis meta-tool | diagnosis |
| 调 Knowledge tool（GetGPUSpecs/GetGPURecommendation） | recommendation |

### 评分公式

```
intent_accuracy = 正确意图数 / 总数
tool_accuracy   = 正确工具数 / (总数 - knowledge_qa 纯文本数)
```

## 6. 评测模型（6 个）

| 显示名 | Model ID | Base URL | 状态 |
|--------|----------|----------|:----:|
| Qwen3-Max | `Qwen/Qwen3-Max` | `https://api.modelverse.cn/v1` | ✅ |
| GLM-5 | `zai-org/glm-5` | `https://api.modelverse.cn/v1` | ✅ |
| Kimi-K2 | `moonshotai/Kimi-K2-Instruct` | `https://api.modelverse.cn/v1` | ✅ |
| GPT-5.4 | `gpt-5.4` | `http://127.0.0.1:8787/v1` | ⏳ 需本地代理 |
| Qwen-Plus | `Qwen/Qwen-Plus` | `https://api.modelverse.cn/v1` | ✅ |
| Qwen3-32B | `Qwen/Qwen3-32B` | `https://api.modelverse.cn/v1` | ✅ |

> 注：glm-5.1、moonshot/kimi-k2.5、deepseek-ai/DeepSeek-V3.2、qwen3.6-plus 当前 API Key 无权限，已替换为授权模型。

API Key 通过环境变量传入：`MODELVERSE_API_KEY`、`LOCAL_PROXY_API_KEY`。

## 7. 用例分布（35 条离线）

| 意图类别 | 条数 | 前缀 |
|----------|:----:|:----:|
| simple_query | 8 | sq_ |
| knowledge_qa | 8 | kq_ |
| complex_task | 7 | ct_ |
| diagnosis | 7 | dg_ |
| recommendation | 5 | rc_ |

## 8. Mock 集成测试（15 条）

### 安全边界（4 条）
1. L2 操作被拒绝（TerminateInstance）
2. L1 操作用户取消（confirmFn=false）
3. L1 操作用户确认（confirmFn=true）
4. 未知 Action 被拦截

### Engine 边界（3 条）
5. Tool 执行返回错误
6. ReAct 最大轮次耗尽（10 轮）
7. FormatToolResult 截断（>4000 字符）

### Workflow 边界（3 条）
8. Workflow 中间步骤失败（库存不足）
9. Workflow 确认步骤取消
10. Workflow 全流程成功（参数变体）

### Diagnosis 边界（2 条）
11. 诊断链提前结束（端口未开放）
12. 诊断链走到兜底

### 上下文与参数（3 条）
13. 多轮对话上下文保持
14. 新用户空实例上下文
15. filterAllowedParams 参数过滤

## 9. 报告格式

```markdown
## Eval Report — 2026-04-13

| Model        | Intent Acc | Tool Acc | Pass | Fail | Time   |
|--------------|:----------:|:--------:|:----:|:----:|-------:|
| Qwen3-Max    |    91.4%   |   88.6%  |  32  |   3  |  42s   |

### Failures
| Case ID | Model | Expected Intent | Got Intent | Expected Tool | Got Tool |
```
