---
name: gpu_specs_query
intent_label: gpu_specs_query
required_tool: DescribeAvailableCompShareInstanceTypes
required_citation: false
---

# gpu_specs_query

平台 GPU 机型规格查询。回答"X 显存多大 / X 支持几张卡 / X 的合法配置"这类静态规格题。
读取 `DescribeAvailableCompShareInstanceTypes` 的 `Name` + `MachineSizes`（GPU 数量、CPU/内存组合），不读 `Status`（库存归 stock_availability）。

## 用户怎么问（positive examples）
- 4090 显存多大
- A100 支持几张卡
- RTX40 系列 GPU 规格
- H100 显存
- 4090 最大配多少内存

## 不应使用此能力（negative examples）
- 4090 现在有没有货 → stock_availability
- 4090 多少钱 → 不在本 PR 范围（pricing 工具）
- 查我账下 4090 实例 → resource_info
- H100 在哪个机房 → 不支持（CompShare 无 H100 在售）

## 边界注意
- H100 / H200 不在 CompShare 售卖范围（`internal/knowledge/gpu_specs_test.go:126` 明示禁推荐），tool 返回空集合或 SoldOut 时礼貌说明
- 不返回精确库存数量（那是 stock_availability + 兜底"不公开"语义）

## Smoke 题
- "4090 显存多大"
- "A100 支持几张卡"
