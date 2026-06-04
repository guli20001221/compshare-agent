# Image Tag Catalog Live Smoke Report

Date: 2026-06-03

Branch: `codex/diagnosis-routing-optimization`

Purpose: verify the first Phase 6 read-only route expansion, `image_tag_catalog`, against the real CompShare API.

## Positive Case

Question:

```text
镜像都有哪些标签可以筛选
```

Trace directory:

```text
C:\Users\23843\AppData\Local\Temp\compshare-image-tag-current-positive-20260603-052255
```

Trace result:

| Field | Value |
| --- | --- |
| intent | `image_tag_catalog` |
| planned_runtime_form | `routing` |
| actual_runtime_form | `routing` |
| cutover_status | `dispatched` |
| tools | `DescribeCompShareImageTags` |
| mutating tools | none |

The reply rendered real tag categories: AIGC热门, 图像/视频生成, 语音/TTS生成, LLM, 计算机视觉, 科学计算, and 其他.

## Boundary Cases

| Question | Trace directory | Intent | Runtime form | Cutover | Tool |
| --- | --- | --- | --- | --- | --- |
| `有哪些 PyTorch 镜像` | `C:\Users\23843\AppData\Local\Temp\compshare-image-tag-current-boundary_list-20260603-052301` | `platform_image_list` | `routing` | `dispatched` | `DescribeCompShareImages` |
| `系统镜像和应用镜像有什么区别` | `C:\Users\23843\AppData\Local\Temp\compshare-image-tag-current-boundary_qa-20260603-052307` | `knowledge_qa` | `terminal_rag` | `dispatched_retrieval` | `SearchKnowledge` |

Both boundaries stayed out of `image_tag_catalog`.
