# Real Account Shadow QA Round2 - Create

**Summary**: 0/1 PASS

| Case | Result | Notes |
|------|:------:|-------|
| round2_create_test_instance | FAIL | [step 1] reply missing any of ['??????', '?????', '??', '??'] |

## round2_create_test_instance

**Input**: `?????3080Ti???? qa-shadow-20260417-01??? Ubuntu ???????`

**Result**: FAIL

- [step 1] reply missing any of ['??????', '?????', '??', '??']

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
      "CPU": 12,
      "ChargeType": "Dynamic",
      "Gpu": 1,
      "GpuType": "3080Ti",
      "Memory": 32768,
      "Zone": "cn-wlcb-01",
      "image": "Ubuntu-nvidia 22.04",
      "price": {
        "Action": "GetCompShareInstanceUserPriceResponse",
        "ListPriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 0.74
          }
        ],
        "OriginalPriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 0.74
          }
        ],
        "PriceDetails": [
          {
            "ChargeType": "Postpay",
            "Instance": 0.7
          }
        ],
        "RetCode": 0,
        "request_uuid": "b93d7464-4359-4871-a5e4-e0191fb583ec"
      },
      "workflow": "CreateInstanceWorkflow"
    }
  确认执行？(y/N)   🔧 调用  ...
  🔧 调用 CreateCompShareInstance ...
  ✅ CreateCompShareInstance [6/7] 创建实例: success
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [7/7] 查看状态: success

Assistant> 实例已成功创建，您可以前往控制台查看实例状态和连接信息。如有后续操作需求（如开机、诊断等），随时告诉我。

You>
```
