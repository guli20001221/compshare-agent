---
name: custom_image_list
intent_label: custom_image_list
skill_group: catalog
required_tool: DescribeCompShareCustomImages
required_citation: false
planner_directives:
  - 'User-owned custom image list questions like "查询自制镜像" should emit custom_image_list.'
planner_examples:
  - question: "查询自制镜像"
    confidence: 0.85
---

# custom_image_list

当前用户的自制镜像列表查询。回答"查询自制镜像 / 我自己制作的镜像 / 自定义镜像有哪些"。
读取 `DescribeCompShareCustomImages` 的列表，仅当前账户的自制镜像。

## 用户怎么问（positive examples）
- 查询自制镜像
- 我制作的镜像
- 我自己上传的镜像列表
- 自定义镜像有哪些

## 不应使用此能力（negative examples）
- 怎么制作自制镜像 → knowledge_qa（how-to）
- 社区镜像 → community_image_list
- 平台官方镜像 → platform_image_list

## 边界注意
- 仅当前账户的自制镜像（API 隐含 account-scoped）
- 不混入平台官方 / 社区镜像

## Smoke 题
- "查询自制镜像"
