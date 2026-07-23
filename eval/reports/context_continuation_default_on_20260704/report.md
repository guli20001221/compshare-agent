# Context Continuation Default-On Validation

Date: 2026-07-04

## Code Under Test

- Agent branch: `codex/context-continuation-default-on`
- Agent base commit: `f7bf8a60`
- Frontend baseline: `feature/AIAssisant-update@42443ae`
- Local stack:
  - Backend: `http://127.0.0.1:7429`
  - WebSocket gateway: `ws://localhost:8090`
  - Frontend dev server: `http://localhost:3000`

## Runtime Flags

- `COMPSHARE_CONTEXT_CONTINUATION`: default-on in this branch; local backend log confirms `COMPSHARE_CONTEXT_CONTINUATION default-on`
- `USE_SESSION_FACT_CONTEXT=1`
- `COMPSHARE_UNIFIED_CREATE=1`
- `COMPSHARE_CREATE_PREF_EXTRACTOR=1`
- `COMPSHARE_ENABLE_MUTATING_TOOLS=1`
- `COMPSHARE_CONFIRM_FORM=1`
- `COMPSHARE_GUIDED_CREATE=1`

## Fixes Validated

1. Context continuation now tolerates numeric `slot_updates` from the model, e.g. `cpu: 4`, `memory_gb: 8`.
2. Create/deploy continuation can recover when the model emits an invented zone code such as `cn-north2-a`; the backend retries zone parsing against the original user text.
3. When the model decides `continue_task` but omits a slot, the backend can derive explicit GPU or zone mentions from the user text, then still validates them through the existing catalog/support-zone logic.

## WebSocket Behavioral Replay

All replay commands used the local WebSocket gateway and followed the backend `meta.SessionId` after the first turn.

| Scenario | Result | Evidence |
| --- | --- | --- |
| `在华北一C用 PyTorch 开 4090` -> `那华北二A呢` | 5/5 passed | All 5 runs reached `CreateInstanceWorkflow` confirmation. Sessions: `5c61675a-4b6d-4d42-9558-7000e847808c`, `1c7eb52a-250f-47d8-b865-3552a36e8df3`, `3ce57402-1223-41d8-98f8-690185383bd6`, `8370d1b3-c19f-488e-b1ca-9682e7fc64a1`, `0b98ee3d-8d72-4384-aed4-24d937909d31`. |
| `我有哪些实例` -> `选第3台` -> `给这台加一块数据盘` -> `200G` | 5/5 passed | All 5 runs reached `CreateDiskWorkflow` confirmation. Sessions: `149fe629-2146-4e87-ba8e-77bf182b59c6`, `104feae6-bdde-4939-8594-23a38c7ec7fa`, `68302087-f6eb-4d88-8d97-d3a7169f7d77`, `db4951e3-e2fc-4b9f-9d4b-c756e1d94bed`, `bf4931a5-1329-4607-acaa-b3da046c9323`. |
| No context: `200G` | 5/5 passed | No confirmation card. The sampled reply clarified the intended operation instead of starting a workflow. |
| No context: `4C8G` | 5/5 passed | No confirmation card. The sampled reply stayed read-only. |
| No context: `那5090呢` | 5/5 passed | No confirmation card. The sampled reply stayed a GPU-spec/status answer. |
| `我有哪些实例` -> `第1台 GPU 忙不忙` -> `帮我重启它` | 5/5 passed | No reselect prompt; all 5 runs reached `RebootInstanceWorkflow` confirmation. Sessions: `959092d7-be06-41c1-b8e1-5231599f4cdb`, `bfe8c597-c649-471e-86a5-2d9ef142f58a`, `a9a30d96-d16a-4fe8-a895-ae21b594a330`, `f488f31d-7757-4438-a4c7-42992d51a992`, `c8c5bd5c-d1cc-494c-a6cb-4ed9252da8fa`. |
| `我有哪些实例` -> `选第3台` -> `把这台系统盘扩一下` -> `200G` | Passed | Reached `ResizeDiskWorkflow` confirmation. Session: `c6dd6af1-3d69-45e4-9a61-fbf111e83bb8`. |
| `我有哪些实例` -> `选第3台` -> `把这台改配一下` -> `4C8G` | Passed safety gate | The context was resolved, then workflow validation stopped because the selected instance was running and resize requires shutdown first. Session: `fa92d5d9-5a59-4df2-a7d5-76774143ffad`. |

## Frontend Status

- The local frontend dev server listens on `:3000`.
- After SSO login, the browser may land on `https://console.compshare.cn/...`; reopening localhost is required.
- The usable local route is `http://localhost:3000/light-gpu/console/resources`.
- The `feature/AIAssisant-update@42443ae` frontend contains the `meta.SessionId` adoption code, and its unit test passed in earlier validation.
- This frontend branch still has the local WebSocket override block commented out in `src/Frame/AIAssistant/service.js`; therefore page-click chat against the local backend is not claimed in this report. Backend/WS behavior was validated through the local gateway.

## Automated Tests

- `go test ./internal/engine -run "TestParseContextDecisionSlotUpdatesAcceptNumericValues|ContextDecision|ApplyContextContinuationDecision|ResumeCreateContextFrame|WorkflowContextFrame" -count=1`: passed.
- `go test ./cmd -run "ContextContinuation|RuntimeOverlay" -count=1`: passed.
- `go test ./internal/engine ./internal/workflow ./internal/httpapi -count=1`: passed.
- `go test ./cmd ./internal/config -count=1`: passed.
