# GPU full spec CLI eval - 2026-05-21

Environment: loaded F:\compshare-agent\eval\shadow_qa\.env.local, config deploy\conf\agent.yaml.example, USE_INTENT_PLANNER_FOR=gpu_specs.

## Overview query

Question: 4090 显存多大

```text
╭──────────────────────────────────────╮
│     优云算力共享 AI 助手 v0.1        │
╰──────────────────────────────────────╯
runtime: planner_mode=off cutover_intents=[gpu_specs]
renderer: grounded_renderer=off
tools: mutating=disabled (read-only mode)

正在初始化，获取您的实例信息...

您可以试试：
  [1] 查看实例状态
  [2] 查看实例监控
  [3] 帮我看看费用

输入 'quit' 或 'exit' 退出。

You>   🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes 调用成功

Assistant> 机型=4090, 性能=83, 显存=24GB, 状态=Normal, 最大卡数=8
机型=4090_48G, 性能=83, 显存=48GB, 状态=Normal, 最大卡数=8

You> 再见！
```
## Single model full specs

Question: 4090 的所有规格

```text
╭──────────────────────────────────────╮
│     优云算力共享 AI 助手 v0.1        │
╰──────────────────────────────────────╯
runtime: planner_mode=off cutover_intents=[gpu_specs]
renderer: grounded_renderer=off
tools: mutating=disabled (read-only mode)

正在初始化，获取您的实例信息...

您可以试试：
  [1] 查看实例状态
  [2] 查看实例监控
  [3] 帮我看看费用

输入 'quit' 或 'exit' 退出。

You>   🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes 调用成功

Assistant> 机型=4090, 可用区=cn-wlcb-01, 性能=83, 显存=24GB, 状态=Normal, 完整配置=1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
机型=4090, 可用区=cn-sh2-02, 性能=83, 显存=24GB, 状态=Normal, 完整配置=1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
机型=4090_48G, 可用区=cn-wlcb-01, 性能=83, 显存=48GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
机型=4090_48G, 可用区=cn-sh2-02, 性能=83, 显存=48GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G

You> 再见！
```

## All GPU full specs

Question: 列出所有 GPU 规格

```text
╭──────────────────────────────────────╮
│     优云算力共享 AI 助手 v0.1        │
╰──────────────────────────────────────╯
runtime: planner_mode=off cutover_intents=[gpu_specs]
renderer: grounded_renderer=off
tools: mutating=disabled (read-only mode)

正在初始化，获取您的实例信息...

您可以试试：
  [1] 查看实例状态
  [2] 查看实例监控
  [3] 帮我看看费用

输入 'quit' 或 'exit' 退出。

You>   🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes 调用成功

Assistant> 机型=4090, 可用区=cn-wlcb-01, 性能=83, 显存=24GB, 状态=Normal, 完整配置=1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
机型=5090, 可用区=cn-sh2-02, 性能=105, 显存=32GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/192G, 4卡/64C/470G, 8卡/124C/940G
机型=4090, 可用区=cn-sh2-02, 性能=83, 显存=24GB, 状态=Normal, 完整配置=1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
机型=5090, 可用区=cn-wlcb-01, 性能=105, 显存=32GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/192G, 4卡/64C/470G, 8卡/124C/940G
机型=4090_48G, 可用区=cn-wlcb-01, 性能=83, 显存=48GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
机型=4090_48G, 可用区=cn-sh2-02, 性能=83, 显存=48GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
机型=3080Ti, 可用区=cn-wlcb-01, 性能=34, 显存=12GB, 状态=Normal, 完整配置=1卡/12C/32G
机型=2080Ti, 可用区=cn-sh2-02, 性能=13, 显存=11GB, 状态=Normal, 完整配置=1卡/4C/24G, 2卡/8C/24G, 4卡/16C/96G, 8卡/32C/192G
机型=3090, 可用区=cn-wlcb-01, 性能=36, 显存=24GB, 状态=Normal, 完整配置=1卡/16C/64G, 2卡/32C/128G, 4卡/64C/256G, 8卡/124C/512G
机型=2080, 可用区=cn-wlcb-01, 性能=10, 显存=8GB, 状态=Normal, 完整配置=1卡/8C/40G, 2卡/20C/80G, 4卡/40C/128G, 8卡/92C/320G
机型=A800, 可用区=cn-wlcb-01, 性能=19, 显存=80GB, 状态=Normal, 完整配置=1卡/16C/240G, 2卡/32C/480G, 4卡/64C/940G, 8卡/124C/1800G
机型=H20, 可用区=cn-wlcb-01, 性能=44, 显存=96GB, 状态=Normal, 完整配置=1卡/16C/240G, 2卡/44C/480G, 4卡/64C/960G, 8卡/188C/1800G
机型=P40, 可用区=cn-wlcb-01, 性能=12, 显存=24GB, 状态=Normal, 完整配置=1卡/8C/64G, 2卡/16C/64G, 4卡/32C/128G, 8卡/44C/440G
机型=V100S, 可用区=cn-wlcb-01, 性能=13, 显存=32GB, 状态=Normal, 完整配置=1卡/10C/64G, 2卡/20C/128G, 4卡/40C/256G, 8卡/92C/512G
机型=A100, 可用区=cn-wlcb-01, 性能=19, 显存=80GB, 状态=Normal, 完整配置=1卡/16C/64G

You> 再见！
```

## CPU and memory wording via capability route

Question: 4090 支持哪些 CPU 和内存

```text
╭──────────────────────────────────────╮
│     优云算力共享 AI 助手 v0.1        │
╰──────────────────────────────────────╯
runtime: planner_mode=off cutover_intents=[gpu_specs]
renderer: grounded_renderer=off
tools: mutating=disabled (read-only mode)

正在初始化，获取您的实例信息...

您可以试试：
  [1] 查看实例状态
  [2] 查看实例监控
  [3] 帮我看看费用

输入 'quit' 或 'exit' 退出。

You>   🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes 调用成功

Assistant> 机型=4090, 可用区=cn-wlcb-01, 性能=83, 显存=24GB, 状态=Normal, 完整配置=1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
机型=4090, 可用区=cn-sh2-02, 性能=83, 显存=24GB, 状态=Normal, 完整配置=1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
机型=4090_48G, 可用区=cn-wlcb-01, 性能=83, 显存=48GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
机型=4090_48G, 可用区=cn-sh2-02, 性能=83, 显存=48GB, 状态=Normal, 完整配置=1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G

You> 再见！
```

## Default model route smoke

Question: 4090 的所有规格

Note: this smoke verifies the default ReAct path can choose the upstream API. The deterministic acceptance path for this PR is the GPU specs capability route above (`USE_INTENT_PLANNER_FOR=gpu_specs`), because it renders the API result directly instead of asking the LLM to summarize the tool payload.

```text
╭──────────────────────────────────────╮
│     优云算力共享 AI 助手 v0.1        │
╰──────────────────────────────────────╯
runtime: planner_mode=off cutover_intents=[]
renderer: grounded_renderer=off
tools: mutating=disabled (read-only mode)

正在初始化，获取您的实例信息...

您可以试试：
  [1] 查看实例状态
  [2] 查看实例监控
  [3] 帮我看看费用

输入 'quit' 或 'exit' 退出。

You>   🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes 调用成功
  🔧 调用 GetGPUSpecs ...
  ✅ GetGPUSpecs 查询成功

Assistant> 以下是平台 **4090（24GB）** 和 **4090_48G（48GB）** 两种型号的全部规格：

---

## 一、RTX 4090（24GB）— 标准版

| 项目 | 详情 |
|------|------|
| **显存** | 24GB GDDR6X |
| **算力** | FP16 约 82.6 TFLOPS |
| **架构** | Ada Lovelace |
| **适用场景** | 推理 / LoRA 微调 / SD/ComfyUI 绘图 / vLLM 部署 |
| **库存状态** | ✅ **Normal（在售）** |
| **可用区** | cn-wlcb-01（乌兰察布）、cn-sh2-02（上海） |

**可选配置（CPU/内存/GPU组合）：**

| GPU 数量 | CPU 核数 | 内存可选 (GB) |
|:--------:|:--------:|:-------------:|
| 1 卡 | 16 核 | 64、94 |
| 2 卡 | 32 核 | 128、192 |
| 4 卡 | 64 核 | 256、384 |
| 8 卡 | 92 核 | 940 |
| 8 卡 | 124 核 | 940 |

---

## 二、RTX 4090_48G（48GB）— 大显存版

| 项目 | 详情 |
|------|------|
| **显存** | 48GB（双卡合并/专用大显存版） |
| **架构** | Ada Lovelace |
| **库存状态** | ✅ **Normal（在售）** |
| **可用区** | cn-wlcb-01（乌兰察布） |

**可选配置（CPU/内存/GPU组合）：**

| GPU 数量 | CPU 核数 | 内存可选 (GB) |
|:--------:|:--------:|:-------------:|
| 1 卡 | 16 核 | 96 |
| 2 卡 | 32 核 | 224 |
| 4 卡 | 64 核 | 470 |
| 8 卡 | 124 核 | 940 |

---

> 💡 **小建议**：如果跑大模型推理或 SD 绘图需要更大显存，推荐 **4090_48G（48GB）**；如果对显存要求不高，**标准 4090（24GB）** 性价比更高。需要我帮您查一下某个具体配置的价格吗？

You> 再见！
```
