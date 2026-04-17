# Real Account Shadow QA Round6 - Dirty Routing (Doubao-Seed-Code)

**Summary**: 1/4 PASS

| Case | Result | Notes |
|------|:------:|-------|
| dirtymini_01_gpu_colloquial_needs_clarification | FAIL | [step 1] rejected tool appeared: StopInstanceWorkflow; [step 1] expected no tool call, got ['DescribeCompShareInstance', 'StopInstanceWorkflow', 'DescribeCompShareInstance']; [step 1] reply missing any of ['哪', '实例', 'uhost-'] |
| dirtymini_02_explicit_name_stop_routes_workflow | FAIL | [step 1] first tool = DescribeCompShareInstance, want StopInstanceWorkflow |
| dirtymini_03_contextual_just_now_start_accepts_fresh_query | PASS | PASS |
| dirtymini_04_vague_crash_then_specific_failure | FAIL | [step 2] expected one of ['DiagnoseInitFailure'], got []; [step 2] first tool = (none), want DiagnoseInitFailure; [step 2] reply missing any of ['初始化', '失败', '删除'] |

## dirtymini_01_gpu_colloquial_needs_clarification

**Result**: FAIL

- [step 1] rejected tool appeared: StopInstanceWorkflow
- [step 1] expected no tool call, got ['DescribeCompShareInstance', 'StopInstanceWorkflow', 'DescribeCompShareInstance']
- [step 1] reply missing any of ['哪', '实例', 'uhost-']

### Step 1: `那个 3080Ti 给我关了`

**Step result**: FAIL
- rejected tool appeared: StopInstanceWorkflow
- expected no tool call, got ['DescribeCompShareInstance', 'StopInstanceWorkflow', 'DescribeCompShareInstance']
- reply missing any of ['哪', '实例', 'uhost-']

**Hook before output**

```text
state=Starting
state=Running
final=Running
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功
  🔧 调用 StopInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [1/3] 查询实例: success
  ⚠️  即将执行变更操作: StopInstanceWorkflow
    参数: {
      "ChargeType": "Dynamic",
      "GPU": 1,
      "GpuType": "3080Ti",
      "Name": "qa-shadow-20260417-01",
      "State": "Running",
      "UHostId": "uhost-1paxrg4g1vfw",
      "warning": "关机后磁盘费用仍会产生，如需彻底停止计费请到控制台释放实例。"
    }
  确认执行？(y/N)   🚫 [2/3] 确认关机: cancelled

Assistant> StopInstanceWorkflow 已取消。

You>
```

## dirtymini_02_explicit_name_stop_routes_workflow

**Result**: FAIL

- [step 1] first tool = DescribeCompShareInstance, want StopInstanceWorkflow

### Step 1: `帮我关掉 qa-shadow-20260417-01`

**Step result**: FAIL
- first tool = DescribeCompShareInstance, want StopInstanceWorkflow

**Hook before output**

```text
state=Starting
state=Running
final=Running
```

**Hook after output**

```text
Stopped
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功
  🔧 调用 StopInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [1/3] 查询实例: success
  ⚠️  即将执行变更操作: StopInstanceWorkflow
    参数: {
      "ChargeType": "Dynamic",
      "GPU": 1,
      "GpuType": "3080Ti",
      "Name": "qa-shadow-20260417-01",
      "State": "Running",
      "UHostId": "uhost-1paxrg4g1vfw",
      "warning": "关机后磁盘费用仍会产生，如需彻底停止计费请到控制台释放实例。"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 StopCompShareInstance ...
  ✅ StopCompShareInstance [3/3] 关机: success

Assistant> 实例 "qa-shadow-20260417-01" (uhost-1paxrg4g1vfw) 已成功关机。

提醒：关机后 GPU/CPU/内存停止计费，但系统盘仍会继续计费。

You>
```

## dirtymini_03_contextual_just_now_start_accepts_fresh_query

**Result**: PASS

### Step 1: `帮我关掉 qa-shadow-20260417-01`

**Step result**: PASS

**Hook before output**

```text
state=Starting
state=Running
final=Running
```

**Hook after output**

```text
Stopped
```

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功
  🔧 调用 StopInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [1/3] 查询实例: success
  ⚠️  即将执行变更操作: StopInstanceWorkflow
    参数: {
      "ChargeType": "Dynamic",
      "GPU": 1,
      "GpuType": "3080Ti",
      "Name": "qa-shadow-20260417-01",
      "State": "Running",
      "UHostId": "uhost-1paxrg4g1vfw",
      "warning": "关机后磁盘费用仍会产生，如需彻底停止计费请到控制台释放实例。"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 StopCompShareInstance ...
  ✅ StopCompShareInstance [3/3] 关机: success

Assistant> 关机操作已完成。实例 qa-shadow-20260417-01 已成功关闭。注意：关机后系统盘仍会继续计费，GPU/CPU/内存将停止计费。

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
      "ChargeType": "Dynamic",
      "GPU": 1,
      "GpuType": "3080Ti",
      "Name": "qa-shadow-20260417-01",
      "State": "Stopped",
      "UHostId": "uhost-1paxrg4g1vfw"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 StartCompShareInstance ...
  ✅ StartCompShareInstance [3/3] 开机: success

Assistant> 开机操作已完成。实例 qa-shadow-20260417-01 正在启动中，预计1-2分钟后可正常使用。

You>
```

## dirtymini_04_vague_crash_then_specific_failure

**Result**: FAIL

- [step 2] expected one of ['DiagnoseInitFailure'], got []
- [step 2] first tool = (none), want DiagnoseInitFailure
- [step 2] reply missing any of ['初始化', '失败', '删除']

### Step 1: `昨晚那台跑崩了`

**Step result**: PASS

```text

Assistant> 您提到的"昨晚那台"具体是哪台实例呢？另外麻烦描述下具体现象：是 SSH 连不上了、nvidia-smi 报错、服务崩了，还是初始化卡住了？

You>
```

### Step 2: `就是 wyptest 那台`

**Step result**: FAIL
- expected one of ['DiagnoseInitFailure'], got []
- first tool = (none), want DiagnoseInitFailure
- reply missing any of ['初始化', '失败', '删除']

```text

Assistant> 您说的是近似实例名的「wyp-test (uhost-1pbs0r67k2am)」吗？麻烦补充描述下**具体现象**哦：
- 是 SSH 连不上了？
- nvidia-smi 报错/GPU 找不到？
- 服务（比如 JupyterLab/自己部署的应用）崩了？
- 还是其他情况？

You>
```
