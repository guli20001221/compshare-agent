# Context Unification Status

Last audited against `origin/main` at `4da4a76f`.

## Current State

The context continuation layer is enabled by default through `COMPSHARE_CONTEXT_CONTINUATION`.
It is a shared decision layer: the model decides whether a short follow-up continues a prior task,
starts a new task, selects an entity, answers a follow-up, clears context, or needs clarification.
The backend still validates every resource, parameter, price, zone, image, and confirmation.

## Unified Paths

These paths now use the shared context decision and frame model:

- Create/deploy continuation:
  - change zone
  - change GPU
  - change image preference/source
  - fill deploy model size
- Workflow-task continuation through structured missing slots:
  - `CreateDiskWorkflow`
  - `ResizeDiskWorkflow`
  - `ResizeInstanceWorkflow`
  - `SetStopSchedulerWorkflow`
  - `ReinstallInstanceWorkflow`
  - `CreateCFSWorkflow`
  - `ResizeCFSWorkflow`
  - `EnableNetOptimizerWorkflow`
  - `CreateCustomImageWorkflow`
  - `RenameInstanceWorkflow`
- Instance-list selection:
  - `PendingSelection*` remains the candidate store.
  - The context decision layer can resolve user follow-ups such as selecting an item or referring to it.
- Recent fact follow-ups when `USE_SESSION_FACT_CONTEXT=1`:
  - stock follow-up
  - price follow-up
  - refund/billing follow-up

## Compatibility State

Some legacy session fields remain intentionally:

- `LastStockGpuModel` is the fallback stock referent when session facts are disabled.
- `LastDeployWorkload`, `LastDeployZone`, and `PendingDeployModel` remain for rollback and legacy compatibility when context continuation is disabled.
- `PendingSelection*` is not legacy behavior by itself; it is the persisted candidate list used by both old and new selection paths.

Do not remove these fields until the corresponding rollback path is deliberately retired.

## Remaining Pre-Router Gates

The following pre-router paths still exist and should not be deleted blindly:

- account-level billing refusal
- pending resource selection recovery
- monitor-history time-window checks
- diagnosis target continuation
- direct diagnosis for explicit targets
- scheduled shutdown direct dispatch
- lifecycle direct dispatch

These are partly safety gates and partly older UX shortcuts. Any migration must first prove the new context layer preserves:

- no write operation without a confirmation card
- no model-selected instance for mutating workflows
- explicit user target, previously user-selected target, or unique-account-instance target only
- clarification instead of confirmation when target or required parameters are uncertain

## Next Migration Candidates

1. Move diagnosis target continuation into the context decision layer.
2. Move monitor-history continuation and missing time-window clarification behind the same context framing model.
3. Review lifecycle and scheduled-shutdown direct dispatch after router and context-decision coverage proves parity.
4. Retire compatibility fields only after their feature flags and rollback behavior are no longer needed.

Each candidate needs focused unit tests, HTTP/WS replay, and a review of write-operation target trust before old code is removed.
