# Knowledge release artifacts

This directory contains the reviewed corpora and qwen3 embedding sidecars that
the manual `import-knowledge-release` job publishes to `compshare-kb`.
The Agent does not load these files or run an in-process retrieval mode: it calls
the deployed knowledge service through MCP, and that service owns query planning,
embedding, fusion and reranking.

## Files

| File | Purpose |
|---|---|
| `stage2b_w0.jsonl` | Public platform corpus |
| `external_w0.jsonl` | Platform-neutral technical corpus |
| `embeddings_<digest>_qwen3-embedding-8b.jsonl` | Corpus-bound qwen3 vectors |
| `base_release.txt` | Active release the candidate must replace |

The corpus and sidecar names are bound by LF-normalized SHA-256 digests in
`internal/knowledge/corpus_digest.go`. Tests and the import worker reject a
mismatched text/vector pair.

Every chunk must be `acl="customer_safe"` and come from the reviewed public
source set. Private chats, tickets, admin pages and temporary signed links are
excluded at source. The external corpus should contain durable protocols and
troubleshooting material, not volatile platform facts such as prices, packages,
current inventory, promotions or console routes.

## Publication

`base_release.txt` makes publication a compare-and-swap. The release named in
the file must still be active when the job publishes; otherwise the candidate
remains validated and the active pointer does not move.

When regenerating a corpus:

1. Read the active release from the `compshare-kb` release ledger.
2. Update `base_release.txt` in the same commit as the corpus.
3. Regenerate the corresponding qwen3 sidecar.
4. Run the corpus, digest and retrieval tests.
5. Run `import-knowledge-release` and inspect its validation output.

If the base comparison fails, create a new commit with the current active
release. A validated candidate has immutable parent and release IDs and cannot
be republished against a different base. `none` is valid only for the first
publication. `KB_PARENT_RELEASE_ID` is reserved for consecutive imports whose
second parent does not exist until the first import completes.

```bash
python scripts/rag_w0/build_corpus_embeddings.py \
  --corpus deploy/kb/stage2b_w0.jsonl \
  --out-dir deploy/kb \
  --env <path-to-.env-with-MODELVERSE_API_KEY> \
  --embed-model qwen3-embedding-8b
```

`internal/knowledge/retriever.go` and
`scripts/rag_w0/evaluate_retrieval.py` are offline evaluation/reference code.
They must not become a second Agent runtime retrieval path.
