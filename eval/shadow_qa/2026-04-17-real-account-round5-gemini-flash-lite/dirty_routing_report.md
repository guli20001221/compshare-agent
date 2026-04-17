# Real Account Shadow QA Round5 - Dirty Routing (Gemini-3.1-Flash-Lite)

**Summary**: 0/4 PASS

| Case | Result | Notes |
|------|:------:|-------|
| dirtymini_01_gpu_colloquial_needs_clarification | FAIL | [step 1] expected no tool call, got ['DescribeCompShareInstance'] |
| dirtymini_02_explicit_name_stop_routes_workflow | FAIL | [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']; [step 1] first tool = DescribeCompShareInstance, want StopInstanceWorkflow |
| dirtymini_03_contextual_just_now_start_accepts_fresh_query | FAIL | [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']; [step 2] expected one of ['StartInstanceWorkflow'], got ['DescribeCompShareInstance'] |
| dirtymini_04_vague_crash_then_specific_failure | FAIL | [step 2] expected one of ['DiagnoseInitFailure'], got ['DescribeCompShareInstance']; [step 2] first tool = DescribeCompShareInstance, want DiagnoseInitFailure |

## dirtymini_01_gpu_colloquial_needs_clarification

**Result**: FAIL

- [step 1] expected no tool call, got ['DescribeCompShareInstance']

### Step 1: `那个 3080Ti 给我关了`

**Step result**: FAIL
- expected no tool call, got ['DescribeCompShareInstance']

**Hook before output**

```text
state=Starting
state=Running
final=Running
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> 已查询到您的实例 **qa-shadow-20260417-01** (3080Ti) 当前处于 **运行中** 状态。

由于您计费方式为 **按量付费**，提醒您：关机后 GPU、CPU 及内存将停止计费，但您的 **磁盘将继续收取费用**。

确认要执行关机操作吗？

You>
```

## dirtymini_02_explicit_name_stop_routes_workflow

**Result**: FAIL

- [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']
- [step 1] first tool = DescribeCompShareInstance, want StopInstanceWorkflow

### Step 1: `帮我关掉 qa-shadow-20260417-01`

**Step result**: FAIL
- expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']
- first tool = DescribeCompShareInstance, want StopInstanceWorkflow

**Hook before output**

```text
state=Running
final=Running
```

**Hook after output**

```text
Running
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> 已为您查询到实例 `qa-shadow-20260417-01` (uhost-1paxrg4g1vfw) 当前处于“运行中”状态。

执行关机操作后，GPU、CPU 和内存将停止计费，但**磁盘费用仍会持续产生**。确认要关机吗？确认后将为您执行。

You>
```

## dirtymini_03_contextual_just_now_start_accepts_fresh_query

**Result**: FAIL

- [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']
- [step 2] expected one of ['StartInstanceWorkflow'], got ['DescribeCompShareInstance']

### Step 1: `帮我关掉 qa-shadow-20260417-01`

**Step result**: FAIL
- expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']

**Hook before output**

```text
state=Starting
state=Running
final=Running
```

**Hook after output**

```text
Running
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> 已查询到实例 **qa-shadow-20260417-01** (uhost-1paxrg4g1vfw) 当前状态为 **运行中**。

请注意：**关机后，实例的 CPU、内存和 GPU 将停止计费，但您额外挂载的云硬盘（如系统盘和数据盘）仍会继续计费。**

确认要执行关机操作吗？

You>
```

### Step 2: `帮我开一下刚才那个`

**Step result**: FAIL
- expected one of ['StartInstanceWorkflow'], got ['DescribeCompShareInstance']

**Hook after output**

```text
Running
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> 您刚才提到的实例 **qa-shadow-20260417-01** (uhost-1paxrg4g1vfw) 目前状态已经是 **运行中 (Running)**，无需再次开机。

请问是否需要对其进行其他操作？

You>
```

## dirtymini_04_vague_crash_then_specific_failure

**Result**: FAIL

- [step 2] expected one of ['DiagnoseInitFailure'], got ['DescribeCompShareInstance']
- [step 2] first tool = DescribeCompShareInstance, want DiagnoseInitFailure

### Step 1: `昨晚那台跑崩了`

**Step result**: PASS

```text

Assistant> 您好，很抱歉听到您的实例运行出现异常。

为了能准确帮您定位并解决问题，能否请您补充一下：

1. **哪台实例？**（您可以提供实例名称或 ID，比如 `qa-shadow-20260417-4090` 等）
2. **具体表现是什么？**（例如：SSH 连不上？nvidia-smi 报错找不着卡？服务启动不起来？还是在执行特定任务时直接崩溃退出了？）

提供这些信息后，我会为您进行针对性的排查。

You>
```

### Step 2: `就是 wyptest 那台`

**Step result**: FAIL
- expected one of ['DiagnoseInitFailure'], got ['DescribeCompShareInstance']
- first tool = DescribeCompShareInstance, want DiagnoseInitFailure

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> 您好，我查询到您的实例列表中有两台名为 **"host"** 的实例，为了确认是哪一台，请核对一下：

1. **uhost-1pbqzm9ry69a**：配置为 4090，当前状态为 **"初始化失败"**。
2. **uhost-1payc1sapvfw**：配置为 3090，当前状态为 **"运行中"**。

由于您提到的 "wyptest" 实例可能已被重命名或与其中一台对应，且其中一台 `uhost-1pbqzm9ry69a` 目前显示“初始化失败”。

**请确认：**
* 您指的是不是那台 **初始化失败的 4090 实例 (uhost-1pbqzm9ry69a)**？
* 除了 "跑崩了"，还有其他具体现象吗（比如刚才提到的初始化失败，还是其他服务报错）？

确认后我会为您调用诊断工具进行深度排查。

You>
```
