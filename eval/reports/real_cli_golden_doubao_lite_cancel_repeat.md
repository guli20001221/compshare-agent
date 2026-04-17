# Real CLI Golden Cancel Repeat - Doubao Lite

**Summary**: 6/6 PASS

| Case | Result | Notes |
|------|:------:|-------|
| golden_06_reset_password | PASS | PASS |
| golden_06_reset_password | PASS | PASS |
| golden_06_reset_password | PASS | PASS |
| golden_15_explicit_reboot | PASS | PASS |
| golden_15_explicit_reboot | PASS | PASS |
| golden_15_explicit_reboot | PASS | PASS |

## golden_06_reset_password

**Input**: `帮我把 uhost-1p1r57tl3cmw 的密码重置为 NewPass123!`

**Result**: PASS

```text
  🔧 调用 ResetPasswordWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  🔧 调用 DescribeCompShareInstance ...
  ⚠️  即将执行变更操作: ResetPasswordWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "host",
      "Password": "[已设置,不显示]",
      "State": "Running",
      "UHostId": "uhost-1p1r57tl3cmw",
      "warning": "密码要求：8-32字符，至少包含2种字符类型（大小写字母/数字/特殊字符）。"
    }
  确认执行？(y/N)   🚫 [2/4] 确认重置: cancelled

Assistant> 已为您取消密码重置操作，如需重置请重新发起请求。

You>
```

## golden_06_reset_password

**Input**: `帮我把 uhost-1p1r57tl3cmw 的密码重置为 NewPass123!`

**Result**: PASS

```text
  🔧 调用 ResetPasswordWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  🔧 调用 DescribeCompShareInstance ...
  ⚠️  即将执行变更操作: ResetPasswordWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "host",
      "Password": "[已设置,不显示]",
      "State": "Running",
      "UHostId": "uhost-1p1r57tl3cmw",
      "warning": "密码要求：8-32字符，至少包含2种字符类型（大小写字母/数字/特殊字符）。"
    }
  确认执行？(y/N)   🚫 [2/4] 确认重置: cancelled

Assistant> 已取消重置实例 `uhost-1p1r57tl3cmw` 密码的操作。如果您需要重新重置，请再次发起请求。

You>
```

## golden_06_reset_password

**Input**: `帮我把 uhost-1p1r57tl3cmw 的密码重置为 NewPass123!`

**Result**: PASS

```text
  🔧 调用 ResetPasswordWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  🔧 调用 DescribeCompShareInstance ...
  ⚠️  即将执行变更操作: ResetPasswordWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "host",
      "Password": "[已设置,不显示]",
      "State": "Running",
      "UHostId": "uhost-1p1r57tl3cmw",
      "warning": "密码要求：8-32字符，至少包含2种字符类型（大小写字母/数字/特殊字符）。"
    }
  确认执行？(y/N)   🚫 [2/4] 确认重置: cancelled

Assistant> 已取消重置密码操作，如有需要可重新发起请求。

You>
```

## golden_15_explicit_reboot

**Input**: `重启一下 uhost-1p1r57tl3cmw`

**Result**: PASS

```text
  🔧 调用 RebootInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  🔧 调用 DescribeCompShareInstance ...
  ⚠️  即将执行变更操作: RebootInstanceWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "host",
      "State": "Running",
      "UHostId": "uhost-1p1r57tl3cmw",
      "warning": "重启会中断当前运行的任务，请确保已保存工作。"
    }
  确认执行？(y/N)   🚫 [2/3] 确认重启: cancelled

Assistant> 重启操作已取消，若您确认需要重启实例 uhost-1p1r57tl3cmw，请重新发起请求。

You>
```

## golden_15_explicit_reboot

**Input**: `重启一下 uhost-1p1r57tl3cmw`

**Result**: PASS

```text
  🔧 调用 RebootInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  🔧 调用 DescribeCompShareInstance ...
  ⚠️  即将执行变更操作: RebootInstanceWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "host",
      "State": "Running",
      "UHostId": "uhost-1p1r57tl3cmw",
      "warning": "重启会中断当前运行的任务，请确保已保存工作。"
    }
  确认执行？(y/N)   🚫 [2/3] 确认重启: cancelled

Assistant> 已取消重启操作，若您需要重启实例 `uhost-1p1r57tl3cmw`，可重新发起请求。

You>
```

## golden_15_explicit_reboot

**Input**: `重启一下 uhost-1p1r57tl3cmw`

**Result**: PASS

```text
  🔧 调用 RebootInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  🔧 调用 DescribeCompShareInstance ...
  ⚠️  即将执行变更操作: RebootInstanceWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "host",
      "State": "Running",
      "UHostId": "uhost-1p1r57tl3cmw",
      "warning": "重启会中断当前运行的任务，请确保已保存工作。"
    }
  确认执行？(y/N)   🚫 [2/3] 确认重启: cancelled

Assistant> 已取消重启操作，若需要重启可再次发起请求。

You>
```
