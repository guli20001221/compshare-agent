---
name: network_accelerator_status
description: 用户问优云智算网络加速是否已开通 / 网速慢想确认加速状态时触发；GitHub/HuggingFace 加速教程仍走 knowledge_qa
metadata:
  intent_label: network_accelerator_status
  skill_group: network
  required_tools:
    - CheckCompShareNetOptimizer
  react_tool_subset:
    - CheckCompShareNetOptimizer
  required_citation: false
  applicable_tiers: [fast]
  handler_key: handleNetAcceleratorStatus
  planner_directives:
    - '用户问"网络加速开了吗"、"网速慢帮我看看是不是没开网络加速"、"网络加速状态"时 emit network_accelerator_status。'
    - '本 capability 只查询网络加速开通状态；不要承诺能开通、代开通或修改网络配置。'
    - '关键边界: "怎么加速 GitHub / HuggingFace / pip / apt 下载" 是教程或配置问题，应 emit knowledge_qa，不能 emit network_accelerator_status。'
  planner_examples:
    - question: "网速怎么这么慢，帮我看看网络加速开没开"
      confidence: 0.86
  verification_status: production_validated
  field_refs_verified: true
  provenance: human_authored
---

# network_accelerator_status

查询优云智算网络加速是否已开通。只读，不开通、不关闭、不修改网络配置。

## 正例

- 网速怎么这么慢，帮我看看网络加速开没开
- 网络加速是不是没开
- 现在网络加速状态是什么

## 反例

- 怎么加速 GitHub 下载 -> knowledge_qa
- HuggingFace 下载太慢怎么处理 -> knowledge_qa
- pip / apt 下载慢怎么换源 -> knowledge_qa

## 边界

- 本 skill 只回答网络加速状态。若未开通，只提示用户去控制台处理。
- 下载源、代理、镜像源、第三方站点访问优化属于教程问题，走 knowledge_qa。
