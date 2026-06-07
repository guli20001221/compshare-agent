# Agentic-RAG SearchKnowledge — enablement evidence (unified plan P5)

Captured 2026-06-07 against `exec/p5-agentic-flip`, `ds-v4-flash`, qwen3 RRF, N=5
(and an N=3 prod-default smoke). Companion to `before_state_report.md` (the flag-OFF
baseline).

## Decision: ship DEFAULT-OFF; re-home the enablement to the external-KB flip
The plan called for flipping `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE` default-on in P5.
Live evidence (below) shows the flip only delivers value when the **external** corpus
is loaded, and is actively harmful at the production default (external KB off). So
this PR ships the machinery + retrieval observability **default-off**, and re-homes
the default-on enablement to the `COMPSHARE_EXTERNAL_KNOWLEDGE`-on decision (its own
parity-gated flip): enable the two together, not before.

What ships now (all default-off, byte-identical to pre-agentic when off):
- `executeSearchKnowledge` + `emitSearchKnowledgeRetrievalTrace` — agent-lane
  retrieval is now observable (enabled, hits, chunk_ids) in traces and eval.
- the P4a empty-target dead-end relax (flag-gated).
- the env gate (`1/on/true/yes` → on, used with external KB on / in eval;
  `""/0/off/...` → off; unknown → off + warn).

## Evidence A — WHEN ENABLED WITH EXTERNAL KB ON (`External=1`, N=5)
This is what the flip delivers once external KB is loaded. Raw:
`after_state_p5_observations.jsonl`.

| set | probes | N | intent | form | retrieval_fired | retrieved chunks (run1) |
|---|---|---|---|---|---|---|
| core tool-ops | `vllm 进程被 kill` | 5 | diagnosis | agent | 5/5 true (h=3) | `ext-gpu-oom-vs-ram-001, ext-gpu-oom-vllm-001, ext-vllm-cuda-error-001` |
| | `sglang 启动报显存不足` | 5 | diagnosis | agent | 5/5 true (h=3) | `ext-sglang-oom-001, ext-gpu-oom-vllm-001, ext-gpu-pytorch-oom-001` |
| | `vllm serve 报 ValueError` | 5 | diagnosis | agent | 5/5 true (h=3) | `ext-vllm-serving-001, ext-vllm-startup-hang-001, ext-vllm-offline-001` |
| generic platform | `我的实例突然连不上了` | 5 | diagnosis | agent(3)/""(2) | 0/5 (not forced) | — clarifies |
| regression | `4090 多少钱` / `4090 有没有货` | 10 | pricing/stock | routing | 0/10 | no mis-fire |
| regression | `vllm 怎么启动 openai 服务` | 5 | knowledge_qa | terminal_rag | 5/5 true (h=3) | `ext-vllm-serving-001` (+ **cited** `ext-vllm-serving-001`) |

- **vllm-killed / sglang-oom (10/10): strongly grounded** — answers carry the RELEVANT
  external chunks' content (OOM-Killer RAM-vs-VRAM + `dmesg`/`free`/`--max-model-len`;
  `--mem-fraction-static 0.7`/`--tp`). The retrieved `ext-*` ids are topically correct.
- **vllm-valueerr (5/5): partial coverage** — retrieval fires + retrieves `ext-vllm-*`,
  but the model states "没有直接匹配到…标准排查条目" and gives concrete general triage +
  asks for the log. Honest; not a clean single-chunk grounded pass.
- `[n]`-marker **cited** is terminal-RAG-only; agent-lane grounding is evidenced by
  `retrieval_fired` + `retrieved_chunk_ids` + the no-raw-leak-guarded synthesis.

## Evidence B — PROD DEFAULT (`External=0`, N=3) — WHY default-off
Raw: `extoff_smoke_observations.jsonl` (probes: `extoff_smoke_probes.json`).

| probe | SearchKnowledge | retrieved (chunk_ids) | problem |
|---|---|---|---|
| `vllm 进程被 kill` | fires (h=3) | `w0-resource_purchase-…, w0-modelverse-…, w0-image-…` | **false-grounding**: topically irrelevant platform docs |
| `sglang 启动报显存不足` | fires (h=3) | `w0-driver_cuda-…, w0-image-…, w0-init_failure-…` | mostly irrelevant |
| `vllm serve 报 ValueError` | fires (h=3) | `w0-image-…, w0-resource_purchase-…, w0-modelverse-…` | irrelevant |
| `ssh连接超时一直进不去` | **does NOT fire** | — | platform symptom → clarifies (instance-specific) |
| `跑模型的时候说找不到GPU` | **does NOT fire** | — | platform symptom → clarifies |

Two findings: (1) at external-off the retriever still returns top-3, so tool-ops
symptoms **false-ground on irrelevant platform chunks**; (2) platform symptoms
(SSH/GPU) **don't fire SearchKnowledge at all** — the ReAct loop correctly clarifies
for instance-specific issues. So the agentic win is specifically **tool-ops + external
KB**; a default-on flip at external-off has **no positive value** (tool-ops → wrong
chunks; platform → clarify as before) and a false-grounding risk. Even a perfect
relevance gate would only suppress the harm, not create value. Hence default-off until
external KB is on. (This refines the plan's "platform-symptom win" claim.)

## Relevance floor (added) + the platform-symptom limit (honest)
Probes: `relevance_floor_probes.json` / `relevance_floor_observations.jsonl` (External=0, N=3).

The retriever always returns top-K, so at external-off a tool-ops symptom gets
topically-irrelevant platform chunks. `executeSearchKnowledge` now applies a
**relevance floor** (reuses the existing `isWeakEvidence` / `weakEvidenceSemanticThreshold=0.5`;
verified: relevant ext-* hits score 0.60–0.99 (kept), irrelevant platform hits score
0.01–0.07 (dropped)). Result at External=0:
- **tool-ops (vLLM/SGLang): false-grounding FIXED.** `weak=true` 3/3 → the agent now
  says e.g. *"知识库中没有直接关于 vLLM 进程被 kill 的专项文档…"* and gives general
  guidance instead of leaning on the irrelevant chunks.
- **platform symptoms (SSH/GPU/init/port): still clarify-first.** They do NOT reliably
  call SearchKnowledge (`b-ssh` sk=0/3, `b-init` sk=0/3, `b-gpu`/`b-port` sk=1/3) — the
  LLM judges these instance-specific and asks "which instance?" without retrieving the
  (existing) platform troubleshooting knowledge. Broadening the SearchKnowledge tool
  description was **insufficient** to change this; making the diagnosis lane retrieve-first
  needs a forceful directive or deterministic auto-seed + broad eval, and there is a real
  design tension (clarify-first is defensible for instance-specific symptoms).

**Net:** the relevance floor makes agentic SearchKnowledge SAFE whenever enabled (no
false-grounding). But default-on at the prod default still has no positive value — tool-ops
get honest "no KB", platform symptoms clarify without grounding — so default-on stays
re-homed to the external-KB-on decision (which makes tool-ops grounded) AND/OR a future
diagnosis-SK-first change (which would make platform symptoms grounded).

## Observability caveat (honest)
A turn may call SearchKnowledge multiple times, but the trace recorder keeps only the
LAST `rec.retrieval` block (cmd/trace.go overwrite). So agent-lane retrieval is now
observable but records only the final retrieval per turn — sufficient for this gate,
not a complete multi-call audit. Recording each call is a follow-up.

## Reproduce
```
# Evidence A (external on): eval/agentic_rag_probe.ps1 -ProbesPath eval/trace_gate/after_state_p5_probes.json -Runs 5 -External 1 -AgenticSearch 1
# Evidence B (prod default): eval/agentic_rag_probe.ps1 -ProbesPath eval/trace_gate/extoff_smoke_probes.json -Runs 3 -External 0 -AgenticSearch 1
```
