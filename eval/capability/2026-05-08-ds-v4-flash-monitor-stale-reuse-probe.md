# ds v4 flash monitor stale-reuse probe

- Date: 2026-05-08
- Base URL: `https://api.modelverse.cn/v1`
- Model: `deepseek-v4-flash`
- Method: real `engine.Chat()` two-turn conversations after `Engine.Init()`, with no object `tool_choice` on ds v4 flash because `SupportsObjectToolChoice=false`.
- Target: `<target>` (State=Running, GPUType=V100S, GPU=1)

## Conclusion

`deepseek-v4-flash` auto routing does **not** reliably refresh adjacent monitor follow-ups. In 2/3 cases the second turn did not call `GetCompShareInstanceMonitor` and still returned concrete GPU/VRAM monitor values from the previous turn. The only fresh recall happened when the user explicitly said "重新查...不要复用上一轮".

Implication: `shouldForceMonitorRecall` must not be deleted yet, but object `tool_choice` cannot be used on ds v4 flash. The next mitigation should be a non-object-tool-choice strategy, such as a narrow dynamic system/developer nudge for adjacent monitor follow-ups, then re-run this probe.

## Decision table

| Case | First monitor calls | Second monitor calls | Classification | First actions | Second actions |
|---|---:|---:|---|---|---|
| `adjacent_same_metric` | 1 | 0 | `STALE_REUSE_RISK` | `GetCompShareInstanceMonitor` | `` |
| `adjacent_explicit_refresh` | 1 | 1 | `FRESH_RECALLED` | `GetCompShareInstanceMonitor` | `GetCompShareInstanceMonitor` |
| `adjacent_pronoun_now` | 1 | 0 | `STALE_REUSE_RISK` | `GetCompShareInstanceMonitor` | `DescribeCompShareInstance, GetGPUSpecs` |

## Case details

### adjacent_same_metric

- First actions: `GetCompShareInstanceMonitor`
- Second actions: ``
- Classification: `STALE_REUSE_RISK`

First reply:

```text
以下是 **`<target>`（<target>）** 的当前资源使用情况：

| 指标 | 使用率 |
|------|:------:|
| 🖥 **CPU** | **0%**（空闲） |
| 💾 **内存** | **4%** |
| 🎮 **GPU 利用率** | **0%**（空闲） |
| 📊 **显存使用率** | **0%**（空闲） |
| 💿 **系统盘（/）** | **29%** |
| 📁 **数据盘（/data）** | **0%** |
| 📁 **数据盘（/mnt/cloud）** | **0%** |

这台 **V100S×1** 实例目前基本处于闲置状态，CPU、GPU、显存和内存资源都几乎没有被占用，说明当前没有运行负载任务。如果需要用它跑训练或推理，随时可以开始使用。
```

Second reply:

```text
刚才的数据已经查到了，**`<target>`（<target> | V100S×1）** 的 GPU 情况如下：

| 指标 | 数值 |
|------|:----:|
| 🎮 **GPU 利用率** | **0%**（完全空闲） |
| 📊 **显存使用率** | **0%**（完全空闲） |

GPU 和显存目前都没有任何负载，机器处于空闲状态。
```

### adjacent_explicit_refresh

- First actions: `GetCompShareInstanceMonitor`
- Second actions: `GetCompShareInstanceMonitor`
- Classification: `FRESH_RECALLED`

First reply:

```text
以下是实例 **<target> (<target>)**（V100S×1）的当前资源利用情况：

| 指标 | 使用率 |
|------|:------:|
| **CPU 使用率** | **0%** |
| **内存使用率** | **4%** |
| **GPU 使用率** | **0%** |
| **GPU 显存使用率** | **0%** |
| **系统盘（/）** | **29%** |
| **数据盘（/data）** | **0%** |

这台实例目前基本处于**空闲状态**，几乎没有任何负载。CPU、GPU 和显存使用率均为 0，内存也只用了 4%。如果是旧 controller 不再使用，可以考虑关机节省费用（关机后 GPU/CPU/内存停止计费，但额外磁盘仍会收费）。需要我帮你关机吗？
```

Second reply:

```text
当前最新数据如下（**<target>**，V100S×1）：

| 指标 | 最新值 |
|:------|:------:|
| **GPU 使用率** | **0%** |
| **GPU 显存使用率** | **0%** |

和上一轮结果一致，这台机器目前 GPU 完全没有负载，处于空闲状态。如果暂时不需要用，可以考虑关机节省费用（按量计费模式下关机后 GPU/CPU/内存停止计费）。
```

### adjacent_pronoun_now

- First actions: `GetCompShareInstanceMonitor`
- Second actions: `DescribeCompShareInstance, GetGPUSpecs`
- Classification: `STALE_REUSE_RISK`

First reply:

```text
以下是实例 **<target>**（`<target>`，V100S×1）的实时资源使用情况：

| 资源指标 | 使用率 |
|---------|:-----:|
| 💻 **CPU** | **0%**（空闲） |
| 🧠 **内存** | **4%** |
| 🎮 **GPU** | **0%**（空闲） |
| 📊 **显存** | **0%**（空闲） |
| 💾 **系统盘 /** | **29%** |
| 💾 **数据盘 /data** | **0%** |
| 💾 **数据盘 /mnt/cloud** | **0%** |

这台实例目前基本处于空闲状态，CPU、GPU 和显存都没有负载，资源利用率很低。如果你有用它跑任务的需求，可以直接使用；如果只是挂着闲置，可以考虑关机节省成本（按量计费关机后 GPU/CPU/内存停止计费）。需要我帮你做什么操作吗？
```

Second reply:

```text
这台 **<target>** 的配置如下：

| 项目 | 规格 |
|------|:----:|
| 🎮 **GPU** | **Tesla V100S × 1** |
| 📊 **显存** | **32 GB** |
| 💻 **CPU** | **10 核** |
| 🧠 **内存** | **64 GB** |

再加上刚才的监控数据——GPU 利用率和显存占用都是 **0%**，说明目前没有任何任务在跑，资源非常充裕。需要跑点什么任务吗？
```

## Interpretation

- Second-turn monitor recalls: 1/3
- Stale-reuse risk cases: 2/3
- If `Second-turn monitor recalls` is 0 and replies contain concrete monitor values, ds v4 flash auto routing is reusing stale context and needs a non-object-tool-choice mitigation.
