---
name: stock_availability
intent_label: stock_availability
required_tool: DescribeAvailableCompShareInstanceTypes
required_citation: false
---

# stock_availability

平台 GPU 库存可售性查询。回答"X 现在有没有货 / X 售罄了吗 / 哪些机型能买"这类实时可售题。
先读取 `DescribeAvailableCompShareInstanceTypes` 的 `Status` 和 `Zone` 字段；当用户明确问某个 GPU 型号且状态为 Normal 时，再用系统镜像做 `CheckCompShareResourceCapacity` 容量预检，回答当前是否真实可创建。**不返回精确剩余数量**。

## 用户怎么问（positive examples）
- 4090 现在有没有货
- A100 售罄了吗
- 上海机房 H100 库存
- 什么机型现在能买
- 4090 还能开吗

## 不应使用此能力（negative examples）
- 4090 显存多大 → gpu_specs_query
- 4090 多少钱 → 不在本 PR 范围
- 我账下的 4090 实例 → resource_info

## 边界注意
- `Normal` 只表示机型开售，不等于当前一定可创建；明确型号的库存问题要以容量预检结果为准
- 仅回答是否可创建，**不返回精确剩余数量**（API 设计不公开）
- H100 不在 CompShare 在售机型范围，明确说"暂不在售"，不要推断库存

## Smoke 题
- "4090 现在有没有货"
- "上海机房 H100 库存"
