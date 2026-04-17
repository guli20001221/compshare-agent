# Real Account Shadow QA - Followup

**Summary**: 3/3 PASS

| Case | Result | Notes |
|------|:------:|-------|
| shadow_08_stop_two_turn_confirm | PASS | PASS |
| shadow_09_billing_after_real_stop | PASS | PASS |
| shadow_10_start_after_real_stop | PASS | PASS |

## shadow_08_stop_two_turn_confirm

**Input**: `2-step conversation`

**Result**: PASS

### Step 1: `帮我关掉 qa-shadow-20260416-01`

**Step result**: PASS

```text

Assistant> 您确认要关闭实例 qa-shadow-20260416-01（UHostId: uhost-1pafu1vekpoe）吗？请注意：关机后GPU/CPU/内存将停止计费，但额外磁盘仍会继续收费。

You>
```

### Step 2: `确认`

**Step result**: PASS

```text
  🔧 调用 StopInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [1/3] 查询实例: success
  ⚠️  即将执行变更操作: StopInstanceWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "qa-shadow-20260416-01",
      "State": "Running",
      "UHostId": "uhost-1pafu1vekpoe",
      "warning": "关机后磁盘费用仍会产生，如需彻底停止计费请到控制台释放实例。"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 StopCompShareInstance ...
  ✅ StopCompShareInstance [3/3] 关机: success

Assistant> 实例 qa-shadow-20260416-01 已成功关机。
注意：关机后GPU/CPU/内存将停止计费，但额外磁盘仍会继续按按量计费。

You>
```

## shadow_09_billing_after_real_stop

**Input**: `为什么 qa-shadow-20260416-01 关机后还有费用`

**Result**: PASS

```text
  🔧 调用 DiagnoseBilling ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/2] 查询实例列表: concluded

Assistant> 根据查询结果，实例`qa-shadow-20260416-01`（uhost-1pafu1vekpoe）当前的扣费明细为：
- 实例费：¥0（已关机，停计GPU/CPU/内存费用）
- 磁盘费：¥0.00
当前每小时合计扣费¥0.00。

### 补充说明：
1.  该实例为**按量计费（Postpay）**模式，关机后GPU/CPU/内存会停止计费，但如果挂载了额外的数据盘，数据盘费用仍会正常收取
2.  如果是包日/包月/包时实例，关机后依然会收取实例费用，仅按量模式关机后GPU/CPU/内存会停计扣费。

You>
```

## shadow_10_start_after_real_stop

**Input**: `把 qa-shadow-20260416-01 开机`

**Result**: PASS

```text
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

Assistant> 开机失败，错误码RetCode=8357，原因是该实例对应的4090 GPU当前库存不足，无法完成开机操作。

You>
```
