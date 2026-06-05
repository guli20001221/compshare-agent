# Stage 2B curated FAQ smoke

Date: 2026-05-09

Model: `deepseek-v4-flash`

Purpose: verify the opt-in curated FAQ retrieval path after Stage 2B Commit 4
engine wiring.

## Runtime

- Config source: `deploy/conf/agent.yaml.example`, copied to a temp path with
  model set to `deepseek-v4-flash`.
- Secrets: loaded from local `eval/shadow_qa/.env.local` through
  `scripts/load_env.ps1`.
- Trace: enabled with `COMPSHARE_TRACE_ENABLED=1`.
- Retrieval gate: `USE_KNOWLEDGE_RETRIEVAL=curated`.
- Corpus: bundled `deploy/kb/curated_faq.jsonl`.
- Raw trace, stdout, stderr, temp config, and temp build artifacts stayed under
  local temp directories and are not committed.

## Sanitized Inputs

Main smoke, 5 user turns:

1. image-type FAQ question
2. SSH / JupyterLab login FAQ question
3. stopped-instance billing-rule FAQ question
4. intentionally out-of-corpus GPU capability wording
5. account-balance hard-block question

Miss smoke, 1 user turn:

1. enterprise legal / refund-policy FAQ question outside the bundled corpus

## Trace Summary

Main smoke:

| Turn | Expected path | Observed trace result |
| --- | --- | --- |
| 1 | image FAQ retrieval hit | `planner.intent=knowledge_qa`, `schema_valid=true`, `confidence=0.82`, `cutover_status=dispatched_retrieval`, `retrieval.enabled=true`, `kb_version=kb.curated.2026-05-09`, `hits=1`, no API tool call |
| 2 | login FAQ retrieval hit | `planner.intent=knowledge_qa`, `schema_valid=true`, `confidence=0.82`, `cutover_status=dispatched_retrieval`, `retrieval.enabled=true`, `kb_version=kb.curated.2026-05-09`, `hits=1`, no API tool call |
| 3 | billing-rule FAQ retrieval hit | `planner.intent=knowledge_qa`, `schema_valid=true`, `confidence=0.82`, `cutover_status=dispatched_retrieval`, `retrieval.enabled=true`, `kb_version=kb.curated.2026-05-09`, `hits=1`, no API tool call |
| 4 | out-of-corpus wording probe | `planner.intent=knowledge_qa`, `schema_valid=true`, `confidence=0.82`, `cutover_status=dispatched_retrieval`, `retrieval.enabled=true`, `kb_version=kb.curated.2026-05-09`, `hits=1`, no API tool call |
| 5 | account hard-block | `engine_hard_block.hit=true`, `category=account_billing_unsupported`, `retrieval.enabled=false`, no API tool call |

Miss smoke:

| Turn | Expected path | Observed trace result |
| --- | --- | --- |
| 1 | curated FAQ miss | `planner.intent=knowledge_qa`, `schema_valid=true`, `confidence=0.82`, `cutover_status=fallback_retrieval_miss`, `retrieval.enabled=true`, `kb_version=kb.curated.2026-05-09`, `hits=0`, no API tool call |

## Safety Checks

- Main smoke trace line count: 5 user turns.
- Miss smoke trace line count: 1 user turn.
- Billing-rule answer currency amount pattern
  `\d+(\.\d+)?\s*(元|RMB|USD)`: false.
- Billing-rule answer includes console/API authority phrasing: true.
- Account hard-block bypassed retrieval and planner handling: true.
- Raw trace and raw transcript were not committed.
- Stderr bytes: 0 for both runs.

## Notes

- The intentionally out-of-corpus GPU capability wording still produced a
  retrieval hit. This is a recall/precision signal for the bundled starter
  corpus and deterministic keyword inference, not a safety failure: the answer
  still came from a curated `customer_safe` chunk and did not call APIs.
- Paraphrased in-corpus queries were not separately probed in this smoke.
  Expand the matrix if real users report unexpected misses on known FAQ topics,
  because the first-slice retriever uses deterministic substring matching rather
  than semantic retrieval.
- The separate miss smoke used a legal/refund-policy FAQ category outside the
  bundled corpus to verify `fallback_retrieval_miss`.
- The smoke exercises only `knowledge_qa` retrieval and account hard-block.
  Mixed `mixed_diagnosis_kb` / `mixed_billing_kb` classification remains
  observability-only and is not handled by Stage 2B Commit 4.
