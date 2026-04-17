# Real Account Shadow QA - Create 3090

**Summary**: 1/1 PASS

| Case | Result | Notes |
|------|:------:|-------|
| shadow_01b_create_3090_stock_probe | PASS | PASS |

## shadow_01b_create_3090_stock_probe

**Input**: `帮我开一台 3090，实例名 qa-shadow-20260416-01b，平台 Ubuntu 系统镜像，按量`

**Result**: PASS

```text
  🔧 调用 CreateInstanceWorkflow ...
  🔧 调用 DescribeCompShareImages ...
  ✅ DescribeCompShareImages [1/7] 查询镜像: success
  🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes [2/7] 查询可用配比: success
  🔧 调用 CheckCompShareResourceCapacity ...
  ❌ CheckCompShareResourceCapacity: [3/7] 检查库存: failed
  🔧 调用 DescribeAvailableCompShareInstanceTypes ...
  ✅ DescribeAvailableCompShareInstanceTypes 调用成功

Assistant> 很抱歉，您指定的3090 GPU当前在`cn-wlcb-01`可用区已售罄，无法创建。目前该可用区尚有库存的GPU机型包括：
1.  **4090**：24GB显存，性能评级83，支持1卡16C/64GB内存配置
2.  **5090**：32GB显存，性能评级105，支持1卡16C/96GB内存配置
3.  **4090_48G**：48GB显存，性能评级83，支持1卡16C/96GB内存配置
4.  **3080Ti**：12GB显存，性能评级34，支持1卡12C/32GB内存配置

请问您需要更换为以上哪一款GPU机型创建实例？实例名称仍为`qa-shadow-20260416-01b`，使用Ubuntu平台系统镜像，按量计费。

You>
```
