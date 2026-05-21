---
name: gpu_specs_query
intent_label: gpu_specs_query
required_tool: DescribeAvailableCompShareInstanceTypes
required_citation: false
planner_directives:
  - '普通 GPU 规格、显存、性能、最大卡数问题应 emit gpu_specs_query，并先输出概览。'
  - '用户明确要求所有/全部/完整规格、某型号所有规格、CPU/内存组合、CPU 和内存、可选配置时，也应 emit gpu_specs_query；渲染层会展开 MachineSizes.Collection.Memory 的所有组合。'
planner_examples:
  - question: "4090 显存多大"
    confidence: 0.85
---

# gpu_specs_query

平台 GPU 机型规格查询。回答显存、性能、最大卡数、合法 CPU/内存/GPU 组合这类静态规格问题。

读取 `DescribeAvailableCompShareInstanceTypes` 的 `Name`、`Performance`、`GraphicsMemory`、`MachineSizes`。库存归 `stock_availability`，本能力不承诺精确库存数量。

## 正例
- 4090 显存多大
- A100 支持几张卡
- RTX40 系列 GPU 规格
- 4090 的所有规格
- 列出所有 GPU 规格
- 4090 有哪些 CPU/内存组合
- 4090 支持哪些 CPU 和内存

## 反例
- 4090 现在有没有货 -> stock_availability
- 4090 多少钱 -> pricing 工具
- 查我账号下 4090 实例 -> resource_info

## 边界
- 普通规格问法先输出概览，不展开所有 CPU/内存组合。
- 明确要求“所有/完整规格”或“CPU/内存组合”时，要保留接口返回的所有规格组合。
