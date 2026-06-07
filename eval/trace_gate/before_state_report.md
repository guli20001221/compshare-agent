# Agentic-RAG mis-routing — BEFORE-state baseline (unified plan P2)

Captured 2026-06-06 against the exec branch (routing byte-identical to
`origin/main` `293f944`), `COMPSHARE_EXTERNAL_KNOWLEDGE=1`, `ds-v4-flash`,
qwen3 RRF retrieval. Runner: `eval/agentic_rag_probe.ps1`, **N=5 per probe**.
Raw observations: `before_state_observations.jsonl` (25 rows). Pinned by
`before_state_test.go::TestMisroutingBeforeState`.

## Finding — the gap reproduced deterministically (zero jitter)

| probe set | example | intent | actual_runtime_form | retrieval-fired | reply |
|---|---|---|---|---|---|
| symptom (15/15) | `vllm 进程被 kill` / `sglang 启动报显存不足` / `vllm serve 报 ValueError` | `diagnosis` | `""` (none) | **false** | canned `请问是哪台实例…` dead-end |
| how-to (10/10) | `vllm 怎么启动 openai 兼容服务` / `怎么降低 vllm 的显存占用` | `knowledge_qa` | `terminal_rag` | **true** | cited answer from external chunks |

All 5 runs of every probe were byte-identical on the `(intent, runtime_form, retrieval-fired)` triple.

## The two halves of the gap

1. **Mis-routing.** Symptom/error-phrased tool-ops Qs classify as `IntentDiagnosis`
   (agent form), not `knowledge_qa`. That is the right *class* (they ARE
   diagnosis-shaped) — the problem is what happens next.
2. **Pre-ReAct dead-end.** For `IntentDiagnosis` with empty `TargetRefs` and >1
   instance, `tryPlannerDiagnosisClarification` returns the canned
   `请问是哪台实例…` **before any RAG or ReAct tool runs**. So `retrieval-fired=false`
   and `ActualRuntimeForm=""` — the turn never reaches the agent loop. This
   refines the plan's expectation (which said the symptom form would be `agent`):
   the dead-end fires *before* the agent form is ever realized, so the form is
   unobservable `""`. The mis-route is therefore more severe than "agent with no
   RAG" — it is a canned reply with no agent execution at all.

## Why this is fixable (the answering evidence already exists)

The how-to probes prove the external corpus answers these topics and is
retrievable today: `怎么降低 vllm 的显存占用` cites `ext-gpu-oom-vllm-001` +
`ext-vllm-quantization-001`; `vllm 怎么启动 openai 兼容服务` cites
`ext-vllm-serving-001`. The symptom Qs simply never reach retrieval. Closing the
gap = make the agent lane call `SearchKnowledge` first (P3/P4a) and route symptom
Qs there past the relaxed dead-end (P4b).

## Corpus-coverage precheck (gates P3's substance gate)

`TestBeforeStateSymptomProbesCorpusCovered` asserts each symptom probe has >=1
answer-bearing external chunk, so a later P3 NO-GO is attributable to
architecture, not a corpus gap:
- `sym-vllm-killed` -> `ext-gpu-kill-process-001` / `ext-vllm-startup-hang-001` / `ext-gpu-oom-vllm-001`
- `sym-sglang-oom` -> `ext-sglang-oom-001`
- `sym-vllm-valueerr` -> `ext-vllm-cuda-error-001` / `ext-vllm-startup-hang-001`

The corpus-uncovered `sglang 端口连不上` probe was deliberately excluded (its only
near-match `ext-vllm-port-001` is vLLM-only); author `ext-sglang-port-*` before
reintroducing a port-specific symptom probe.

## How later phases use this baseline

After P3+P4a+P4b land (flag-on), re-run `eval/agentic_rag_probe.ps1` with
`-AgenticSearch 1` and capture an after-state. GATE A passes when the symptom set
flips to: `SearchKnowledge` tool-call **before** any `Diagnose*`,
retrieval-fired=**true** on external chunks, a substantive cited answer, no
which-instance dead-end — while genuine platform-instance symptoms still clarify.
