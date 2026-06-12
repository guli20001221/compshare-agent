# Intent-router prompt: strip dev provenance — A/B eval (2026-06-12)

## Change
`renderPlannerPromptExampleGroups` (internal/intent/planner.go) emitted
`source="..."` attributes on the `<examples>`/`<example>`/`<question>`
delimiters — authoring/PR-background provenance (e.g. `B8.3: 部署 + 模型全称 …`).
Those labels are developer metadata the model does not need; the naming/prompt
review flagged them as noise. They are dropped from the **rendered** prompt.

The `Source` field stays in the data model (frontmatter-validated, used for
authoring/review) — only the render path omits it. No example question,
PlanJSON, directive, intent enum, or route order changed, so this is a
provenance-only edit. The pinned intent-router prompt SHA
(`systemPromptSHA256Baseline`) moves by construction and is updated with
justification; a negative assertion locks `source=` out of the rendered prompt.

This prompt feeds `IntentRouter.Route` (intent classification) at
`planner.go:155` — it is the single-turn intent **router** prompt, not the ReAct
tool-selection prompt. So `cases.json`/`TestEval` (which exercises the ReAct
prompt via `prompt.BuildSystemWithOptions`) is **not** a gate for this change;
only the real engine path exercises it.

## Method
Two FRESH builds off `main 5b8d983`: Arm A = baseline (worktree), Arm B = this
change (main folder, commit `8a5c191`). Only the planner-prompt render differs,
baked at build time. Both run the shipped CLI (deepseek-v4-flash via Modelverse;
agentic RAG default-on).

Read-only run. **Rationale for read-only (deviation from the Lane B write-ops-on
default):** this edit changes only the intent-router prompt
(`buildSystemPrompt`); the mutating flag provably cannot affect intent
classification — it only changes the ReAct prompt and tool visibility, never the
`planner.intent` the router emits. Read-only is therefore a valid gate here and
avoids any live-account mutation risk.

Targeted probe set (12 Qs, N=3) hitting exactly the intents whose few-shot
examples carry the stripped provenance: deploy_model ×4, pricing_query ×2,
stock_availability, resource_info, monitor_query, operation_lifecycle,
gpu_specs_query, community_image_list. This is the sensitive test — the standing
realism set is knowledge_qa/diagnosis-heavy and under-exercises these examples.

## Result — no routing regression
- **11/12 probes: byte-identical intent across both arms, all 3 runs.**
- The 1 exception (operation_lifecycle on a deliberately nonexistent instance ID,
  bare action verb "关机") jitters `operation_lifecycle`↔`unknown` in **both**
  arms — a pre-existing classification ambiguity for bare-verb + missing-target,
  not introduced by the strip (Arm A 1/3, Arm B 2/3 op_lifecycle — within N=3
  jitter, if anything marginally better in B).
- **Fabrication/refusal not in scope:** this is the intent-router prompt;
  knowledge_qa answer faithfulness is governed by the separate RAG synthesis
  prompt, which this change does not touch.
- `go test ./...` green (SHA pin moved with justification; render-structure +
  negative `source=` contract test pass).

**Verdict:** provenance-only prompt slim; no behavioral regression.
