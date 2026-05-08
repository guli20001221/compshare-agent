# T-007b shadow runner smoke

Run date: 2026-05-09

Model: `deepseek-v4-flash`

Base URL: `https://api.modelverse.cn/v1`

Config:

- `COMPSHARE_TRACE_ENABLED=1`
- `USE_INTENT_PLANNER=shadow`
- `COMPSHARE_TRACE_DIR=<temp-dir>`
- Runtime config was a temporary copy of `deploy/conf/agent.yaml.example` with `model: "deepseek-v4-flash"`.
- Secrets were loaded from a local `.env.local` through `scripts/load_env.ps1`.

Command shape:

```powershell
.\scripts\load_env.ps1 -Path <local-env-file>
$env:COMPSHARE_TRACE_ENABLED = "1"
$env:USE_INTENT_PLANNER = "shadow"
$env:COMPSHARE_TRACE_DIR = "<temp-dir>\trace_cases_name"
<sanitized T-007b shadow smoke inputs> | <temp-built-agent.exe> cli -c <temp-dir>\agent.yaml
python scripts\planner_vs_guard_diff.py <temp-trace-jsonl> --output <temp-dashboard-report>
```

Scope:

- Ran four real-account CLI sessions with two user turns each plus `exit`.
- Three sessions replayed the PR #12-style monitor follow-up shapes as independent two-turn conversations.
- One control session covered account-level billing hard-block and resource listing.
- The target was selected from the real account at run time and represented only as `<target>` in this artifact.
- Raw CLI transcripts, raw JSONL trace, temporary inputs, temporary binary, temporary `agent.yaml`, and `.env.local` were kept in a local temp directory and are not committed.

Trace checks:

| Check | Result |
| --- | --- |
| JSONL trace file exists | PASS |
| Trace lines | 8 |
| User turns | 8 |
| `schema_version == "trace.v0.1"` for all lines | PASS |
| Planner-enabled turns | 8 |
| Planner schema-valid turns | 7/8 (`87.50%`) |
| Invalid/fallback planner turns reported by dashboard | 1 |
| Planner rate-limit hook action | `shadow_planner` on 8/8 turns |
| Planner rate-limit denials | 0 |
| Observed planner intents | `monitor_query:5`, `resource_info:1`, `unknown:2` |
| Observed `entity_registry.sync_event` values | `init` |
| `entity_registry.snapshot_id` non-empty on all lines | PASS (8/8) |
| Unique `entity_registry.snapshot_id` count | 1 |
| `entity_registry.age_seconds` range | `0..54` |
| Engine hard-block count | 1 |
| Tool call sources observed | `main_react`, `knowledge_local` |
| Raw `COMPSHARE_PUBLIC_KEY` / `COMPSHARE_PRIVATE_KEY` / `LLM_API_KEY` present in trace or dashboard report | PASS (0 hits) |
| Raw sampled user prompts present in trace | PASS (0 hits) |
| Raw UHostId-looking values present in trace | PASS (0 hits) |
| IPv4-looking raw values present in trace | PASS (0 hits) |
| Sensitive string markers (`Bearer`, `Password`, `Jupyter`, `PublicKey`, `PrivateKey`, `ChargeAmount`, `Balance`) present in trace | PASS (0 hits) |
| CLI stderr bytes across all four sessions | 0 |

Per-turn trace summary:

| Row | Session shape | Turn | Planner intent | Schema valid | Tool calls | `monitor_call_in_current_turn` | Engine hard-block |
| --- | --- | ---: | --- | --- | --- | --- | --- |
| 1 | PR #12 same-metric | 1 | `monitor_query` | true | `DescribeCompShareInstance`, `GetCompShareInstanceMonitor` | true | false |
| 2 | PR #12 same-metric | 2 | `monitor_query` | true | `GetCompShareInstanceMonitor` | true | false |
| 3 | PR #12 explicit-refresh | 1 | `monitor_query` | true | none | false | false |
| 4 | PR #12 explicit-refresh | 2 | `monitor_query` | true | `DescribeCompShareInstance`, `GetCompShareInstanceMonitor` | true | false |
| 5 | PR #12 pronoun-now | 1 | `unknown` | false | none | false | false |
| 6 | PR #12 pronoun-now | 2 | `monitor_query` | true | `DescribeCompShareInstance`, `GetCompShareInstanceMonitor` | true | false |
| 7 | account billing hard-block | 1 | `unknown` | true | none | false | true |
| 8 | resource listing control | 2 | `resource_info` | true | `DescribeCompShareInstance` | false | false |

PR #12 monitor follow-up representation:

| Case | Second-turn planner intent | Second-turn current monitor call | Dashboard classification |
| --- | --- | --- | --- |
| `adjacent_same_metric` | `monitor_query` | true | represented; no freshness miss on follow-up |
| `adjacent_explicit_refresh` | `monitor_query` | true | represented; no freshness miss on follow-up |
| `adjacent_pronoun_now` | `monitor_query` | true | represented; no freshness miss on follow-up |

Dashboard summary excerpt:

| Metric | Value |
| --- | ---: |
| Total turns | 8 |
| Planner-enabled turns | 8 |
| Schema-valid rate | 87.50% |
| Invalid/fallback planner turns | 1 |
| Engine hard-block count | 1 |
| Monitor freshness misses | 1 |

Intent distribution:

| Intent | Count |
| --- | ---: |
| `monitor_query` | 5 |
| `resource_info` | 1 |
| `unknown` | 2 |

Account hard-block agreement:

| Outcome | Count |
| --- | ---: |
| matched | 0 |
| mismatched | 0 |
| engine_only | 1 |
| not_applicable | 7 |

Notes:

- The first run used an explicit user-provided instance ID as `<target>`. It produced a valid trace but increased planner `unknown` outcomes, so the final smoke above used the instance name path, which better represents normal user phrasing while still keeping the target redacted from artifacts.
- The dashboard reported one `monitor_freshness_miss` on the first turn of the explicit-refresh session, not on any of the three PR #12 second-turn follow-ups. This is acceptable for T-007b because the dashboard is observability-only and correctly marks the missing current-turn monitor call.
- Planner slots did not include explicit metric values in this smoke even when the planner classified `monitor_query`. This is a shadow-quality signal for later planner prompt/schema tuning; it did not affect the current ReAct user path.
- The account-level billing hard-block was engine-only in this smoke: the deterministic hard-block fired, while the shadow planner emitted `unknown`. This is trace-visible and should be monitored by the planner-vs-runtime dashboard.
- This artifact intentionally records only aggregate trace properties and sanitized tool names, not raw trace lines, raw tool arguments, raw tool results, raw user text, instance identifiers, IP addresses, transcripts, or credentials.
