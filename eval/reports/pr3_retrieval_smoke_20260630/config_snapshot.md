# PR3 retrieval smoke config snapshot

- Date: 2026-06-30
- Branch: codex/pr3-retrieval-gates
- Base commit: 5641ea1528847b3539b56fc8a8e86650676404e2
- Corpus: deploy/kb/stage2b_w0.jsonl
- CI retrieval gate: BM25-only, TopK=10, pinned corpus loader
- Key-protected live smoke: blocked in this local shell because no model/API environment variables were present

Expected live flags when a key is available:

- COMPSHARE_INTENT_ROUTER_MODE=dispatch
- COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP=1
- COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE=1
- COMPSHARE_RAG_GROUNDED_VALIDATOR=1
- RAG_RETRIEVAL_MODE=qwen3_rrf
- Router model: deepseek-v4-flash

