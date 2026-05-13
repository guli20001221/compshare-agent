# Stage 2B RAG W0 Offline Scripts

This directory contains the first offline guardrail scripts for PR-RAG-1.

The scripts are intentionally deterministic and use only the Python standard
library. They do not call an LLM, fetch remote content, or write runtime corpus
files.

Typical order:

```powershell
python scripts/rag_w0/build_source_manifest.py --bundle F:\compshare-agent-runs\rag-source-bundle-20260512 --out F:\compshare-agent-runs\rag-w0-current\source_manifest.json
python scripts/rag_w0/validate_source.py --manifest F:\compshare-agent-runs\rag-w0-current\source_manifest.json
python scripts/rag_w0/extract_assets.py --source-manifest F:\compshare-agent-runs\rag-w0-current\source_manifest.json --assets F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json
python scripts/rag_w0/classify_links.py --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json
python scripts/rag_w0/validate_assets.py --assets F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json
python scripts/rag_w0/validate_chunks.py --chunks F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl
```

Generated run artifacts stay under `F:\compshare-agent-runs\...`; do not commit
the raw source bundle or generated manifests wholesale.
