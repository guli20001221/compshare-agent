# Model Repository Browse Live Smoke Report

Date: 2026-06-03

Branch: `codex/diagnosis-routing-optimization`

Purpose: verify the second Phase 6 read-only route expansion, `model_repository_browse`, against the real CompShare API.

## Positive Case

Question:

```text
模型仓库里有哪些 LLM 模型可以直接用
```

Trace directory:

```text
C:\Users\23843\AppData\Local\Temp\compshare-model-repo-current2-positive_llm-20260603-053802
```

Trace result:

| Field | Value |
| --- | --- |
| intent | `model_repository_browse` |
| planned_runtime_form | `routing` |
| actual_runtime_form | `routing` |
| cutover_status | `dispatched` |
| tools | `DescribeModelRepositoryTags`, `DescribeModelRepositoryModels` |
| mutating tools | none |

The reply rendered real model repository tags (`Model`, `AI`) and real model entries such as Llama/Gemma model paths. Duplicate tags from the API were de-duplicated before rendering.

## Boundary Cases

| Question | Trace directory | Intent | Runtime form | Cutover | Tool / action |
| --- | --- | --- | --- | --- | --- |
| `怎么把 HuggingFace 上的模型下载到实例里` | `C:\Users\23843\AppData\Local\Temp\compshare-model-repo-current2-boundary_hf_download-20260603-053808` | `knowledge_qa` | `terminal_rag` | `dispatched_retrieval` | `SearchKnowledge` |
| `帮我部署个 Qwen2.5-7B 跑推理服务` | `C:\Users\23843\AppData\Local\Temp\compshare-model-repo-current2-boundary_deploy-20260603-053820` | `deploy_model` | `agent` | `dispatched_agent` | `deploy_match`, `CheckCompShareResourceCapacity` |

Both boundaries stayed out of `model_repository_browse`. The deploy boundary remained read-only in this run because mutating tools were disabled.
