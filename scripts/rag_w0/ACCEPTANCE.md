# RAG W0 Section Label Acceptance

PR-RAG-4.f uses offline section labels only for multi-topic FAQ sources. The live quality gate is based on anchored business sections and final chunk distribution, not on average model confidence.

## Required Inputs

- `section_labels.live.jsonl`
- `needs_split.jsonl`
- final candidate chunks JSONL
- `scripts/rag_w0/pinned_sections.json`

## Gate

Run:

```powershell
python scripts/rag_w0/verify_pinned_sections.py `
  --labels <run-dir>/section_labels.live.jsonl `
  --needs-split <run-dir>/needs_split.jsonl `
  --chunks <run-dir>/chunks.live.jsonl
```

The command exits non-zero for hard failures:

- any pinned section is missing or labeled with the wrong product area;
- any empty `selected_area` row lacks a valid `empty_label_reason`;
- any `needs_split` row is missing required review fields;
- final chunks have fewer than 100 rows;
- final chunks have fewer than 2 `init_failure` or fewer than 2 `billing_rule` rows;
- fewer than 6 product areas are represented in final chunks.

`needs_split` with more than 5 rows is a soft warning. The command still exits 0, but the PR description must explain why the review queue is larger.

The old aggregate gates are not blocking gates:

- average confidence;
- percentage of `needs_review` rows.

Those values remain useful diagnostics, but mixed FAQ sections can legitimately lower them.
