# Shared Image List Live Smoke Report

Date: 2026-06-03

Branch: `codex/diagnosis-routing-optimization`

Purpose: verify the third Phase 6 read-only route expansion, `shared_image_list`, against the real CompShare API.

## Positive Case

Question:

```text
别人共享给我的镜像在哪看
```

Trace directory:

```text
C:\Users\23843\AppData\Local\Temp\compshare-shared-image-positive_shared-20260603-054652
```

Trace result:

| Field | Value |
| --- | --- |
| intent | `shared_image_list` |
| planned_runtime_form | `routing` |
| actual_runtime_form | `routing` |
| cutover_status | `dispatched` |
| tools | `DescribeCompShareSharingImages` |
| mutating tools | none |

The real API returned two shared images. The committed report omits the concrete image IDs and only records that the route used the expected read-only API.

## Boundary Cases

| Question | Trace directory | Intent | Runtime form | Cutover | Tool |
| --- | --- | --- | --- | --- | --- |
| `社区里别人发布的镜像在哪看` | `C:\Users\23843\AppData\Local\Temp\compshare-shared-image-boundary_community-20260603-054701` | `community_image_list` | `routing` | `dispatched` | `DescribeCommunityImages` |
| `怎么把我自己的镜像共享给别人` | `C:\Users\23843\AppData\Local\Temp\compshare-shared-image-boundary_share_out-20260603-054708` | `knowledge_qa` | `terminal_rag` | `dispatched_retrieval` | `SearchKnowledge` |

Both boundaries stayed out of `shared_image_list`. The write-adjacent "share my own image" question stayed out of the read-only route.
