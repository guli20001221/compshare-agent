#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from scripts.rag_w0.build_corpus_embeddings import (
    DEFAULT_BASE_URL,
    chunk_repr,
    compute_lf_sha256,
    embed_batch,
    load_env,
    read_jsonl,
    sidecar_filename,
    write_sidecar,
)


def read_sidecar(path: Path) -> tuple[dict[str, Any], dict[str, list[float]]]:
    lines = path.read_text(encoding="utf-8").splitlines()
    meta = json.loads(lines[0])["_meta"]
    vectors = {row["chunk_id"]: row["vector"] for row in map(json.loads, lines[1:])}
    return meta, vectors


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Reuse unchanged vectors and embed only changed RAG V2 chunk representations.",
        allow_abbrev=False)
    parser.add_argument("--old-corpus", type=Path, required=True)
    parser.add_argument("--new-corpus", type=Path, required=True)
    parser.add_argument("--old-sidecar", type=Path, required=True)
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--env", type=Path, required=True)
    parser.add_argument("--embed-model", default="qwen3-embedding-8b")
    parser.add_argument(
        "--report", type=Path,
        help="write the reuse counts here as JSON. Without it the numbers exist "
             "only on stdout, where nothing reads them: a build that silently "
             "re-embedded the whole corpus is indistinguishable in every "
             "artifact from one that reused every vector.")
    parser.add_argument(
        "--corpus-name", default="",
        help="label for the report; this script only ever sees paths")
    args = parser.parse_args(argv)

    old_rows = read_jsonl(args.old_corpus)
    new_rows = read_jsonl(args.new_corpus)
    old_repr = {str(row["chunk_id"]): chunk_repr(row) for row in old_rows}
    meta, old_vectors = read_sidecar(args.old_sidecar)
    if meta.get("embed_model") != args.embed_model:
        raise ValueError("old sidecar model does not match requested model")
    if meta.get("corpus_digest") != compute_lf_sha256(args.old_corpus):
        raise ValueError("old sidecar is not bound to old corpus")

    changed = [row for row in new_rows if old_repr.get(str(row["chunk_id"])) != chunk_repr(row)]
    env = load_env(args.env)
    new_vectors: list[list[float]] = []
    dim = int(meta["dim"])
    if changed:
        new_vectors, new_dim = embed_batch(
            [chunk_repr(row) for row in changed],
            base_url=env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL).rstrip("/"),
            api_key=env["MODELVERSE_API_KEY"],
            model=args.embed_model,
        )
        if new_dim != dim:
            raise ValueError(f"embedding dimension changed: {new_dim} != {dim}")
    replacements = {str(row["chunk_id"]): vector for row, vector in zip(changed, new_vectors)}
    output_rows: list[tuple[str, list[float]]] = []
    for row in new_rows:
        chunk_id = str(row["chunk_id"])
        vector = replacements.get(chunk_id)
        if vector is None and old_repr.get(chunk_id) == chunk_repr(row):
            vector = old_vectors.get(chunk_id)
        if vector is None:
            raise ValueError(f"no vector available for {chunk_id}")
        output_rows.append((chunk_id, vector))

    digest = compute_lf_sha256(args.new_corpus)
    out = args.out_dir / sidecar_filename(digest, args.embed_model)
    write_sidecar(out, corpus_digest=digest, embed_model=args.embed_model, dim=dim, rows=output_rows)
    reused = len(new_rows) - len(changed)
    print(f"reused={reused} embedded={len(changed)} output={out}")

    if args.report:
        # The reuse KEY is chunk_repr -- title + question patterns + truncated
        # content -- and not chunk_id. Those first two fields are not in the
        # chunk_id hash, so rewriting the question-pattern or product-area rule
        # invalidates every vector while every id stays put. Measured on one real
        # release pair: 78 of 526 chunk ids survived and 0 of 526 vectors did.
        # That failure raises nothing and looks exactly like an ordinary build;
        # it is only visible as this number.
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps({
            "corpus": args.corpus_name or args.new_corpus.stem,
            "chunks": len(new_rows),
            "reused": reused,
            "embedded": len(changed),
            "reuse_ratio": round(reused / len(new_rows), 4) if new_rows else 0.0,
            "embed_model": args.embed_model,
            "dim": dim,
            "corpus_digest": digest,
            "sidecar": out.name,
        }, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
