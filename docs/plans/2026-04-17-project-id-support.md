# ProjectId Support for Scheduler API

> Date: 2026-04-17
> Status: Implemented

## Problem

Shadow QA (2026-04-16) exposed `SetStopSchedulerWorkflow` failing on the real
account with:

```
Params [ProjectID] not available
```

`CancelStopSchedulerWorkflow` succeeded, so the defect is scoped to
`UpdateCompShareStopScheduler` — not a scheduler-wide issue.

## Root cause

Authoritative evidence from the upstream repo:

- `F:\uhost-compshare-api-master\internal\api\compshare\update_compshare_stop_scheduler.go`
  lines 67–69: `preDoWork` returns `ParamsError("ProjectID")` when
  `req.ProjectID == ""`.
- `F:\uhost-compshare-api-master\internal\api\compshare\delete_compshare_stop_scheduler.go`
  does not check `ProjectID` — explains why cancel worked.
- `F:\uhost-compshare-api-master\docs\api\scheduler\UpdateCompShareStopScheduler.md`
  marks `ProjectId` **required** and documents discovery via `GetProjectList`
  (`ProjectSet[].ProjectId`, prefer `IsDefault=true`).
- `F:\uhost-compshare-api-master\examples\phase4\main.go` line 42 hard-codes
  `projectId = "org-cwy2qk"` for the same account used in shadow QA.

Conclusion: `ProjectId` is an **account-level constant** required by some
write APIs. The agent never discovered nor injected it.

## Design

**Two-layer support for flexibility:**

1. **Config-first:** `agent.yaml` can pre-set `project_id`. Env override
   `COMPSHARE_PROJECT_ID` supported.
2. **Runtime discovery:** if config left it empty, `Engine.Init` calls
   `GetProjectList` once and caches the result on the `ExternalExecutor`.

**Auto-injection in ExternalExecutor.Execute:** on every request, if
`projectId != ""` and the caller didn't explicitly supply `ProjectId` in args,
add it to the signed form params. This means no workflow, diagnosis chain,
or direct API tool needs to know about `ProjectId` — the executor handles it
transparently.

Explicit caller-supplied `ProjectId` always wins, preserving the option for
future multi-project flows.

## Files changed

| File | Change |
|------|--------|
| `internal/config/config.go` | `AgentConfig.ProjectId` field + `COMPSHARE_PROJECT_ID` env override |
| `deploy/conf/agent.yaml.example` | Document the new field |
| `internal/tools/external.go` | `projectId` field on `ExternalExecutor`, `SetProjectId`/`ProjectId` methods, auto-injection in `Execute` |
| `internal/engine/engine.go` | `ensureProjectId` + `unwrapExternalExecutor` + `pickProjectId`; called first in `Init` |
| `CLAUDE.md` / `AGENTS.md` | Upstream API reference paths (so future sessions don't re-explore) |

## Tests

| File | Test | Verifies |
|------|------|----------|
| `internal/tools/external_test.go` | `TestExternalExecutor_ProjectIdFromConfig` | NewExternalExecutor reads config value |
| | `TestExternalExecutor_SetProjectId` | Runtime setter works |
| | `TestExternalExecutor_AutoInjectsProjectId` | Configured ProjectId shows in signed form |
| | `TestExternalExecutor_ExplicitProjectIdOverridesConfig` | Caller-supplied value wins |
| | `TestExternalExecutor_NoProjectIdWhenUnset` | Unset → no injection |
| `internal/engine/engine_test.go` | `TestEnsureProjectId_UsesConfigWhenSet` | GetProjectList NOT called when config has value |
| | `TestEnsureProjectId_FetchesWhenUnset_PicksDefault` | IsDefault=true wins |
| | `TestEnsureProjectId_FallsBackToFirstWhenNoDefault` | First entry when no default marker |
| | `TestEnsureProjectId_SilentOnMalformed` | Empty ProjectSet → Init still succeeds |
| | `TestEnsureProjectId_SkipsForMockExecutor` | Non-ExternalExecutor path is a no-op |
| | `TestPickProjectId` (7 subtests) | All extraction edge cases |

All pass. Zero regressions across `go test ./...`.

## Out of scope for this change

- Multi-project accounts: current design picks exactly one ProjectId at Init
  time. If workflows ever need to target specific projects, they can pass
  `ProjectId` in args explicitly — auto-injection respects that.
- `ProjectId` as a tool-level schema field: intentionally NOT added to the
  tool registry. `ProjectId` is infrastructure, not user-facing; surfacing it
  to the LLM would invite prompt injection and confusion.

## Real-account verification (next step)

Once merged, re-run the shadow QA scheduler case
(`shadow_06_set_scheduler`). Expected: the live `UpdateCompShareStopScheduler`
call now succeeds; the failure-point in `summary.md` §failure-points#3 closes.
