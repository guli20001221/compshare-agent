# RAG anti-confusion anchor — live A/B smoke report (2026-06-06)

Follow-up to the platform 256-Q retrieval parity (`platform_parity_report.json`).
The parity proved the merged (687 platform + 29 external) index does **not**
regress platform Top-3 retrieval (aggregate + per-group identical). It also
surfaced one answer-faithfulness *risk*: external self-hosting serving chunks
(`ext-ollama-openai-001`, `ext-vllm-serving-001`) are topically adjacent to the
ModelVerse hosted-OpenAI-compatible-API cluster and get pulled into those
questions' top-3.

This report checks, on the real runtime path (CLI, ds-v4-flash, external-on),
whether that retrieval-layer collision actually misdirects the **answer** — and
whether the new anti-confusion anchor in
`internal/prompt/rag_system_segments/third_party_tool_addendum.txt` changes
anything.

## Method — single-variable A/B

Same 6 questions, same env (`COMPSHARE_EXTERNAL_KNOWLEDGE=1`,
`RAG_RETRIEVAL_MODE=qwen3_rrf`, `USE_GROUNDED_RENDERER=llm`), git-bash UTF-8
stdin (avoids the PS5.1 GBK trap). The **only** difference between the two runs
is the prompt: `agent.exe` (anchor) vs `agent_noanchor.exe` (the addendum
reverted to its pre-anchor `HEAD` state, rebuilt).

The 6 questions are the parity's collision set: 5 ModelVerse hosted-API
(`grok` 0194, `Qwen3-vl-Plus` 0196, `gemini-3-pro` 0198, `doubao` 0202, "OpenAI
SDK base url" 0234) + the account-quota vs process-config case (`分配 GPU 额度`
0063).

## Result

Every turn (both builds): `intent=knowledge_qa`, `kb_version=merged`. The
external chunk **is** retrieved into top-3 at runtime in all 6 — the collision
is real — but is **not cited** in either build:

| # | question | external chunk in top-3 | cited | answer (both builds) |
|---|---|---|---|---|
| 0194 | grok via OpenAI | `ext-ollama-openai-001` | platform only | platform protocol table ✅ |
| 0196 | Qwen3-vl-Plus via OpenAI | `ext-vllm-serving-001` | platform only | `api.modelverse.cn/v1` + API Key ✅ |
| 0198 | gemini-3-pro via OpenAI | `ext-ollama-openai-001` | platform only | platform + honest hedge via `GET /v1/models` ✅ |
| 0202 | doubao via OpenAI | `ext-ollama-openai-001` | platform only | platform protocol table ✅ |
| 0234 | OpenAI SDK base url | `ext-ollama-openai-001` | platform only | `api.modelverse.cn/v1` ✅ |
| 0063 | allocate GPU quota to employees | `ext-gpu-visible-devices-001` | platform only | platform team-amount API + UI ✅ (no `CUDA_VISIBLE_DEVICES`) |

**Both** the anchor and no-anchor builds answer all 6 platform-correctly, with
**zero** self-host / `CUDA_VISIBLE_DEVICES` misdirection.

## Conclusion — the anchor is DEFENSIVE INSURANCE, not a proven-necessary fix

The existing guards — the third-party-tool addendum, the anti-fabrication
anchors, the reranker keeping platform chunks at rank-1, and the
grounded-renderer's citation discipline — **already** keep all 6 collision cases
platform-correct. The external chunk is retrieved-but-not-cited in both builds,
so the new anchor did not flip any case wrong→right on this set.

The anchor is kept as cheap, harmless, scale-safe insurance: it makes the
"hosted ModelVerse API vs self-hosting in your instance" rule explicit, so the
guarantee is robust as the external corpus scales in Phase 5 (sglang / comfyui /
ollama batches add more self-hosting chunks competing with platform-API chunks,
and today's clean reranker result is not guaranteed at 100+ external chunks). It
is **not** presented as a fix for a demonstrated misdirection.

Verification: `go test ./...` (`COMPSHARE_PROJECT_ID=test-project`) exit 0,
including the protective `TestBuildRAGMessagesEncodesPlatformVsSelfHostAntiConfusion`.

## Default-on status

`COMPSHARE_EXTERNAL_KNOWLEDGE` stays **OFF**. Both gates now pass (retrieval
parity + this answer-faithfulness A/B), so the flip is lower-risk than feared,
but the decision is deferred to PR review.
