# eval/skill — R2 skill-level evaluation

Per-skill evaluation so that, before we let the model select skills more freely
(R4, description-driven selection), we can tell **"变聪明了"** from **"变不稳了"**.

One case file (`cases.jsonl`), two layers:

| Layer | Test | Real model? | CI-gated? | Measures |
|---|---|---|---|---|
| **Offline (deterministic)** | `TestOfflineSkillEval` | no | **yes** (no key, no cloud) | the wiring contract — "选得出来但做不了" |
| **Selection (opt-in)** | `TestSelectionSkillEval` | yes (`-skillmodel`) | no | real skill-hit / wrong-skill — "选错了没人发现" + the R4 trigger |

## Why the split

The skill-hit / wrong-skill metric is a *classifier-quality* signal. Forcing it
into CI with a tuned heuristic would be tautological (it would pass our own cases
by construction). So CI gates only the **deterministic wiring** (given the right
intent, does the skill call the right tools, avoid forbidden/mutating tools, and
render the right answer?); the **real classifier quality** is measured on demand
with a real model, exactly like `eval.TestEval`'s `-model` gate.

## Case format (`cases.jsonl`)

One JSON object per line (`//` comments and blank lines skipped):

```json
{"id":"pricing_01","question":"想问下 4090 现在租一个小时要多少钱啊","lane":"fast",
 "expected_skill":"pricing_query",
 "expected_tools":["DescribeAvailableCompShareInstanceTypes","GetCompShareInstancePrice"],
 "forbidden_tools":["DescribeCompShareImages"],
 "reply_should_contain":["4090"],"overlapping_group":"pricing_vs_billing","tags":["price"]}
```

- `lane`: `fast` (6 catalog capabilities) drives **both** layers; `diagnosis` / `agent`
  drive only the selection layer in v1 (see Scope).
- Questions mimic **real CompShare user phrasing** (colloquial, problem-driven; calibrated
  against `聊天记录.md`), never byte-equal to a skill's triggers, so the selection layer
  is non-tautological. The GPU token (`4090`/`A100`) is verbatim because the pricing/specs
  handlers extract it from the raw question.
- Mutating tools (`Create*`, `*Workflow`) are **implicitly forbidden** for every case.

## Metrics

1. **skill-hit / wrong-skill** — selection layer (raw question → planner → `DeriveSelectedSkills`).
2. **expected-tool hit** — offline layer.
3. **forbidden-tool misuse** — offline layer (incl. all mutating tools).
4. **reply-keyword pass** — offline layer.
5. **R4 trigger** — selection layer, per `overlapping_group` wrong-skill rate.

## R4 trigger

`R4` (planner proposes skills by name+description, code adjudicates) is **evidence-gated,
not a milestone**. When a confusable `overlapping_group` (e.g. the 3-way `image_list`)
shows a **sustained** wrong-skill rate across **N≥5** runs, that is the evidence that
intent enumeration is breaking down and R4 should start. Baseline (deepseek-v4-flash,
2026-06-02, N=1): `image_list` 0/6 wrong → **R4 not yet warranted**.

## Scope (v1)

- Deterministic CI layer covers the **6 fast-tier skills** end-to-end via the exported
  `intent.NewDemoHandler(...).DispatchCapability` seam (0 LLM).
- **Diagnosis (5) + deploy (1):** cases exist and run through the selection layer, but
  their specific-skill plan-time selection is `resolved_in_react` / agent-saga and is
  **R2-v2** (needs an engine-level run). The selection layer reports diagnosis **lane**
  routing only.
- v1's first real finding: diagnosis lane-routing 6/10 — naturally-phrased diagnosis
  questions under-route to `knowledge_qa` (tracked separately; confirm with N≥5).

## Run

```bash
# Offline deterministic layer (CI, no key):
go test ./eval/skill -run TestOfflineSkillEval

# Opt-in real-model selection layer (needs LLM_API_KEY/MODELVERSE_API_KEY in env):
go test ./eval/skill -run TestSelectionSkillEval -skillmodel deepseek-v4-flash -v
```

## Adding a skill

A new fast-tier catalog skill **must** get at least one `lane:fast` case (the offline
test fails otherwise — non-vacuity guard). Bind each case's `expected_tools` to the
skill's real `required_tools`, and pick `reply_should_contain` keywords grounded in the
canned executor data (`executor_test.go`), not prose words the deterministic render may omit.
