# Stage 2B RAG W0 Offline Scripts

This directory contains the offline guardrail and corpus preparation scripts for
Stage 2B RAG W0.

The production pipeline scripts are deterministic and use only the Python
standard library. `model_smoke.py` is the only script that calls an LLM, and it is
only used as a bounded provider gate before running the later batch steps. Link
snapshotting does not fetch remote content unless `snapshot_links.py
--allow-network` is passed explicitly.

Typical order:

```powershell
python scripts/rag_w0/build_source_manifest.py --bundle F:\compshare-agent-runs\rag-source-bundle-20260512 --out F:\compshare-agent-runs\rag-w0-current\source_manifest.json
python scripts/rag_w0/validate_source.py --manifest F:\compshare-agent-runs\rag-w0-current\source_manifest.json
python scripts/rag_w0/extract_assets.py --source-manifest F:\compshare-agent-runs\rag-w0-current\source_manifest.json --assets F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json
python scripts/rag_w0/classify_links.py --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json
python scripts/rag_w0/snapshot_links.py --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json --snapshot-root F:\compshare-agent-runs\rag-w0-current\link_snapshots
python scripts/rag_w0/describe_images.py --assets F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --out F:\compshare-agent-runs\rag-w0-current\asset_notes.jsonl
python scripts/rag_w0/validate_assets.py --assets F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json
python scripts/rag_w0/normalize_docs.py --source-manifest F:\compshare-agent-runs\rag-w0-current\source_manifest.json --assets F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json --asset-notes F:\compshare-agent-runs\rag-w0-current\asset_notes.jsonl --out-dir F:\compshare-agent-runs\rag-w0-current\normalized_docs
python scripts/rag_w0/clean_docs.py --normalized-dir F:\compshare-agent-runs\rag-w0-current\normalized_docs --out-dir F:\compshare-agent-runs\rag-w0-current\cleaned_docs
python scripts/rag_w0/validate_cleaned_docs.py --dir F:\compshare-agent-runs\rag-w0-current\cleaned_docs
python scripts/rag_w0/mine_internal_cases.py --source F:\compshare-agent-runs\rag-source-bundle-20260512\internal_cases\spt-record.txt --source-id wxwork-spt-record-2026-05 --out F:\compshare-agent-runs\rag-w0-current\internal_case_mining\cases.jsonl --approval-template-out F:\compshare-agent-runs\rag-w0-current\internal_case_mining\approval_templates.jsonl
python scripts/rag_w0/model_smoke.py --asset-manifest F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --cases F:\compshare-agent-runs\rag-w0-current\internal_case_mining\cases.jsonl --out F:\compshare-agent-runs\rag-w0-current\model_smoke_summary.json --emit-asset-notes F:\compshare-agent-runs\rag-w0-current\asset_notes.smoke.jsonl
python scripts/rag_w0/chunk_docs.py --cleaned-dir F:\compshare-agent-runs\rag-w0-current\cleaned_docs --asset-notes F:\compshare-agent-runs\rag-w0-current\asset_notes.jsonl --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json --cases F:\compshare-agent-runs\rag-w0-current\internal_case_mining\cases.jsonl --out F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl
python scripts/rag_w0/validate_chunks.py --chunks F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl
python scripts/rag_w0/generate_eval_questions.py --chunks F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl --cases F:\compshare-agent-runs\rag-w0-current\internal_case_mining\cases.jsonl --out F:\compshare-agent-runs\rag-w0-current\golden_questions.jsonl
```

Generated run artifacts stay under `F:\compshare-agent-runs\...`; do not commit
the raw source bundle or generated manifests wholesale.

`model_smoke.py` reads credentials from `.env.local` or the process
environment. Keep that file local; it is ignored by git.

Final chunking is intentionally gated. `chunk_docs.py` fails unless every
included image has a reviewed VL note (`included_with_vl_note`) and every link is
resolved, snapshotted, navigation-only, or excluded. For local dry runs before
the full Qwen VL/link pass, add `--allow-incomplete-inputs`; do not promote those
outputs.

When normalized documents contain `<!-- asset_note: ... -->` comments,
`chunk_docs.py` renders included image notes into chunk text as `[图说] ...` and
keeps matching `asset_refs` for later UI rendering. Malformed asset-note JSON is
fatal in gated chunking and only downgraded to a warning for dry runs.
