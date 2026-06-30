# PR3 retrieval smoke config snapshot

- Date: 2026-06-30
- Branch: `codex/pr3-retrieval-gates`
- Start commit before review fix: `3cf60031`
- Corpus: `deploy/kb/stage2b_w0.jsonl`
- Env sources: `F:\compshare-agent\.env`, `F:\compshare-agent\.env.local`
- Secret handling: loaded into the process environment only; no secret values are recorded in this report.
- CI retrieval gate: BM25-only, TopK=10, pinned corpus loader

Live smoke flags:

- `COMPSHARE_INTENT_ROUTER_MODE=dispatch`
- `COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP=1`
- `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE=1`
- `COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD=1` (explicit smoke-only fallback; code default is off)
- `COMPSHARE_RAG_GROUNDED_VALIDATOR=1`
- `COMPSHARE_RAG_DOMAIN_MATCH_GUARD=1`
- `COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS=1`
- `RAG_RETRIEVAL_MODE=qwen3_rrf`
- Router model: `deepseek-v4-flash`
