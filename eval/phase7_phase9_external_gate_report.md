# Phase 7 / Phase 9 External Gate Report

Date: 2026-06-03
Backend branch: `codex/diagnosis-routing-optimization`
Backend head checked: `7a6e3c9`

This report records the remaining external gates after the diagnosis/routing work.
It is intentionally de-identified: no live credentials and no raw instance IDs are included.

## Phase 7: write saga status

### Current status

`CreateCustomImageWorkflow` is already implemented in the backend and registered as a workflow tool.

The workflow path is:

1. `DescribeCompShareInstance`
2. user confirmation
3. `CreateCompShareCustomImage`
4. optional `GetCompShareImageCreateProgress`

The raw mutating API `CreateCompShareCustomImage` is not exposed as the user-facing action.
The workflow asks for one confirmation before the mutating call.
Destructive actions remain hard-refused.

### API shape verification

Upstream `CreateCompShareCustomImageRequest` contains:

- `UHostId`
- `Name`
- `Description`
- optional `Softwares`
- optional `SoftwarePorts`
- optional `FirewallPorts`

It embeds `BaseRequest`, which contains `user_email`.

The workflow currently passes only the minimum v1 fields:

- `UHostId`
- `Name`
- optional `Description`

It does not pass `Region`, `Zone`, `Softwares`, `SoftwarePorts`, or `FirewallPorts`.
Unit tests pin this behavior.

### Live evidence

The previously committed live smoke report `eval/pr217_live_smoke_report.md` shows:

- deny leg: confirmation denied, no `CreateCompShareCustomImage` call
- destructive leg: hard refusal, zero tool calls
- approve leg: reached real `CreateCompShareCustomImage`, then upstream returned `RetCode=210 Missing params [user_email]`
- orphan check: no matching custom image was left behind

Therefore the backend workflow is not waiting on instance cleanup.
Test-created instances do not need to be deleted after the run.
The remaining approve-leg blocker is gateway/user identity propagation for `user_email`.

### Phase 7 conclusion

Do not build publish/update image workflows until the gateway/user_email behavior is clarified, because those upstream requests also embed `BaseRequest` and may need the same identity field.

After gateway clarification, rerun the custom-image approve smoke with mutating tools enabled and verify:

- confirmation appears before create
- `CreateCompShareCustomImage` succeeds without `RetCode=210 Missing params [user_email]`
- progress check runs when an image id is returned
- listing custom images can observe the new image or expected in-progress state
- destructive/delete-class actions remain hard-refused

## Phase 9: frontend / console integration status

### Backend HTTP/SSE gate

Backend HTTP tests now pin the important console-facing behavior:

- SSE step events are emitted before `done`
- step events omit API args and tool result payloads
- `done.Content` carries the final answer
- persisted assistant content does not expose access tokens
- HTTP traces record `actual_runtime_form=agent` for ReAct/tool-call paths

Focused backend verification passed on this branch:

```powershell
go test ./internal/httpapi -count=1
```

### Frontend repo check

Checked frontend repo:

- path: `F:\frontend\frame`
- branch: `feature/console-ai-step-envelope`
- head: `06d2e4f`
- tracked status: clean

The current `src/Frame/AIAssistant/service.js` is explicitly marked local-only:

- hardcoded gateway: `http://127.0.0.1:8080`
- hardcoded org ids
- hardcoded project id
- comment says "local-only" and "forbidden for production"

The frontend can consume the current backend shape for local testing:

- `event: step`
- `event: confirmation`
- `event: token`
- `event: done`
- `ConfirmCSAgentAction`
- `done.Content`

But this is not production-ready.

Production integration still needs:

- replace local hardcoded gateway/org/project fields with production gateway/project context
- keep `ConfirmCSAgentAction`
- keep `onStep`, `onConfirmation`, and `done.Content` handling
- remove debug console logs around confirmation events
- add friendly labels for new actions such as `CreateCustomImageWorkflow`, `CheckCompShareNetOptimizer`, image-tag/model-repo/shared-image reads
- run a browser smoke against the real console build after the production service path is restored

### Phase 9 conclusion

Backend Phase 9 gates are pinned by tests.
Frontend Phase 9 is only locally viable today.
It should not be treated as production console integration until the production gateway path is restored and a browser smoke passes.
