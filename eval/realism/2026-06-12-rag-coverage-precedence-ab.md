# RAG partial-coverage precedence — A/B eval (2026-06-12)

## Change
`internal/prompt/rag_system_segments/coverage_rules.txt` rule 2 (partial coverage)
previously invited an open-ended "建议 `<具体下一步行动>`". That conflicts with
`anti_fabrication_anchors.txt`'s hard ban on evidence-external next-steps — a
contract pinned by `rag_test.go` (anti-fab anchor #4, mapping to fab pattern 0259,
≤0.5% fab gate). The precedence between the two was unstated.

Resolved **toward the guardrail** (anti-fab segment byte-unchanged):
- the gap is stated as a meta-statement (no citation needed);
- only two non-fabricable supplements are allowed after the gap — point to a
  platform entry (控制台 / 官方文档), or ask the user for the missing specifics;
- everything else defers to 反编造锚点 ("冲突时以反编造锚点为准").

`coverage_rules` feeds `buildRAGSystemPrompt` **only** — it never reaches the planner
(`basePromptScaffold`) or the ReAct prompt (`segments.go`). So by construction this
change **cannot** affect intent routing.

## Method
Realism harness (`run_realism_eval.ps1`): questions paraphrased from the WeCom
after-sales log (input realism only — never corpus content). Production-matching
config: deepseek-v4-flash (Modelverse) + agentic RAG default-on + write-ops ON.
- Arm A = clean main (`fc05c19`); Arm B = this change. Only `coverage_rules.txt`
  differs, baked at build time via `go:embed`.
- N=1 × 30 Qs both arms; N=3 × 6 divergent Qs both arms.

## Result — no regression attributable to the edit
- **Routing:** 4 N=1 intent/guard diffs; N=3 shows them as jitter — both arms span the
  same intent set on the boundary Qs, and the two N=1 engine-guard hits (uncited-guard,
  token-budget) did not recur. The edit cannot move routing by construction.
- **Fabrication:** none introduced. Partial-coverage Qs show consistent conservative
  gap-marking ("无法确认 / 资料未写明") plus only the allowed meta-pointer (控制台/官方文档).
- **Over-refusal:** no question answered by A went to a hard refusal in B.
- **cite-rate:** ≈0 both arms — agentic positional citation is the separate pre-existing
  gap (#155), unchanged here.
- `go test ./...` green.

**Verdict:** preventive hardening — closes the latent fabrication invitation while
preserving helpful gap-marking; no behavioral regression.
