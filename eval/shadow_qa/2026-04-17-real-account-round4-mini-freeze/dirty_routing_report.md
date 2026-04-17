# Real Account Shadow QA Round4 - Dirty Routing (Doubao-Seed-Mini)

**Summary**: 0/4 PASS

| Case | Result | Notes |
|------|:------:|-------|
| dirtymini_01_gpu_colloquial_needs_clarification | FAIL | [step 1] expected no tool call, got ['DescribeCompShareInstance'] |
| dirtymini_02_explicit_name_stop_routes_workflow | FAIL | [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']; [step 1] first tool = DescribeCompShareInstance, want StopInstanceWorkflow |
| dirtymini_03_contextual_just_now_start_accepts_fresh_query | FAIL | [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance'] |
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

Assistant> 请确认您要关闭以下实例：
| 实例ID | 实例名称 | GPU配置 | 计费方式 |
| ---- | ---- | ---- | ---- |
| uhost-1paxrg4g1vfw | qa-shadow-20260417-01 | 3080Ti×1 | Dynamic |

注意：关机后GPU/CPU/内存将停止计费，但额外磁盘仍会继续收费。
是否确认执行关机操作？

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

Assistant> 您要操作的实例 `qa-shadow-20260417-01`（UHostId: uhost-1pafu1vekpoe）当前状态已经是 **Stopped（已关机）**，无需重复执行关机操作。

You>
```

## dirtymini_03_contextual_just_now_start_accepts_fresh_query

**Result**: FAIL

- [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']

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

Assistant> 您要操作的实例`qa-shadow-20260417-01`（UHostId: uhost-1pafu1vekpoe）当前状态已经是**Stopped（已关机）**，无需执行关机操作。

You>
```

### Step 2: `帮我开一下刚才那个`

**Step result**: PASS

**Hook after output**

```text
Running
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功
  🔧 调用 StartInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [1/3] 查询实例: success
  ⚠️  即将执行变更操作: StartInstanceWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "qa-shadow-20260416-01",
      "State": "Stopped",
      "UHostId": "uhost-1pafu1vekpoe"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 StartCompShareInstance ...
  ❌ StartCompShareInstance: [3/3] 开机: failed

Assistant> 很抱歉，开机操作失败了。错误码 `8357` 表示当前可用区（`cn-wlcb-01`）的 **RTX4090** 资源暂时售罄，无法启动该实例。

您可以稍后再尝试开机，或者如果需要立即使用GPU算力，可以考虑更换其他GPU型号的实例（如3090、A100等），如需更换型号可先帮您查询对应库存~

You>
```

## dirtymini_04_vague_crash_then_specific_failure

**Result**: FAIL

- [step 2] expected one of ['DiagnoseInitFailure'], got ['DescribeCompShareInstance']
- [step 2] first tool = DescribeCompShareInstance, want DiagnoseInitFailure

### Step 1: `昨晚那台跑崩了`

**Step result**: PASS

```text

Assistant> 您提到的“跑崩了”描述比较模糊，请您告知具体是哪台实例（可以提供实例ID或唯一名称），以及具体的故障现象（例如SSH连接失败、nvidia-smi报错、服务无法访问、初始化卡住等），以便我帮您排查问题。

You>
```

### Step 2: `就是 wyptest 那台`

**Step result**: FAIL
- expected one of ['DiagnoseInitFailure'], got ['DescribeCompShareInstance']
- first tool = DescribeCompShareInstance, want DiagnoseInitFailure

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> 在当前的实例列表中未找到名称为`wyptest`的实例，请您确认实例的正确名称或提供实例ID（例如`uhost-xxx`格式），我将为您进一步排查故障。

当前您的实例列表如下：
1.  `host`（uhost-1pbqzm9ry69a）：状态=初始化失败
2.  `host`（uhost-1payc1sapvfw）：GPU=3090×1，状态=运行中
3.  `qa-shadow-20260417-4090`（uhost-1payastkiw8o）：GPU=4090×1，状态=运行中
4.  `qa-shadow-20260417-01`（uhost-1paxrg4g1vfw）：GPU=3080Ti×1，状态=运行中
5.  `勿删除`（uhost-1paieu38kerc）：GPU=4090×1，状态=运行中
6.  `qa-shadow-20260416-01`（uhost-1pafu1vekpoe）：GPU=4090×1，状态=关机
7.  `test-3090`（uhost-1pa8zsdq560o）：GPU=3090×1，状态=运行中
8.  `内网ping勿删`（uhost-1ozccvmyd3ia）：GPU=0，状态=运行中
9.  `内网ping勿删除`（uhost-1oz238rtbtim）：GPU=0，状态=运行中

You>
```
