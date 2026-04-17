# Real Account Shadow QA Round2 - Platform Failures

**Summary**: 0/1 PASS

| Case | Result | Notes |
|------|:------:|-------|
| pf_01_create_3090_no_stock_fidelity | FAIL | [step 1] reply missing any of ['??', '??', '???', '????', '??'] |

## pf_01_create_3090_no_stock_fidelity

**Input**: `?????3090???? qa-shadow-20260417-3090??? Ubuntu ???????`

**Result**: FAIL

- [step 1] reply missing any of ['??', '??', '???', '????', '??']

```text
  🔧 调用 CreateInstanceWorkflow ...
  🔧 调用 DescribeCompShareImages ...
  ✅ DescribeCompShareImages [1/7] 查询镜像: success
  🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes [2/7] 查询可用配比: success
  🔧 调用 CheckCompShareResourceCapacity ...
  ✅ CheckCompShareResourceCapacity [3/7] 检查库存: success
  🔧 调用 GetCompShareInstanceUserPrice ...
  ✅ GetCompShareInstanceUserPrice [4/7] 查询价格: success
  ⚠️  即将执行变更操作: CreateInstanceWorkflow
    参数: {
      "CPU": 16,
      "ChargeType": "Dynamic",
      "Gpu": 1,
      "GpuType": "3090",
      "Memory": 65536,
      "Zone": "cn-wlcb-01",
      "image": "Ubuntu-nvidia 22.04",
      "price": {
        "Action": "GetCompShareInstanceUserPriceResponse",
        "ListPriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 1.19
          }
        ],
        "OriginalPriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 1.19
          }
        ],
        "PriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 1.13
          }
        ],
        "RetCode": 0,
        "request_uuid": "0eecd960-bdd0-42f2-aedd-292d76dabb46"
      },
      "workflow": "CreateInstanceWorkflow"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 CreateCompShareInstance ...
  ✅ CreateCompShareInstance [6/7] 创建实例: success
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [7/7] 查看状态: success

Assistant> 已成功创建基于 Ubuntu 系统镜像的 3090 GPU 实例，工作流执行完成。您可以在控制台查看实例状态和连接信息。

You>
```
