# Real Account Shadow QA Round6 - Platform Fidelity (Doubao-Seed-Code)

**Summary**: 1/1 PASS

| Case | Result | Notes |
|------|:------:|-------|
| pfmini_01_start_4090_fidelity_probe | PASS | PASS |

## pfmini_01_start_4090_fidelity_probe

**Result**: PASS

### Step 1: `帮我开机 qa-shadow-20260417-4090`

**Step result**: PASS

**Hook before output**

```text
Stopped
```

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
      "GpuType": "4090",
      "Name": "qa-shadow-20260417-4090",
      "State": "Stopped",
      "UHostId": "uhost-1payastkiw8o"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 StartCompShareInstance ...
  ✅ StartCompShareInstance [3/3] 开机: success

Assistant> 实例已成功开机！

您的实例 **qa-shadow-20260417-4090** (uhost-1payastkiw8o) 现在正在启动中，稍等片刻即可使用。

You>
```
