# RAG Phase 2 — live CLI smoke report (2026-06-06)

Authoritative faithfulness + wiring check for the external tool/ops corpus served
at runtime. Default-OFF; this smoke runs with `COMPSHARE_EXTERNAL_KNOWLEDGE=1`.

## Setup

- Binary: `agent.exe` built from this branch (`feat/rag-external-corpus`).
- Config: dev `deploy/conf/agent.yaml`; retrieval mode `qwen3_rrf` (default).
- Env: `COMPSHARE_EXTERNAL_KNOWLEDGE=1`, `COMPSHARE_TRACE_ENABLED=1`.
- Corpus: platform `stage2b_w0.jsonl` (687) + external `external_w0.jsonl` (29).

## Boot + platform non-regression

- Boot log: `rag: merged external knowledge corpus deploy/kb/external_w0.jsonl into the index (716 total chunks)`.
- Platform control question `包月是怎么收费的` → correct grounded platform billing
  answer (包月/退费规则/云盘 0.3 元/GB/月). Platform RAG **unchanged** by the merge.

## External tool questions (4/4 grounded + cited)

Each routed to `intent=knowledge_qa` (runtime form `terminal_rag`), retrieved from
the `kb_version=merged` index via the `SearchKnowledge` step, and cited the
correct external chunk. Trace fields (intent / kb_version=merged / SearchKnowledge
invoked / cited) verified directly from the per-turn JSONL trace.

| # | Question | cited external chunk(s) |
|---|---|---|
| 1 | vllm 怎么启动一个 openai 兼容的 api 服务 | `ext-vllm-serving-001` |
| 2 | vllm 加载模型报显存不足怎么降 | `ext-gpu-oom-vllm-001`, `ext-vllm-quantization-001` |
| 3 | ollama 怎么用 openai 的库去调 | `ext-ollama-openai-001` |
| 4 | 怎么用 nvidia-smi 看显存被谁占了 | `ext-gpu-nvidia-smi-001` |

All four answers are faithful to the cited chunk content ("根据资料…", correct
commands/flags, no fabrication beyond the evidence).

## Secret-redaction note (offline false positive ≠ runtime)

Q3 cites the Ollama serving chunk, which documents `api_key="ollama"` /
`api_key="EMPTY"`. The runtime answer **preserved these placeholders correctly** —
`security.RedactOperationalTokensInText` (engine.go) only redacts
`Bearer`/`token=`/CompShare tokens, not `api_key=`. The offline `evaluate_answers`
harness flags these via `unsafe_cleaned_matches`/`SECRET_RE` (18/29
"safety_failure"), but that is a **harness-only false positive**; the runtime path
is unaffected. → authoritative faithfulness check is this CLI smoke, not offline.

## Other gates (reproduced)

- External retrieval eval (`qwen3_rrf`): **Top-3 = 1.0 (29/29)**, all groups 1.0.
- `go test ./...` (`COMPSHARE_PROJECT_ID=test-project`): exit 0.
- `scripts/test_rag_w0_scripts.py`: 139 passed.
- `loadKnowledgeCorpora` integration test: OFF=687, ON=716, broken-external=687
  (graceful fallback).

## Status

Phase 2 verified end-to-end. External knowledge is **reachable now** for tool
how-to questions (they classify as `knowledge_qa`). Default stays **OFF**; flipping
`COMPSHARE_EXTERNAL_KNOWLEDGE` on requires a platform 377-Q parity re-run against
the 716-chunk merged index first.
