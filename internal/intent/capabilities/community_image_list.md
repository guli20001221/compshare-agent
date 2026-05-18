---
name: community_image_list
intent_label: community_image_list
required_tool: DescribeCommunityImages
required_citation: false
---

# community_image_list

平台社区镜像市场查询。回答"查询社区镜像 / 社区镜像列表 / 别人发布的镜像"。
读取 `DescribeCommunityImages` 的 `CompshareImageGroup` 分组结构（按名称 + 作者聚合）。

## 用户怎么问（positive examples）
- 查询社区镜像
- 社区镜像列表
- 别人发布的镜像

## 不应使用此能力（negative examples）
- 怎么发布社区镜像 → knowledge_qa（how-to）
- 我自己的镜像 → custom_image_list
- 平台官方镜像 → platform_image_list

## 边界注意
- 返回分组结构（每组对应一个镜像名 + 作者，含多个版本 Data 数组）
- 默认返回前 20 条；不深分页

## Smoke 题
- "查询社区镜像"
