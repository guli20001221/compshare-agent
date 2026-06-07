# Default-on gate — external KB + agentic SearchKnowledge (Phase 3)

Flips `COMPSHARE_EXTERNAL_KNOWLEDGE` and `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE` to
**default-on** (boot-only, `=0` rolls back). Enabled **together** — the agentic
SearchKnowledge value comes from the external tool/ops KB; agentic-alone at
external-off had no positive value (P5). All evidence below is from **merged main**
(origin/main after #239/#240/#241), `ds-v4-flash`, qwen3 RRF, live CLI.

## What the evidence supports (stated precisely — NOT "zero drift")

- **No platform-hosted-API vs self-hosted-service confusion.** The 34-probe joint
  enablement eval on merged main (N=5, 170 runs) shows self-hosted questions ground
  on the external corpus (8/8) and platform questions ground on the platform corpus,
  with no swap in either direction. The one previously-found confusion
  (`"我起了个本地服务，api key 在哪拿"` → platform console key) is fixed by the
  self-host-key anchor (#241): now a safe abstain 5/5, no platform-key mis-direction.
  Raw: `joint_onmain_anchor_observations.jsonl`.
- **No external-corpus contamination of platform answers.** Platform-FAQ faithfulness
  eval (15 probes × both conditions, ext-on vs prod-default ext-off, 45+45 runs):
  **zero** `ext-*` citations in any platform answer. Even the one probe that *retrieved*
  an `ext-ollama` chunk at ext-on (`fq-modelverse-curl`) did not cite it and answered
  with the correct platform `api.modelverse.cn` curl. Citations stayed `w0-*` / handler
  `cited=[]` under both conditions. Raw: `platform_faithfulness_ext{1on,0off}_observations.jsonl`.
- **Platform-key path intact (no over-correction).** `pa-modelverse-apikey-where` and
  the OpenAI-SDK probe return the platform console key + `api.modelverse.cn` 5/5 / 10/10.
- **Regression unchanged.** Pricing/stock route to handlers, off-topic/account refuse,
  instance-symptom clarifies → SearchKnowledge fires 0/N on these; no `ext-*` leakage.
- **Retrieval parity** for platform questions is preserved by the merged index
  (#237 256-Q gate: platform Top-3 unchanged on the 687+29 index) — cited, not re-run here.

## Known limitation — do NOT read this as "the flags have zero effect"

The faithfulness eval surfaced **one** flake: `fq-coding-plan-bill` — a Coding-Plan
billing question that occasionally misroutes to the GPU-pricing handler (a non-answer).
Measured rate (N=10 each): **ext0off (current prod) 1/10; ext1on (Phase-3) 3/10**.

- This is a **planner intent-classification jitter**, NOT external-corpus contamination
  (the on-topic samples are substance-equivalent to baseline; no foreign billing facts;
  no self-hosted content). Verified architecturally: the planner classification
  (`internal/intent/planner.go`) consults **neither** flag nor the tool registry —
  `IntentToolSubset(i)` runs only *after* an intent is chosen. So the flags cannot
  change the billing→pricing decision.
- It is **pre-existing**: the misroute appears at the current production default too
  (1/10), so default-on does not introduce it.
- Honest caveat: at N=10 the observed rates are 1/10 vs 3/10. Per the architecture the
  true rates are equal (a common ~20% misfire on this question), and 1-vs-3 is not
  statistically distinguishable at N=10 (Fisher exact p≈0.6) — but we do **not** claim
  the flags have provably zero effect on the observed sample. Treated as a pre-existing
  planner-quality issue to fix separately (billing↔pricing disambiguation), NOT a
  default-on blocker, since it is flag-independent and already present in production.

## Safety / rollback

- **Boot-only, reversible.** Both flags resolve once at `cmd` startup (CLI + HTTP).
  `COMPSHARE_EXTERNAL_KNOWLEDGE=0` → platform-only; `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE=0`
  → SearchKnowledge hidden. The Go-package default for the agentic tool stays off, so
  engine/tools unit tests are unaffected.
- **External merge degrades safely:** `loadKnowledgeCorpora` falls back to platform-only
  if the external corpus is missing/bad/digest-drifted — a broken external file never
  takes down platform RAG.
- Full `go test ./...` green on merged main + this flip.

## Net

The flip is **safe on the confusion and contamination axes** (the gate's core concern):
no platform/self-host confusion, no external-corpus contamination of platform answers,
regression unchanged, rollback one env var. The one open item is a **pre-existing,
flag-independent** billing↔pricing planner jitter, tracked as a separate follow-up.
