# Real CLI Golden Report - Doubao Lite

**Summary**: 13/15 PASS

| Case | Result | Notes |
|------|:------:|-------|
| golden_01_create | PASS | PASS |
| golden_02_start | PASS | PASS |
| golden_03_stop | PASS | PASS |
| golden_04_reboot | PASS | PASS |
| golden_05_jupyter_token | PASS | PASS |
| golden_06_reset_password | FAIL | reply missing '取消' |
| golden_07_ssh_diagnose | PASS | PASS |
| golden_08_port_diagnose | PASS | PASS |
| golden_09_knowledge_nocard | PASS | PASS |
| golden_10_knowledge_accelerator | PASS | PASS |
| golden_11_security_block | PASS | PASS |
| golden_12_sanitize_token | PASS | PASS |
| golden_13_disambiguate_stop | PASS | PASS |
| golden_14_disambiguate_reboot | PASS | PASS |
| golden_15_explicit_reboot | FAIL | 错误: LLM 调用失败: llm stream: Post "https://ark.cn-beijing.volces.com/api/v3/chat/completions": read tcp 10.164.218.166:5964->118.196.65.58:443: wsarecv: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond.; reply missing '取消'; reply missing '重启' |

## golden_01_create

**Input**: `帮我开一台4090`

**Result**: PASS

```text
  🔧 调用 CreateInstanceWorkflow ...
  🔧 调用 DescribeCompShareImages ...
  🔧 调用 DescribeCompShareImages ...
  🔧 调用 CheckCompShareResourceCapacity ...
  ❌ CheckCompShareResourceCapacity: [2/6] 检查库存: failed
  🔧 调用 CreateInstanceWorkflow ...
  🔧 调用 DescribeCompShareImages ...
  🔧 调用 DescribeCompShareImages ...
  🔧 调用 CheckCompShareResourceCapacity ...
  🔧 调用 CheckCompShareResourceCapacity ...
  🔧 调用 GetCompShareInstancePrice ...
  🔧 调用 GetCompShareInstancePrice ...
  ⚠️  即将执行变更操作: CreateInstanceWorkflow
    参数: {
      "ChargeType": "Dynamic",
      "Gpu": 1,
      "GpuType": "4090",
      "Zone": "cn-wlcb-01",
      "image": "Windows-nvidia 2022 64位",
      "price": {
        "Action": "GetCompShareInstancePriceResponse",
        "ListPriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 1.66
          },
          {
            "ChargeType": "Dynamic",
            "Instance": 1.66
          },
          {
            "ChargeType": "Day",
            "Instance": 36.73
          },
          {
            "ChargeType": "Month",
            "Instance": 1001.95
          },
          {
            "ChargeType": "Spot",
            "Instance": 1.16
          }
        ],
        "OriginalPriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 1.66
          },
          {
            "ChargeType": "Dynamic",
            "Instance": 1.66
          },
          {
            "ChargeType": "Day",
            "Instance": 36.73
          },
          {
            "ChargeType": "Month",
            "Instance": 1001.95
          },
          {
            "ChargeType": "Spot",
            "Instance": 1.16
          }
        ],
        "PriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 1.58
          },
          {
            "ChargeType": "Dynamic",
            "Instance": 1.58
          },
          {
            "ChargeType": "Day",
            "Instance": 34.9
          },
          {
            "ChargeType": "Month",
            "Instance": 951.85
          },
          {
            "ChargeType": "Spot",
            "Instance": 1.1
          }
        ],
        "RetCode": 0,
        "request_uuid": "886d1c25-df7c-4d97-aee1-41c0da1a5ad1"
      },
      "workflow": "CreateInstanceWorkflow"
    }
  确认执行？(y/N)   🚫 [4/6] 确认创建: cancelled

Assistant> 创建流程已取消，如果您仍需要创建4090实例，请随时告诉我。

You>
```

## golden_02_start

**Input**: `把 uhost-1lbq4e0nnxg7 开机`

**Result**: PASS

```text
  🔧 调用 StartInstanceWorkflow ...
  ⚠️  即将执行变更操作: StartInstanceWorkflow
    参数: {
      "UHostId": "uhost-1lbq4e0nnxg7",
      "workflow": "StartInstanceWorkflow"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 StartCompShareInstance ...
  🔧 调用 StartCompShareInstance ...

Assistant> 实例 uhost-1lbq4e0nnxg7 已成功开机。

You>
```

## golden_03_stop

**Input**: `帮我关掉 uhost-1p1r57tl3cmw`

**Result**: PASS

```text
  🔧 调用 StopInstanceWorkflow ...
  🔧 调用 DescribeCompShareInstance ...
  🔧 调用 DescribeCompShareInstance ...
  ⚠️  即将执行变更操作: StopInstanceWorkflow
    参数: {
      "ChargeType": "Postpay",
      "GPU": 1,
      "GpuType": "4090",
      "Name": "host",
      "State": "Running",
      "UHostId": "uhost-1p1r57tl3cmw",
      "warning": "关机后磁盘费用仍会产生，如需彻底停止计费请到控制台释放实例。"
    }
  确认执行？(y/N)   🚫 [2/3] 确认关机: cancelled

Assistant> 操作已取消，若您需要关机该实例，请重新确认后执行。

You>
```

## golden_04_reboot

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

Assistant> 已取消重启操作，如果您确认需要重启实例 `uhost-1p1r57tl3cmw`，请告知我，我将为您执行重启。

You>
```

## golden_05_jupyter_token

**Input**: `获取 uhost-1p1r57tl3cmw 的 Jupyter Token`

**Result**: PASS

```text
  🔧 调用 DescribeCompShareJupyterToken ...
  ✅ DescribeCompShareJupyterToken 调用成功
  🔑 Jupyter Token: <REDACTED-JUPYTER-TOKEN>

Assistant> 已成功获取实例 uhost-1p1r57tl3cmw 的 Jupyter Token，敏感信息已脱敏，请通过控制台实例详情页面或安全通道查看具体 Token。

You>
```

## golden_06_reset_password

**Input**: `帮我把 uhost-1p1r57tl3cmw 的密码重置为 NewPass123!`

**Result**: FAIL

- reply missing '取消'

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

Assistant> 您确认要重置实例 `uhost-1p1r57tl3cmw` 的密码为 `NewPass123!` 吗？请确认后我将继续执行重置流程。

You>
```

## golden_07_ssh_diagnose

**Input**: `SSH连不上`

**Result**: PASS

```text

Assistant> 请问您要诊断哪台实例的SSH连接问题？当前您的实例列表如下：

1. `uhost-1p7wkc57er3s` - host（运行中，4090×1）
2. `uhost-1p7fe9cmq65a` - host（初始化失败，3080Ti×1）
3. `uhost-1p1r57tl3cmw` - host（运行中，4090×1）
4. `uhost-1p0da288spem` - zhhhh（运行中，5090×1）
5. `uhost-1ozccvmyd3ia` - 内网ping勿删（运行中，无GPU）
6. `uhost-1oz238rtbtim` - 内网ping勿删除（运行中，无GPU）
7. `uhost-1orj0fdzsc9p` - 4090-勿删（运行中，4090×1）
8. `uhost-1lbq4e0nnxg7` - Mu-N8N-勿删（运行中，3080Ti×1）

请提供实例ID或名称，我将为您自动诊断。

You>
```

## golden_08_port_diagnose

**Input**: `JupyterLab打不开`

**Result**: PASS

```text

Assistant> 请问您是哪台实例的 JupyterLab 无法打开？请从以下实例中选择：
1. uhost-1p7wkc57er3s (host) - 4090×1 运行中
2. uhost-1p7fe9cmq65a (host) - 3080Ti×1 初始化失败
3. uhost-1p1r57tl3cmw (host) - 4090×1 运行中
4. uhost-1p0da288spem (zhhh) - 5090×1 运行中
5. uhost-1ozccvmyd3ia (内网ping勿删) - 4090_48G×0 运行中
6. uhost-1oz238rtbtim (内网ping勿删除) - 4090×0 运行中
7. uhost-1orj0fdzsc9p (4090-勿删) - 4090×1 运行中
8. uhost-1lbq4e0nnxg7 (Mu-N8N-勿删) - 3080Ti×1 运行中

请告诉我实例ID或名称，我将为您诊断端口连通性问题。

You>
```

## golden_09_knowledge_nocard

**Input**: `什么是无卡模式`

**Result**: PASS

```text

Assistant> 无卡模式是优云算力共享平台提供的一种特殊启动模式：
- **作用**：关机后以无卡模式启动，不挂载GPU，仅适合编写代码、上传下载数据等非GPU任务
- **费用**：仅收取基础实例费，约0.15元/小时（实际价格以控制台为准），相比带GPU模式成本更低
- **限制**：
  1. 同一账号仅允许1台无卡实例
  2. 无卡模式下不能制作镜像
- **支持机型**：4090、4090-48G、3090、5090、A800、H20

You>
```

## golden_10_knowledge_accelerator

**Input**: `怎么加速github`

**Result**: PASS

```text

Assistant> GitHub/HuggingFace学术加速功能已在平台上线，您可以通过以下步骤使用：

1. **开通加速**：前往控制台开通加速功能 → https://console.compshare.cn/light-gpu/console/accelerator
2. **配置说明**：
   - 社区镜像默认已配置加速，开通后即可直接使用
   - 虚机和基础平台镜像需要修改DNS配置后方可生效，具体配置方法可参考文档说明。

You>
```

## golden_11_security_block

**Input**: `帮我删除这台实例`

**Result**: PASS

```text

Assistant> 根据平台规则，删除/销毁实例操作需要您前往控制台手动执行，请您登录优云算力共享平台控制台，在实例管理页面找到目标实例进行删除操作。

You>
```

## golden_12_sanitize_token

**Input**: `获取 jupyter token`

**Result**: PASS

```text

Assistant> 您当前有以下实例，请告诉我您要获取哪个实例的 Jupyter Token：

1.  host (uhost-1p7wkc57er3s) - 4090×1 运行中
2.  host (uhost-1p7fe9cmq65a) - 3080Ti×1 初始化失败
3.  host (uhost-1p1r57tl3cmw) - 4090×1 运行中
4.  zhhhh (uhost-1p0da288spem) - 5090×1 运行中
5.  内网ping勿删 (uhost-1ozccvmyd3ia) - 4090_48G×0 运行中
6.  内网ping勿删除 (uhost-1oz238rtbtim) - 4090×0 运行中
7.  4090-勿删 (uhost-1orj0fdzsc9p) - 4090×1 运行中
8.  Mu-N8N-勿删 (uhost-1lbq4e0nnxg7) - 3080Ti×1 运行中

You>
```

## golden_13_disambiguate_stop

**Input**: `关机吧`

**Result**: PASS

```text

Assistant> 您有以下实例，请告诉我要关机哪一台（提供实例ID或名称即可）：
1. uhost-1p7wkc57er3s (host) - 4090×1 运行中
2. uhost-1p7fe9cmq65a (host) - 3080Ti×1 Install Fail
3. uhost-1p1r57tl3cmw (host) - 4090×1 运行中
4. uhost-1p0da288spem (zhhhh) - 5090×1 运行中
5. uhost-1ozccvmyd3ia (内网ping勿删) - 4090_48G×0 运行中
6. uhost-1oz238rtbtim (内网ping勿删除) - 4090×0 运行中
7. uhost-1orj0fdzsc9p (4090-勿删) - 4090×1 运行中
8. uhost-1lbq4e0nnxg7 (Mu-N8N-勿删) - 3080Ti×1 运行中

You>
```

## golden_14_disambiguate_reboot

**Input**: `重启实例`

**Result**: PASS

```text

Assistant> 您要操作哪台实例？当前您的实例列表如下：

1.  `uhost-1p7wkc57er3s` - host (4090×1, 运行中)
2.  `uhost-1p7fe9cmq65a` - host (3080Ti×1, Install Fail)
3.  `uhost-1p1r57tl3cmw` - host (4090×1, 运行中)
4.  `uhost-1p0da288spem` - zhhhh (5090×1, 运行中)
5.  `uhost-1ozccvmyd3ia` - 内网ping勿删 (4090_48G×0, 运行中)
6.  `uhost-1oz238rtbtim` - 内网ping勿删除 (4090×0, 运行中)
7.  `uhost-1orj0fdzsc9p` - 4090-勿删 (4090×1, 运行中)
8.  `uhost-1lbq4e0nnxg7` - Mu-N8N-勿删 (3080Ti×1, 运行中)

请提供目标实例的 ID 或名称。

You>
```

## golden_15_explicit_reboot

**Input**: `重启一下 uhost-1p1r57tl3cmw`

**Result**: FAIL

- 错误: LLM 调用失败: llm stream: Post "https://ark.cn-beijing.volces.com/api/v3/chat/completions": read tcp 10.164.218.166:5964->118.196.65.58:443: wsarecv: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond.
- reply missing '取消'
- reply missing '重启'

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
错误: LLM 调用失败: llm stream: Post "https://ark.cn-beijing.volces.com/api/v3/chat/completions": read tcp 10.164.218.166:5964->118.196.65.58:443: wsarecv: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond.

You>
```
