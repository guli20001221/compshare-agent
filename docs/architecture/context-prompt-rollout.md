# Context and Prompt Optimization Rollout

Date: 2026-06-03

This document records the rollout gates for the console-agent context and planner-prompt optimization work. Runtime behavior stays default-off unless stated otherwise.

## Flags

| Flag | Default | Scope | Notes |
|---|---:|---|---|
| `USE_SESSION_FACT_CONTEXT=1` | off | HTTP hydrated sessions | Reads same-session `RecentFacts` into an advisory 30s prompt block. CLI does not hydrate session state, so CLI is not an end-to-end proof for this flag. |
| `USE_REACT_RESULT_PROJECTION=1` | off | CLI + HTTP ReAct | Shrinks selected bulky read-only tool results before they enter model-visible history. |
| `USE_REACT_HISTORY_COMPACTION=1` | off | CLI + HTTP ReAct | Replaces count-only long-history trimming with deterministic summary and old retrievable-tool placeholders. |
| `USE_PLANNER_MINIMAL_CORE=1` | off | reserved | Compiler tests exist, but live model parsing remains full `Plan` until a future prompt contract change. |

## Local Gates

Run unit and trace gates:

```powershell
$env:COMPSHARE_PROJECT_ID='test-project'
go test ./eval/trace_gate ./internal/observability -count=1
go test ./internal/engine ./internal/intent ./internal/httpapi ./cmd -count=1
go test ./... -count=1
```

Run real CLI regression for ReAct performance flags:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\context_prompt_cli_regression.ps1 `
  -Tag context-prompt-rollout `
  -EnableProjection `
  -EnableHistoryCompaction `
  -Mutating 1
```

The script writes a `summary.json` under `%TEMP%\compshare-<tag>-cli-*` and fails on:

- planner `schema_valid=false`
- non-zero escaped hallucination count
- intent drift on labeled cases
- forbidden action or forbidden intent hits
- missing planner trace

## Rollout Order

| Environment | Enabled |
|---|---|
| local default | trace gate only |
| local smoke | `USE_REACT_RESULT_PROJECTION=1`, then add `USE_REACT_HISTORY_COMPACTION=1` |
| staging | `USE_SESSION_FACT_CONTEXT=1` for HTTP session flow, then P2/P3 flags one at a time |
| limited production | one flag at a time, with trace gate and smoke comparison |
| full production | only after trace gate and CLI/HTTP smoke remain green |

## Stop Conditions

Stop rollout and turn the newest flag off if any condition appears:

- `schema_valid` rate drops
- diagnosis anchor like issue #123 routes to `knowledge_qa`
- escaped hallucination count becomes non-zero
- monitor follow-up treats cached facts as real-time truth
- projection removes SSH login command, disk details, GPU type/count, or API error evidence needed for diagnosis
- long conversation loses selected instance or operation context
- provider returns output-mode or JSON-format 400 errors

## Prompt SHA Note

Planner prompt SHA is intentionally re-pinned in P5 after adding XML-like example delimiters. The example JSON payloads, route order, intent enum, and required-tool allowlists are unchanged by that prompt hygiene step.
