# knowledge_qa terminal-RAG → agent-loop A/B (Phase 2 gate)

**Date:** 2026-06-08 · **Build:** `claude/kqa-agent-loop` off `origin/main a73ac05` (PR #235 + #250 merged) · **Model:** deepseek-v4-flash · **Judge:** claude-opus-4-7 (ModelVerse) over the **actual CLI replies**.

**Verdict: GATE NOT MET — do NOT flip `COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP` default-on.** The agent-loop route works, routes and grounds correctly, and its *substantive* answers are as faithful as terminal — but it **false-refuses ~44% of covered knowledge_qa turns** on flash where terminal refuses 0%. The mechanism lands default-off (reversible, byte-identical) as the foundation; the flip is blocked pending the cite-reliability fixes below.

## Setup
- A = flag off (terminal RAG, `tryStage2BRetrieval`); B = flag on (forced-`SearchKnowledge` agent loop). Both: `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE=1`, `COMPSHARE_EXTERNAL_KNOWLEDGE=1`, same 8-probe set (external vLLM/PyTorch/ComfyUI/Linux + platform error-code + CUDA + a billing anti-contamination control), N=2 each ⇒ 16 runs/condition.
- Runner: `eval/agentic_rag_probe.ps1`; analysis/judge: `eval/kqa_ab_analyze.py`; raw: `kqa_agent_loop_ab_report.json`.

## Results

| metric | A (terminal) | B (agent-loop) | gate |
|---|---|---|---|
| intent = knowledge_qa | 15/16 (1 planner jitter→diagnosis, #123) | 16/16 | — |
| runtime form | terminal_rag 15 / agent 1 | **agent 16/16** | ✅ migration routing correct |
| retrieval fired | 1.00 | 0.88 | ⚠️ |
| expected-chunk coverage (top-K) | 8/8 | 7/8 | ⚠️ minor (model-query drift) |
| control contamination (billing) | 0 | 0 | ✅ no external bleed |
| **refusal rate** | **0.00** | **0.44** | ❌ **DECISIVE FAIL** |
| substantive-answer faithfulness (fab / judged) | 2/16 | 1/9 | ✅ B ≤ A (comparable) |
| token-budget canned | 0 | 1/16 | ⚠️ agent-loop heavier |

## Why B refuses 44% (7/16)
- **2/7 — forced-hop misfire:** `sk=False/retr=False`; flash occasionally ignored the forced `SearchKnowledge` object tool_choice and answered directly → round-0 cited-contract gate correctly refused (no fabrication). (tmux r1 [cold start], vllm r2.)
- **5/7 — cite-gate over-refusal:** `sk=True/retr=True` — SearchKnowledge fired and retrieved the right chunks, but flash did not emit valid `[[chunk_id]]` markers, so `guardSearchKnowledgeSynthesis` replaced a grounded answer with the canned refusal. (tmux r2, pytorch-ddp r1, platform-errcode r2, billing r1+r2.)

**Root cause = incomplete cite-or-refuse PARITY in the Phase-1 coupling:**
1. Terminal `answerWithRetrievedEvidence` (engine.go:2187) does **one retry** with a stronger cite instruction before refusing; the agent-loop `guardSearchKnowledgeSynthesis` is **one-shot** (refuse immediately). No second chance.
2. Terminal validates **numbered `[1]`** citations (flash emits these reliably in the dedicated RAG prompt); the agent-loop requires **`[[chunk_id]]`** (flash must echo long opaque IDs verbatim — far less reliable).
3. The agent-loop spends more tokens (full ReAct system prompt + tool round-trip + evidence echoed into context); 1/16 hit the per-turn cap (`token_budget`).

The migration itself is sound: 16/16 correct routing, faithful where it answers, no contamination, retrieval lands the expected chunk. The blocker is purely that flash cannot drive the `[[chunk_id]]` cite protocol reliably enough one-shot.

## Recommendation (Phase-3 prerequisites — do before any default flip)
1. **Cite-retry parity:** on an uncited agent-loop synthesis, re-prompt once with an explicit cite reminder before falling back to the refusal (mirror engine.go:2187), instead of one-shot refusing.
2. **Citation format:** accept numbered `[n]` citations mapped to the SearchKnowledge ledger order (what flash emits reliably), not only `[[chunk_id]]`.
3. **Forced-hop reliability:** characterize/retry the ~12% rounds where flash ignored the forced `SearchKnowledge` tool_choice.
4. **Token headroom:** give the knowledge_qa-agent-loop turn budget margin (or a leaner agent prompt) so synthesis doesn't trip the per-turn cap.
5. Re-run this A/B (N≥5) after (1)–(4); flip only when B refusal-rate ≤ A and faithfulness ≤ A hold.

**Disposition:** Phase 1 (mechanism) + Phase 2 (harness + this eval) land **default-off** — a reversible, byte-identical foundation plus the negative gate result as the repo's decision record. `COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP` stays off until the above are fixed and re-eval'd.
