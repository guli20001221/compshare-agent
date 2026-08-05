# Context Unification Status

Last audited against `origin/main` at `a714b639`, plus the lifecycle/scheduler
ordinal resolution and `SelectedInstance` TTL changes described below.

## Current State

Context continuation is a fixed runtime capability and cannot be disabled by configuration.
It is a shared decision layer: the model decides whether the current turn continues a prior task,
starts a new task, selects an entity, answers a follow-up, clears context, or needs clarification.
The backend still validates every resource, parameter, price, zone, image, and confirmation.

## Resolution model: mutating is deterministic, read-only can be model-driven

Not every context path routes through the model decision layer, and that split
is **deliberate, not unfinished convergence**:

- **Read-only continuation** (diagnosis target, monitor-history, stock/price/
  billing facts) may resolve a referenced instance through the shared
  `resolveContextDecision` layer. If the model mis-resolves, the worst outcome
  is reading the wrong instance's state — recoverable.
- **Mutating target resolution** (lifecycle stop/start/reboot/reset-password/
  rename/resize, scheduled shutdown) resolves the instance **deterministically
  from the user's literal text** — explicit ID, unique name, a pronoun bound to
  a prior user selection, or an ordinal against the displayed candidate list.
  It must **never** resolve a mutating target from a model-supplied reference.

Why the asymmetry: routing mutating resolution through the model's free
reference resolution reintroduces the phantom-instance class of bug. The write
path therefore treats the model's target reference as advisory only: the server
re-derives, for every write target, a **dual proof** — a SelectionProof (the user
genuinely chose it: a current-turn literal id span, a card/ordinal pick, or the
account's sole fresh instance) AND an ExistenceProof (it exists in this account
this turn: a fresh-complete registry hit, a this-turn read, or a this-turn point
`DescribeCompShareInstance` whose response echoes the same id). An observed
referent is never a selection. This dual proof is the single write-target
authority (`internal/engine/target_evidence.go` + the resolver evidence
verifier); the former workflow-layer re-authorization center was deleted, so a
target the resolver never verified can no longer be laundered into execution.

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
  - Monitor-history follow-ups that include a displayed ordinal, such as
    "第 2 台昨天 00:00 到 01:00 的 CPU 历史监控", first use the context
    decision layer to bind the instance, then reuse the existing historical
    monitor time-window and single-instance validation.
  - If that follow-up omits a concrete time window, such as "第 2 台上周 CPU
    历史监控", the same selection binding is used and the existing historical
    monitor path asks for a concrete <=24h time range before calling tools.
- Recent fact follow-ups when `USE_SESSION_FACT_CONTEXT=1`:
  - stock follow-up
  - price follow-up
  - refund/billing follow-up
- Mutating lifecycle / scheduled-shutdown ordinal reference (deterministic):
  - "关机第 2 台" / "第 2 台定时关机" resolve the target against the displayed
    candidate list via `ordinalTargetFromPending` (see the resolution-model
    section above). This shares the same ordinal matcher as instance-list
    selection but does **not** call the model decision layer.

## Compatibility State

Some legacy session fields remain intentionally:

- `LastStockGpuModel` is **deleted** (2026-08-05), together with the whole stock-referent carry: `recordResolvedStockGpuFact`, `stockGpuModelFromRecentFacts`, `fallbackStockGpuModel`, `ReadRuntime.FallbackGPUModel` and the `RememberStockReferent` effect. It was not a compatibility field waiting to age out — it was the server remembering which GPU the user meant and substituting it into a later tool call whose arguments did not name one. The canonical transcript replays the earlier stock turn, so the model can carry the card itself.
- `LastDeployWorkload`, `LastDeployZone`, and `PendingDeployModel` remain read-only fallbacks for sessions written by older binaries; new turns write the unified context frame.
- `PendingSelection*` is not legacy behavior by itself; it is the persisted candidate list used by both old and new selection paths.

These compatibility fields may be removed only after old persisted sessions have aged out.

### SelectedInstance binding TTL

`SelectedInstanceID` / `SelectedInstanceName` / `SelectedInstanceSource` now
carry `SelectedInstanceAtUnix`, stamped whenever the binding is (re)established
by a genuine selection (passive re-observation does not refresh it). At turn
entry, `expireStaleSelectedInstance` clears the binding once it is older than
`selectedInstanceTTLSeconds` (30 min), before the turn-start snapshot is frozen,
so a long-abandoned selection is neither resolved as "它" nor trusted by the
guard. Rows persisted before the field existed (`SelectedInstanceAtUnix == 0`)
are treated as unstamped and never auto-expired.

## Remaining Pre-Router Gates

The following pre-router paths still exist. They are **safety gates or correct
mutating-path specialization, not pending model-layer migrations**:

- account-level billing refusal (canned safety reply)
- pending resource selection recovery (deterministic candidate-list restore)
- monitor-history time-window checks (read-only validation)
- direct diagnosis for explicit targets (read-only; model resolution allowed)
- scheduled shutdown direct dispatch (**mutating — stays deterministic**)
- lifecycle direct dispatch (**mutating — stays deterministic**)

Invariants every path preserves (do not regress):

- no write operation without a confirmation card
- no model-selected instance for mutating workflows
- explicit user target, previously user-selected target, ordinal against the
  displayed candidate list, or unique-account-instance target only
- clarification instead of confirmation when target or required parameters are uncertain

## Non-Goals / Deliberate Deviations

- **Lifecycle and scheduled-shutdown will NOT be routed through
  `resolveContextDecision`.** An earlier attempt was reverted after an
  executable red test confirmed it introduced a two-turn phantom-target
  poisoning vector (see the resolution-model section). Mutating target
  resolution is deterministic by design; the difference from the read-only
  paths is correct specialization, not convergence debt.

## Next Migration Candidates

1. Retire compatibility fields (`LastDeployWorkload`, `LastDeployZone`,
   `PendingDeployModel`) only after their feature flags and rollback behavior are
   no longer needed. `PendingSelection*` stays (shared candidate store, not
   legacy). `LastStockGpuModel` was removed on 2026-08-05 and did not wait for
   that criterion, because it was not read for compatibility — it was read to
   edit the next tool call's arguments.
2. (Optional, low priority) atomic persistence of the per-turn message writes
   and the session context write, which are currently separate non-transactional
   calls; a failed context write is logged, not rolled back. Accepted as
   low-risk for now.

Each candidate needs focused unit tests, HTTP/WS replay, and a review of write-operation target trust before old code is removed.
