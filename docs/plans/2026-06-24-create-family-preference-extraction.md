# Create-family preference extraction — two-stage design (Stage 2 behind the speech_act gate)

Status: design note (for the create-family/router line — #335 router, #337 speech_act gate). Not yet implemented.
Author: handed to codex from the architecture discussion 2026-06-24. Decision: **two-stage (B)**.

## Problem

PR #337 made create/deploy a **structured, gated** entry: it classifies `speech_act`
(command vs question/comparison/…) and only enters the create/deploy flow on a
*command*. That part is correct — it stops "我问个价" from popping a paid confirm card.

But to rebuild the create card it back-filled the user's **preferences**
(GPU / zone / image / platform-vs-community / purpose) with **backend keyword
matching** (`ImageName: PyTorch`, `ImageSource: community`, …). That is the wrong
layer:

- Extracting "PyTorch / 社区镜像 / 训练" from free text is **classification/extraction**,
  i.e. a judgment call. Per `CLAUDE.md` Rule 5, the model does
  classification/extraction; code does deterministic transforms. Keyword tables are a
  **fallback, not the baseline**.
- The old behavior (LLM directly calls the bare `CreateInstanceWorkflow` and fills
  `ImageName/ImageSource`) had good preference handling but an **unsafe gate** (bare
  tool could fire on a mere question).

Goal: keep the **deterministic safety gate** AND restore **LLM-driven preference
extraction**, without loading the general router with create-only fields for the
~90% of turns that are not create/deploy.

## Decision: two-stage (B)

- **Stage 1 — router (stays lean, unchanged):** `intent` + `speech_act` + the existing
  universal slots (`target_refs`). **No create-preference fields are added to the
  router schema.** Every non-create turn's router output is byte-identical to today.
- **Stage 2 — create-preference extraction (new):** a second, focused LLM call with a
  **create-only schema**, run **only** when the create-family gate has already passed
  (see trigger below). The #337 gate already establishes "this is a create-family
  command" — that is exactly the trigger for Stage 2.
- **Backend (deterministic):** take Stage-2 slots → validate against real APIs
  (stock / price / image catalog / GPU↔image compat) → render the confirm card. **No
  text re-parsing of the user utterance.**

### Why not a single enriched router schema (A)

A would add the preference slots to the shared `intent.Slots` union (intent-scoped +
`omitempty`), matching the existing `Action`/`Metrics` pattern. It is the more
conformant option, and the scoping mechanisms below would keep non-create output
clean. We rejected it as the default because the create-preference set is materially
**richer** than existing slots (5 fields, enums + free-text `image_pref`): its schema
would be visible to flash on every turn, raising verbosity / schema-invalid risk on
non-create turns. B isolates the rich schema to the path that needs it. Cost = one
extra flash call on create turns only (rare, high-value — the user is about to spend
money). This mirrors the diagnosis flow (route first, then in-flow evidence).

## Stage 2 trigger (exact)

```
run Stage 2  iff  intent ∈ {create_instance, deploy_model}  AND  speech_act == command
```

- `deploy_model` exists today (`intent.IntentDeployModel`). `create_instance` is the
  new intent from #335. Gate on the create-family set, not on a tool name.
- Gate not satisfied → no Stage-2 call; the existing consult / clarify path runs.
- **Degrade-safe:** if the Stage-2 call errors or times out → fall back to today's
  behavior (ask the user / minimal card with no pre-filled prefs). Never block the turn
  on Stage 2.

## Stage 2 schema (create-preference)

```jsonc
{
  "gpu_pref":     "string, optional. Free text e.g. '4090' / 'A800'. Empty if unspecified.",
  "zone_pref":    "string, optional. Empty if unspecified.",
  "image_pref":   "string, optional. Free text e.g. 'PyTorch' / 'ComfyUI' / 'vLLM'. Empty if unspecified.",
  "image_source": "enum {platform, community, any}. Default 'any'.",
  "purpose":      "enum {training, inference, image_gen, general}. Default 'general'."
}
```

- **All optional.** Empty / default means "user did not say" → backend does NOT
  pre-fill; it asks or uses the platform default. Zero-pref MUST yield empty, never a
  hallucinated value (anchor this in eval).
- **`image_pref` stays free-text** (do NOT enum it — the image catalog changes).
  Backend resolves it by fuzzy-matching the **live image catalog**, not a hardcoded
  list.
- Keep the schema **tight**: enums where the value space is closed (`image_source`,
  `purpose`), imperative one-line field descriptions, no prose. (flash gets verbose /
  schema-invalid under conditional "when X…" phrasing and long inputs.)

## Backend consumption (deterministic — code's job)

- `gpu_pref` / `zone_pref` → stock + availability (`DescribeAvailable…`). If
  unavailable, surface alternatives rather than failing silently.
- `image_pref` + `image_source` → resolve against the real image catalog
  (platform/community) **with a GPU↔image compatibility pre-check**. Note:
  `CheckCompShareResourceCapacity` returns `ResourceEnough=true` even for cards an
  image does not support, so use the `SupportedGpuTypes` / `gpuImageCompatible`
  pre-check (#190) before building the card.
- `purpose` → influences defaults / ordering only; **never a hard filter**.
- Render the confirm card from validated values. The editable-confirm-form path
  (`COMPSHARE_CONFIRM_FORM`, see `docs/plans/2026-06-10-create-flow-form-confirm.md`)
  consumes the same validated slots; each edit re-runs validation and re-confirms.

## Slot scoping / safety (mandatory)

- Validator/handler: create-preference slots are **scoped to create-family intents**.
  Any stray fill on a non-create intent is dropped — same defense-in-depth treatment
  `intent.Slots.Action` already gets (see its `// See memory: llm-filter-nondeterministic`
  comment in `internal/intent/types.go`: code drives off the model's structured slot,
  it does not let the model filter "by mood").
- Stage 2 is **read-only extraction**. The actual mutation still goes through the
  existing saga + confirm gate (no auto-execute). Destructive / L2 actions stay refused
  regardless (`internal/tools/safe_executor.go`).

## Retire / fallback

- Demote the #337 backend keyword scrapers from the **primary** path. Keep a thin
  keyword fallback **only** for when Stage 2 returns empty (degrade-safe), clearly
  marked as fallback — not the baseline.

## Eval gate (before flipping the flag on)

Do **not** assume flash extracts these slots accurately. Gate on:

- **Oracle smoke**, N≥5 per probe, pro-vs-flash on a create-preference probe set (vary
  phrasing, zero-pref, multi-pref, ambiguous source).
- **Anchor accuracy**: `image_source` / `purpose` enums correct; `image_pref` free-text
  captured; **zero-pref → empty** (no hallucination).
- **Jitter check**: a single smoke cannot prove classification stability; require N≥5
  same-question runs before calling it stable.
- **Non-create turns unchanged**: since Stage 1 did not change, the router output for
  non-create turns is byte-stable by construction — assert it in a test.
- Ship behind a **boot flag, default off** until eval clears (consistent with the other
  router changes).

## Open questions for the router owner / codex

1. Stage 2 reuses the #335 router model (flash) + same client? (Recommend yes — one
   model, proven structured output.)
2. Where does Stage 2 live — inside the create-family handler right after the gate, or
   as a router sub-call? (Recommend: in the create-family dispatch, co-located with the
   flow that consumes it.)
3. Multi-turn edits: if the user adds a preference in a follow-up ("换成社区镜像"),
   Stage 2 re-runs on the new turn and the result merges into the in-progress card —
   confirm the edit loop (≤3 edits, per the confirm-form plan) re-extracts rather than
   re-parsing.
