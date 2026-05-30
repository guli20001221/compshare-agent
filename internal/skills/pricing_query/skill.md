---
name: pricing_query
description: 用户问某 GPU 机型的产品公开定价（多少钱 / 包月包日 / spot 价）时触发
intent_label: pricing_query
skill_group: catalog
required_tools:
  - GetCompShareInstancePrice
react_tool_subset:
  - GetCompShareInstancePrice
  - DescribeAvailableCompShareInstanceTypes
required_citation: false
applicable_tiers: [fast]
handler_key: handlePricingQuery
planner_directives:
  - 'GPU 价格 / 多少钱 / 几钱 / 费用 / 计费 / 包月包日多少钱类问题应 emit pricing_query。用户已给出 GPU 型号即可路由。'
  - '"X 多少钱一小时" "X 价格" "X 包月多少" "X 包日多少" "X spot 价" "X 抢占式价" 都属本 capability。'
  - '关键区分: 本 capability 仅回答**平台产品公开定价**(购买前调研); 用户对**自己已有账单**的怨言/不解(如"账单怎么这么高"/"扣费太多")应 emit billing_instance, 不在本 capability。'
planner_examples:
  - question: "4090 多少钱一小时"
    confidence: 0.9
  - question: "A100 包月多少钱"
    confidence: 0.85
verification_status: production_validated
field_refs_verified: true
provenance: human_authored
---

# pricing_query

平台 GPU 实例**产品公开定价**查询。回答按量(Postpay)/包日/包月/抢占式(Spot)等分项价格。

读取 `GetCompShareInstancePrice` 的 `Hour/Day/Month/Spot` 计费方式返回；与 `gpu_specs_query` 不同的是这里关注**价格本身**,而 `gpu_specs_query` 关注规格/显存/卡数。

## 正例(产品公开定价 — 购买前)
- 4090 多少钱一小时
- A100 包月多少钱
- 5090 spot 价格
- 4090 24G 一天多少钱
- H20 计费方式
- 4090D 抢占式

## 反例
**规格/库存类(同属产品信息,但走其他 capability)**
- 4090 显存多大 -> gpu_specs_query(规格,非价格)
- 4090 现在有货吗 -> stock_availability(库存,非价格)

**个人账单/资源类(已购买,走 legacy intent)**
- 我账单怎么这么高 -> billing_instance(用户**自己**账单怨言)
- 充值 10 块就被扣完了 我啥也没干啊 -> billing_instance(个人账单投诉)
- 我的实例还剩几天 -> resource_info(用户自己的实例)
- 我的 4090 一小时用了多少钱 -> billing_instance("用了多少"= 个人账单,非产品定价)
- account balance / 我余额 -> billing_account_unsupported(账户级实时数据)

**选型咨询(走 RAG)**
- 推荐用什么 GPU 跑 LoRA -> knowledge_qa(选型建议,非具体价格)

## 边界
- **关键边界**:有"**我的 / 我已经 / 用了**"语义 → billing_instance;**无所有格的产品定价** → pricing_query。 这是 planner 容易漂移的 case,反例段已经把 4 个 billing-shape 钉死。
- 当用户没指明 GPU 型号(如"价格多少")应返回 clarify 提示,不要 default 选 4090。
- 默认 spec(1 卡 / 16 核 / 64GB 内存 / cn-wlcb-01)。 用户明确指定不同 spec 时再覆盖。
- 同型号多 SKU(如 4090 24GB / 4090 48GB)合并展示,标注"标准版/大显存版"。
