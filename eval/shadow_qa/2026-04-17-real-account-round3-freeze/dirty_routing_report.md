# Real Account Shadow QA Round3 - Dirty Routing

**Summary**: 2/4 PASS

| Case | Result | Notes |
|------|:------:|-------|
| dirtyfinal_01_gpu_colloquial_needs_clarification | PASS | PASS |
| dirtyfinal_02_explicit_name_stop_routes_workflow | PASS | PASS |
| dirtyfinal_03_contextual_just_now_start | FAIL | [step 1] first tool = DescribeCompShareInstance, want StopInstanceWorkflow; [step 2] first tool = DescribeCompShareInstance, want StartInstanceWorkflow |
| dirtyfinal_04_vague_crash_then_specific_failure | FAIL | [step 1] expected no tool call, got ['DiagnoseInitFailure', 'DescribeCompShareInstance'] |

## dirtyfinal_01_gpu_colloquial_needs_clarification

**Result**: PASS

### Step 1: `那个 3080Ti 给我关了`

**Step result**: PASS

**Hook before output**

```text
state=Starting
state=Running
final=Running
```

```text

Assistant> 您当前有2台3080Ti的实例，请确认要关闭哪一台：
1. qa-shadow-20260417-01 (uhost-1paxrg4g1vfw)，当前状态：关机
2. wyp-test-no-delete (uhost-1pabqmq2xn2a)，当前状态：初始化失败

请提供实例ID或名称告诉我您要关闭的目标。

You>
```

## dirtyfinal_02_explicit_name_stop_routes_workflow

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
Stopping
```

```text
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

Assistant> 已成功为您关机实例 **qa-shadow-20260417-01 (uhost-1paxrg4g1vfw)**。

⚠️ 提醒：关机后按量模式下，额外数据盘仍会继续计费。如果您不需要保留该磁盘，请记得在控制台删除。

You>
```

## dirtyfinal_03_contextual_just_now_start

**Result**: FAIL

- [step 1] first tool = DescribeCompShareInstance, want StopInstanceWorkflow
- [step 2] first tool = DescribeCompShareInstance, want StartInstanceWorkflow

### Step 1: `帮我关掉 qa-shadow-20260417-01`

**Step result**: FAIL
- first tool = DescribeCompShareInstance, want StopInstanceWorkflow

**Hook before output**

```text
state=Stopping
state=Stopped
state=Starting
state=Running
final=Running
```

**Hook after output**

```text
Stopping
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

Assistant> 已成功关机实例 `qa-shadow-20260417-01` (ID: uhost-1paxrg4g1vfw)。

⚠️ 提醒：按量计费模式下，关机后额外磁盘仍会继续计费。如需完全停止计费，可删除实例（需在控制台手动操作）。

You>
```

### Step 2: `帮我开一下刚才那个`

**Step result**: FAIL
- first tool = DescribeCompShareInstance, want StartInstanceWorkflow

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

Assistant> 已成功开机实例 `qa-shadow-20260417-01` (ID: uhost-1paxrg4g1vfw)。实例正在启动中，请稍候即可连接使用。

You>
```

## dirtyfinal_04_vague_crash_then_specific_failure

**Result**: FAIL

- [step 1] expected no tool call, got ['DiagnoseInitFailure', 'DescribeCompShareInstance']

### Step 1: `昨晚那台跑崩了`

**Step result**: FAIL
- expected no tool call, got ['DiagnoseInitFailure', 'DescribeCompShareInstance']

```text
  🔧 调用 DiagnoseInitFailure ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/1] check_init_state: concluded

Assistant> 已为您扫描所有实例，发现以下 4 台实例初始化失败：
- `uhost-1pan4ajz9whk` (wyp-test) — 镜像：`cuda130_torch291_py312`
- `uhost-1pampc1jb0mo` (wyptest) — 镜像：`cuda130_torch291_py312`
- `uhost-1paihpariz58` (wyp-test) — 镜像：`cuda130_torch291_py312`
- `uhost-1pabqmq2xn2a` (wyp-test-no-delete) — 镜像：`cuda130_torch291_py312`

**建议**：初始化失败的实例建议删除后重建，可换用官方系统镜像或其他可用镜像。您需要我帮您创建新实例，还是需要进一步排查其他问题？

You>
```

### Step 2: `就是 wyptest 那台`

**Step result**: PASS

```text
  🔧 调用 DiagnoseInitFailure ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/1] check_init_state: concluded

Assistant> 针对 `wyptest` (uhost-1pampc1jb0mo) 的诊断结果：

**结论**：实例初始化失败，使用的镜像为 `cuda130_torch291_py312`。可能原因：
1. 镜像本身异常
2. 资源分配冲突
3. 平台临时故障

**建议**：删除该实例后重新创建，换用官方平台镜像。您需要我帮您创建一台新的 4090 实例吗？可以直接使用官方 PyTorch 镜像。

You>
```
