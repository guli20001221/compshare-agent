# ext_03 Re-run After Billing Stale Fix

**Summary**: 1/1 PASS

| Case | Result | Notes |
|------|:------:|-------|
| ext_03_billing_after_external_stop_same_session | PASS | PASS |

## ext_03_billing_after_external_stop_same_session

**Result**: PASS

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

Assistant> 根据诊断结果，您的实例 **qa-shadow-20260417-01 (uhost-1paxrg4g1vfw)** 当前状态为 **运行中(Running)**，采用 **按量计费** 方式，因此会持续扣费：

| 费用项 | 单价(每小时) |
|--------|--------------|
| 实例费 | ¥0.70        |
| 磁盘费 | ¥0.00        |
| **合计** | **¥0.70/时** |

**建议**：
- 如果您暂时不使用该实例，请及时关机，关机后 GPU/CPU/内存 将停止计费（仅额外磁盘仍会收取少量费用）。
- 如果您长期使用该实例，包月计费会更划算，您可以在控制台查看包月价格进行对比。

You>
```

### Step 2: `那为什么 qa-shadow-20260417-01 还在扣费`

**Step result**: PASS

```text
  🔧 调用 DiagnoseBilling ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/2] 查询实例列表: concluded

Assistant> 根据最新查询，您的实例 **qa-shadow-20260417-01 (uhost-1paxrg4g1vfw)** 当前状态已更新为 **已关机(Stopped)**：

| 费用项 | 单价(每小时) |
|--------|--------------|
| 实例费 | ¥0.00（已关机停止计费） |
| 磁盘费 | ¥0.00 |
| **合计** | **¥0.00/时** |

目前该实例已停止扣费。如果您在账单中仍看到历史扣费记录，那是关机前产生的费用。如有其他疑问，可前往控制台费用明细页面查看完整的扣费记录。

You>
```
