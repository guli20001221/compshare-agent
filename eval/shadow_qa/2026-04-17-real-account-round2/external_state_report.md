# Real Account Shadow QA Round2 - External State

**Summary**: 2/3 PASS (initial run, 2026-04-17)  
**Follow-up**: ext_03 re-run after billing-stale hard-guard fix — **PASS**.
See [ext_03_rerun_report.md](./ext_03_rerun_report.md).

| Case | Initial | Post-fix | Notes |
|------|:------:|:------:|-------|
| ext_01_stop_after_external_start_same_session | PASS | — | PASS |
| ext_02_scheduler_after_external_stop_same_session | PASS | — | PASS |
| ext_03_billing_after_external_stop_same_session | FAIL | **PASS** | Initial failure was same-session billing follow-up reusing old `Running` conclusion. Closed by a narrow `tool_choice=DiagnoseBilling` hard guard on adjacent billing keywords. See `ext_03_rerun_report.md` for the post-fix transcript. |

## ext_01_stop_after_external_start_same_session

**Result**: PASS

### Step 1: `帮我关掉 qa-shadow-20260417-01`

**Step result**: PASS

**Hook after output**

```text
state=Stopping
state=Stopping
state=Stopping
state=Stopped
state=Starting
state=Running
final=Running
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

Assistant> 已成功执行关机操作，实例 `qa-shadow-20260417-01` (uhost-1paxrg4g1vfw) 已关机。

⚠️ 提醒：关机后按量模式下，额外磁盘仍会继续计费。如果您长期不使用该实例，建议删除或释放额外磁盘以节省费用。

You>
```

### Step 2: `帮我关掉 qa-shadow-20260417-01`

**Step result**: PASS

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

Assistant> 实例 `qa-shadow-20260417-01` (uhost-1paxrg4g1vfw) 已成功关机。当前状态为已停止。

⚠️ 提醒：关机后按量计费模式下，额外磁盘仍会继续计费。如需彻底停止计费，请删除实例或释放额外磁盘。

You>
```

## ext_02_scheduler_after_external_stop_same_session

**Result**: PASS

### Step 1: `1小时后自动关闭 qa-shadow-20260417-01`

**Step result**: PASS

**Hook after output**

```text
state=Stopping
state=Stopping
state=Stopped
state=Stopping
state=Stopped
final=Stopped
```

```text
  🔧 调用 SetStopSchedulerWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  ❌ DescribeCompShareInstance: [1/3] 查询实例: failed

Assistant> 设置定时关机失败，实例 `qa-shadow-20260417-01` 当前状态为 **关机中（Stopping）**，未处于运行状态，无需设置定时关机。

You>
```

### Step 2: `1小时后自动关闭 qa-shadow-20260417-01`

**Step result**: PASS

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> 查询到实例 `qa-shadow-20260417-01` 当前状态为 **已关机（Stopped）**，定时关机功能仅支持对运行中的实例进行设置，因此无法完成操作。

You>
```

## ext_03_billing_after_external_stop_same_session

**Result**: FAIL

- [step 2] expected one of ['DiagnoseBilling'], got []

### Step 1: `为什么 qa-shadow-20260417-01 在扣费`

**Step result**: PASS

**Hook before output**

```text
state=Starting
state=Starting
state=Running
final=Running
```

**Hook after output**

```text
state=Stopping
state=Stopping
state=Stopping
state=Stopped
final=Stopped
```

```text
  🔧 调用 DiagnoseBilling ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/2] 查询实例列表: concluded

Assistant> 根据诊断结果，您的实例 **qa-shadow-20260417-01 (uhost-1paxrg4g1vfw)** 当前状态为 **运行中(Running)**，计费方式为按量计费，因此会产生持续扣费：

费用明细（每小时）：
- 实例费：¥0.70
- 磁盘费：¥0.00
- **合计**：¥0.70/小时

**建议**：
- 如果您暂时不使用该实例，建议关机，关机后GPU/CPU/内存会停止计费（仅额外磁盘继续收费）
- 如果长期使用，包月计费会更划算，您可以在控制台查看包月价格对比

You>
```

### Step 2: `那为什么 qa-shadow-20260417-01 还在扣费`

**Step result**: FAIL
- expected one of ['DiagnoseBilling'], got []

```text

Assistant> 根据最新的费用诊断结果，您的实例 **qa-shadow-20260417-01 (uhost-1paxrg4g1vfw)** 当前实际状态为 **运行中(Running)**，并非关机状态。

由于您使用的是**按量计费**模式，只要实例保持运行中，GPU/CPU/内存就会持续按小时计费，因此会产生持续扣费。

如果您确认不需要继续使用，可以将实例关机，关机后GPU/CPU/内存会立即停止计费（仅保留磁盘的少量费用）。

You>
```
