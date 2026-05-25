---
name: platform_image_list
intent_label: platform_image_list
skill_group: catalog
required_tool: DescribeCompShareImages
required_citation: false
planner_directives:
  - 'Platform image list questions like "查询平台镜像列表" or "Ubuntu 22.04 镜像有吗" should emit platform_image_list.'
  - 'Concept questions like "系统镜像和基础镜像有什么区别" or how-to questions like "怎么发布社区镜像" stay in knowledge_qa, NOT image-list capabilities.'
planner_examples:
  - question: "查询平台镜像列表"
    confidence: 0.85
---

# platform_image_list

平台官方镜像列表查询。回答"平台支持哪些系统镜像 / Ubuntu 22.04 镜像有吗 / CUDA 镜像列表"。
读取 `DescribeCompShareImages` 的 `ImageSet`，仅平台官方（System + App 两类），不含自制 / 社区。

## 用户怎么问（positive examples）
- 查询平台镜像列表
- 平台支持哪些系统镜像
- Ubuntu 22.04 镜像有吗
- CUDA 镜像列表
- 平台官方 PyTorch 镜像

## 不应使用此能力（negative examples）
- 系统镜像和基础镜像有什么区别 → knowledge_qa（概念解释）
- 我自己制作的镜像 → custom_image_list
- 别人发布的社区镜像 → community_image_list
- 怎么发布社区镜像 → knowledge_qa（how-to）

## 边界注意
- 仅返回平台官方镜像；不混入用户自制或社区镜像
- ImageType 枚举：System（裸 OS）/ App（应用基础镜像如 PyTorch / CUDA / ComfyUI / Ollama）

## Smoke 题
- "查询平台镜像列表"
- "Ubuntu 22.04 镜像有吗"
