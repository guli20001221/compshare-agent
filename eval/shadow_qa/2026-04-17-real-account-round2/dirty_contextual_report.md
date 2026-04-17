# Real Account Shadow QA Round2 - Dirty Contextual

**Summary**: 0/1 PASS

| Case | Result | Notes |
|------|:------:|-------|
| dirty_05_contextual_just_now_clean | FAIL | [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']; [step 1] reply missing '??'; [step 2] expected one of ['StartInstanceWorkflow'], got []; [step 2] reply missing any of ['??', '??', '??'] |

## dirty_05_contextual_just_now_clean

**Input**: `2-step conversation`

**Result**: FAIL

- [step 1] expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']
- [step 1] reply missing '??'
- [step 2] expected one of ['StartInstanceWorkflow'], got []
- [step 2] reply missing any of ['??', '??', '??']

### Step 1: `???? qa-shadow-20260417-01`

**Step result**: FAIL
- expected one of ['StopInstanceWorkflow'], got ['DescribeCompShareInstance']
- reply missing '??'

```text
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance 调用成功

Assistant> **qa-shadow-20260417-01 实例详情**

| 参数 | 信息 |
|------|------|
| 实例ID | `uhost-1paxrg4g1vfw` |
| 状态 | **Running (运行中)** |
| GPU | 3080Ti × 1 (12GB 显存) |
| CPU | 12 核 (Intel/CascadeLakeR) |
| 内存 | 32GB |
| 系统盘 | 60GB CLOUD_SSD |
| 计费方式 | Dynamic (按量后付费) |
| 公网IP | 106.75.68.216 |
| 镜像 | Ubuntu-nvidia 22.04 (系统镜像) |
| SSH 登录 | `ssh ubuntu@106.75.68.216` |

请问您需要对这台实例进行什么操作？（如开机/关机/重启/诊断/查询价格等）

You>
```

### Step 2: `?????????`

**Step result**: FAIL
- expected one of ['StartInstanceWorkflow'], got []
- reply missing any of ['??', '??', '??']

```text

Assistant> 请问您具体需要什么帮助呢？是需要对实例 `qa-shadow-20260417-01` 进行操作（如关机/重启/诊断等），还是有其他问题需要咨询？

You>
```
