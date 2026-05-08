# T-004b entity registry runtime smoke

Run date: 2026-05-09

Model: `deepseek-v4-flash`

Base URL: `https://api.modelverse.cn/v1`

Config:

- `COMPSHARE_TRACE_ENABLED=1`
- `COMPSHARE_TRACE_DIR=<temp-dir>`
- Runtime config was a temporary copy of `deploy/conf/agent.yaml.example` with `model: "deepseek-v4-flash"`.
- Secrets were loaded from a local `.env.local` through `scripts/load_env.ps1`.

Command shape:

```powershell
.\scripts\load_env.ps1 -Path <local-env-file>
$env:COMPSHARE_TRACE_ENABLED = "1"
$env:COMPSHARE_TRACE_DIR = "<temp-dir>\trace"
<sanitized resource-basic smoke inputs> | go run .\cmd cli -c <temp-dir>\agent.yaml
```

Scope:

- Ran one real-account CLI smoke with two resource-basic user turns plus `exit`.
- The run exercised the startup `Engine.Init` registry refresh path and wrote one trace line per user turn.
- Raw CLI transcript, raw JSONL trace, temporary `agent.yaml`, and `.env.local` were kept in a local temp directory and are not committed.

Trace checks:

| Check | Result |
| --- | --- |
| JSONL trace file exists | PASS |
| Trace lines | 2 |
| User turns | 2 |
| `schema_version == "trace.v0.1"` for all lines | PASS |
| Observed `entity_registry.sync_event` values | `init` |
| `entity_registry.snapshot_id` non-empty on all lines | PASS (2/2) |
| Unique `entity_registry.snapshot_id` count | 1 |
| `snapshot_id` stable across unchanged turns | PASS |
| `entity_registry.age_seconds` values | `8, 13` |
| `age_seconds` increases across turns | PASS |
| Tool calls during user turns | none |
| Raw `COMPSHARE_PUBLIC_KEY` / `COMPSHARE_PRIVATE_KEY` / `LLM_API_KEY` present in trace | PASS (0 hits) |
| Raw sampled user prompts present in trace | PASS (0 hits) |
| IPv4-looking raw values present in trace | PASS (0 hits) |
| Sensitive string markers (`Bearer`, `Password`, `Jupyter`, `PublicKey`, `PrivateKey`, `ChargeAmount`, `Balance`) present in trace | PASS (0 hits) |
| CLI stderr bytes | 0 |
| CLI transcript error patterns (`错误:`, `API 调用失败`, `Invalid param`, `panic`, `fatal`) | PASS (0 hits) |
| Artifact secret scan | PASS (`scripts/secret_scan.ps1`) |

Notes:

- The smoke intentionally records only aggregate trace properties and sanitized command shape, not raw trace lines, raw tool arguments, raw tool results, raw user text, instance identifiers, IP addresses, or credentials.
- No user-turn tool calls were needed for this T-004b check. The acceptance target is the registry state populated from the startup refresh and exposed in each turn trace.
- The stable `snapshot_id` plus increasing `age_seconds` confirms that freshness is represented separately from inventory identity, as required by the T-004b ticket.
