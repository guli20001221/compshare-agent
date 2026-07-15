#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
from pathlib import Path

from scripts.rag_w0.build_corpus_embeddings import chunk_repr
from scripts.rag_v2.pipeline import normalized_file_digest


def rows(path: Path) -> list[dict[str, object]]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Re-key a sidecar only when embedding inputs are byte-identical.")
    parser.add_argument("--old-corpus", type=Path, required=True)
    parser.add_argument("--new-corpus", type=Path, required=True)
    parser.add_argument("--old-sidecar", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args(argv)

    old_rows, new_rows = rows(args.old_corpus), rows(args.new_corpus)
    if [row.get("chunk_id") for row in old_rows] != [row.get("chunk_id") for row in new_rows]:
        raise ValueError("chunk IDs differ; full re-embedding is required")
    if [chunk_repr(row) for row in old_rows] != [chunk_repr(row) for row in new_rows]:
        raise ValueError("embedding inputs differ; full re-embedding is required")
    sidecar_lines = args.old_sidecar.read_text(encoding="utf-8").splitlines()
    meta = json.loads(sidecar_lines[0])
    meta["_meta"]["corpus_digest"] = normalized_file_digest(args.new_corpus)
    args.out.write_text(json.dumps(meta, ensure_ascii=False, sort_keys=True) + "\n" + "\n".join(sidecar_lines[1:]) + "\n", encoding="utf-8")
    print(args.out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
