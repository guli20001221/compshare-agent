# Stage 2B RAG W0 Offline Scripts

This directory contains the offline guardrail and corpus preparation scripts for
Stage 2B RAG W0.

The scripts are intentionally deterministic and use only the Python standard
library. They do not call an LLM or write runtime corpus files. Link snapshotting
does not fetch remote content unless `snapshot_links.py --allow-network` is
passed explicitly.

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
python scripts/rag_w0/validate_chunks.py --chunks F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl
```

Generated run artifacts stay under `F:\compshare-agent-runs\...`; do not commit
the raw source bundle or generated manifests wholesale.
