# Routing-accuracy eval set (handling-class labels, from 637 real sessions)

This directory holds the **production-representative, handling-class-labeled** set
that backed the routing-accuracy gate (`TestOnlineRoutingHandlingEval`, P0 阶段0 §③).
That gate and its `eval/intent/` package were removed with the P6 router deletion;
the label data is retained here for the planned central-Agent HTTP/WebSocket replay
gate that will replace it.

Full provenance + methodology: `docs/research/routing_eval_set_637_2026-06-22.md`.

## Committed files

| file | what |
|---|---|
| `routing_eval_cases.jsonl` | **The gate input.** 173 balanced cases (≤25 per handling_class), each self-contained: `sid`, `handling_class`, `fine_intent`, `multi_handling`, `in_capability`, `expected_observable`, `user_msgs` (all user turns), `last_user_text` (the routed turn). |
| `routing_eval_set.jsonl` | The authoritative 173 labels (sid + handling_class + fine_intent + axisB + confidence) the cases file was joined against. |

## How it was built

1. The `route-label-637` workflow (20 parallel sonnet labelers + 1 independent
   blind opus recheck) labeled **all 637 real sessions** with an
   `expected handling_class` (the MECE-stable downstream-handling axis).
   Inter-labeler agreement: handling_class **κ = 0.79**, fine_intent exact **90%**
   → silver labels (see the design doc §1).
2. `routing_eval_set.jsonl` = the balanced subset (≤25 per class; low-frequency
   classes fully included).
3. `routing_eval_cases.jsonl` = `routing_eval_set.jsonl` ⋈ the per-session
   `user_msgs` (from the labeling batches), joined on `sid`. `last_user_text` =
   the **last** user turn, which is the turn the gate routes (see below). All 173
   sids joined with non-empty `user_msgs` (0 missing).

The upstream `batches/` + `labels_full.jsonl` (full 637) are kept as untracked
local artifacts; the committed `routing_eval_cases.jsonl` is the durable input.

## handling_class schema (8 classes)

`read_query` (只读 API) · `knowledge_answer` (KB 检索回答) · `lifecycle_mutate`
(确认门 saga) · `diagnosis` (诊断流程) · `refuse_out_of_scope` (诚实拒答) ·
`greeting_smalltalk` (问候) · `create_deploy` (创建确认 saga) · `ambiguous` (澄清).

## Why the LAST user turn

`handling_class` is a **session-level** label; the router classifies **per turn**.
108/173 sessions are single-turn (unambiguous). For multi-turn sessions the gate
routes the last user turn — the simplest deterministic rule, with fewer failure
modes than first-turn (greeting-prefixed sessions are common; "你好" as the first
turn would misroute, whereas the last turn carries the substantive intent). The
known artifact: slot-fill create sessions ("为我创建一台 v100S" → "1卡，按量…") put the
create intent in turn 0, so the last-turn fragment may under-route — visible in
the confusion matrix, curated as the floor is raised.

## Privacy

Source sessions were PII-redacted upstream (instance IDs → `[INSTANCE_ID]`, IPs →
`[IP]`). A pre-commit scan of `routing_eval_cases.jsonl` found no IPs, passwords,
API keys, or DSNs (the only long-hex strings are public Ollama model-blob
SHA256s). Consistent with the behavioral-gate precedent of committing redacted
real user text (`eval/realism/http_failure_replay_main_20260616_all.jsonl`).
