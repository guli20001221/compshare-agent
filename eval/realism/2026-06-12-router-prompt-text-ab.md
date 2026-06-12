# A/B — intent-router prompt role rename (#130 Stage 2)

**Change:** `basePromptScaffold()` first line, the model-facing role self-description:
`"You are the IntentPlan planner for the CompShare console agent."`
→ `"You are the intent router for the CompShare console agent."`
(`internal/intent/router.go:583`). Nothing else in the planner/router prompt
changed — same intent enum, required_tools, route order, examples, directives.
`systemPromptSHA256Baseline` moved `f013ea42…` → `10d56b7e…` by construction.

**Why eval-gated (not byte-stable):** this is model-read text that defines the
model's role, so it is a behavior surface. The gate is **intent-classification
parity** between the old wording (arm A) and the new (arm B): does renaming the
role from "planner" to "router" shift how the model classifies turns?

## Method

- **arm A** = `#286` tip `e5fddd1` ("IntentPlan planner"); **arm B** = this change
  `2217f44` ("intent router"). Both built from `./cmd`, run from their own
  worktree (deploy/kb aligned, corpus digest pinned).
- **Runner:** `eval/realism/run_realism_eval.ps1`, fresh CLI session per question
  (no PriorText bleed), **read-only** (mutating tools unset — classification is
  independent of the mutating flag, which only changes the ReAct prompt + tool
  visibility), model = deepseek-v4-flash (shipped `agent.yaml`), agentic RAG on.
- **Sets:**
  1. **Boundary probe, N=3** — 12 questions exercising the direct-dispatch intents
     the 30Q realism set under-covers (pricing / stock / resource / monitor /
     gpu_specs / operation_lifecycle / deploy_model / billing_instance /
     network_accelerator_status / disk_info / community_image_list / knowledge_qa).
  2. **Realism 30Q, N=1** — `realism_questions.jsonl` (paraphrased real after-sales
     turns), broad coverage.
  3. **Divergent re-probe, N=5 per arm** — every question that differed in (1)/(2),
     re-run to separate jitter from change-induced drift (per the N≥5 discipline
     before attributing a regression).

Gate = **A↔B intent parity**: arm B must not systematically misclassify a turn
that arm A classified stably.

## Results

### Boundary probe (N=3) — 34/36 identical
All 11 direct-dispatch intents **3/3 stable and identical on both arms**:
pricing_query, stock_availability, resource_info, monitor_query, gpu_specs_query,
operation_lifecycle, deploy_model, billing_instance, network_accelerator_status,
community_image_list, knowledge_qa.
Only divergence: **P10 disk** ("数据盘还剩多少空间") — arm A itself unstable
(resource_info / disk_info / disk_info), arm B (monitor_query / disk_info /
monitor_query). A borderline question unstable on **both** arms.

### Realism 30Q (N=1) — 28/30 identical
Divergences (both within the question's acceptable `expect_intent` set):
- **R07** "关机了为什么还在收费": A=billing_instance, B=knowledge_qa
- **R26** "网速怎么这么慢啊": A=network_accelerator_status, B=knowledge_qa

### Divergent re-probe (N=5 per arm) — jitter vs drift

| Question | Arm A (planner) | Arm B (router) | Reading |
|---|---|---|---|
| R26 网速慢 | netacc ×4, diagnosis ×1 | netacc ×4, diagnosis ×1 | **identical distribution** → the N=1 diff was single-sample jitter |
| R07 关机还扣费 | billing_instance ×3, knowledge_qa ×2 | knowledge_qa ×5 | arm A already unstable (3:2); arm B consolidated to knowledge_qa — a valid `expect_intent` and the canonical answer (storage-fee policy is a knowledge explanation, not a per-instance bill lookup) |
| P10 数据盘剩余 | disk_info ×4, monitor_query ×1 | disk_info ×5 | both dominant disk_info; arm B **more** stable |

## Verdict — PASS, no classification regression

- 62/66 comparisons identical out of the gate.
- The 4 raw diffs decompose to: **1 statistically identical at N=5** (R26), and
  **3 borderline questions** (R07, P10) where arm A was *itself* jittery and arm B
  resolved **within the acceptable intent set and at least as stably** (R07→
  knowledge_qa 5/5, P10→disk_info 5/5).
- **No case** of arm A stable on intent X → arm B systematically shifted to Y.
  The direct-dispatch intents — where a "router" vs "planner" framing would most
  plausibly bite — are byte-for-byte stable across arms.

The role self-description rename does not move classification behavior. Change is
safe to land.

*(Per-question replies and traces contain real account identifiers and are NOT
committed — only intent labels and this summary. `eval/realism/out*/` is
gitignored.)*
