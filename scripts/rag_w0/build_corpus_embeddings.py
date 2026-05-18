#!/usr/bin/env python3
"""Build corpus embedding sidecar for hybrid retrieval.

Reads a pinned corpus JSONL, computes LF-normalized sha256 (matching
internal/knowledge/corpus_digest.go), embeds each chunk with the configured
ModelVerse embedding model, and writes one of:
  deploy/kb/embeddings_<corpus_digest>.jsonl                     (default model)
  deploy/kb/embeddings_<corpus_digest>_<embed-model>.jsonl       (non-default)

with one row per chunk plus a leading _meta header. The sidecar layout is
keyed by chunk_id so the Go loader can do dict lookups regardless of corpus
row order.

Run:
    python -m scripts.rag_w0.build_corpus_embeddings \
        --corpus deploy/kb/stage2b_w0.jsonl \
        --out-dir deploy/kb \
        --env F:/compshare-agent/.env.local

    # Lane B qwen3 stack:
    python -m scripts.rag_w0.build_corpus_embeddings \
        --corpus deploy/kb/stage2b_w0.jsonl \
        --out-dir deploy/kb \
        --env F:/compshare-agent/.env.local \
        --embed-model qwen3-embedding-8b
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

DEFAULT_BASE_URL = "https://api.modelverse.cn/v1"
DEFAULT_EMBED_MODEL = "text-embedding-3-large"
BATCH_SIZE = 32
MAX_CONTENT_RUNES_FOR_EMB = 1800  # mirror run_hybrid_eval.py chunk_repr cap


def compute_lf_sha256(path: Path) -> str:
    """LF-normalized sha256 — matches internal/knowledge/corpus_digest.go ComputeCorpusDigest."""
    data = path.read_bytes()
    data = data.replace(b"\r\n", b"\n").replace(b"\r", b"\n")
    return hashlib.sha256(data).hexdigest()


def load_env(path: Path) -> dict[str, str]:
    env = dict(os.environ)
    if path.exists():
        for line in path.read_text(encoding="utf-8-sig").splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            env[key.strip()] = value.strip().strip('"').strip("'")
    if not env.get("MODELVERSE_API_KEY"):
        raise ValueError("MODELVERSE_API_KEY missing from env / .env.local")
    return env


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with path.open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            s = line.strip()
            if s:
                rows.append(json.loads(s))
    return rows


def chunk_repr(c: dict[str, Any]) -> str:
    """Build embedding input text — title + patterns + truncated content.

    Mirrors run_hybrid_eval.py:172-178 byte-for-byte; do not change without
    rebuilding the sidecar and re-running 5 hard contracts + new hybrid gate.
    """
    title = str(c.get("title") or "")
    qp = " | ".join(str(p) for p in (c.get("question_patterns") or []))
    content = str(c.get("content") or "")
    return f"标题: {title}\n常见问法: {qp}\n正文: {content[:MAX_CONTENT_RUNES_FOR_EMB]}"


def embed_batch(
    texts: list[str],
    *,
    base_url: str,
    api_key: str,
    model: str,
) -> tuple[list[list[float]], int]:
    """Embed all texts in BATCH_SIZE chunks. Returns (vectors, dim).

    Dim is derived from the first batch's response and then enforced as an
    invariant across every subsequent batch — same-run inconsistency is a
    fatal error, but no global dim constant is required so the same script
    works for text-embedding-3-large (3072), qwen3-embedding-8b (4096 default
    or 1024 quantized), or any future OpenAI-compatible embedding model.
    """
    out: list[list[float] | None] = [None] * len(texts)
    dim = 0
    for start in range(0, len(texts), BATCH_SIZE):
        chunk = list(enumerate(texts[start : start + BATCH_SIZE], start=start))
        body = json.dumps({"model": model, "input": [t for _, t in chunk]}, ensure_ascii=False).encode("utf-8")
        req = urllib.request.Request(
            f"{base_url}/embeddings",
            data=body,
            method="POST",
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
            },
        )
        backoff = 1.0
        last_exc: Exception | None = None
        for attempt in range(5):
            try:
                with urllib.request.urlopen(req, timeout=120) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
                last_exc = None
                break
            except urllib.error.HTTPError as exc:
                body_txt = exc.read().decode("utf-8", errors="replace")
                if exc.code in (429, 500, 502, 503, 504) and attempt < 4:
                    time.sleep(backoff)
                    backoff *= 2
                    last_exc = exc
                    continue
                raise RuntimeError(f"HTTP {exc.code}: {body_txt[:400]}") from exc
            except (urllib.error.URLError, TimeoutError) as exc:
                if attempt < 4:
                    time.sleep(backoff)
                    backoff *= 2
                    last_exc = exc
                    continue
                raise
        if last_exc is not None and last_exc:
            raise last_exc
        vectors = sorted(data.get("data", []), key=lambda x: x.get("index", 0))
        if len(vectors) != len(chunk):
            raise RuntimeError(f"batch size mismatch: got {len(vectors)} expected {len(chunk)}")
        for (i, _), v in zip(chunk, vectors):
            emb = v["embedding"]
            if dim == 0:
                dim = len(emb)
                if dim == 0:
                    raise RuntimeError("first embedding has zero dim")
            elif len(emb) != dim:
                raise RuntimeError(
                    f"inconsistent embedding dim within run: got {len(emb)} expected {dim} "
                    f"(model={model} batch index={i})"
                )
            out[i] = emb
    assert all(o is not None for o in out)
    return out, dim  # type: ignore


def write_sidecar(
    out_path: Path,
    *,
    corpus_digest: str,
    embed_model: str,
    dim: int,
    rows: list[tuple[str, list[float]]],
) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("w", encoding="utf-8", newline="\n") as fh:
        meta = {
            "_meta": {
                "corpus_digest": corpus_digest,
                "embed_model": embed_model,
                "dim": dim,
                "rows": len(rows),
            }
        }
        fh.write(json.dumps(meta, ensure_ascii=False, sort_keys=True) + "\n")
        for chunk_id, vector in rows:
            fh.write(
                json.dumps({"chunk_id": chunk_id, "vector": vector}, ensure_ascii=False) + "\n"
            )


def sidecar_filename(corpus_digest: str, embed_model: str) -> str:
    """Default model writes embeddings_<digest>.jsonl (unchanged, backward
    compatible with existing pin in corpus_digest.go). Non-default models
    append a model suffix so multiple sidecars can coexist on disk and the
    Go loader (B.3) can pick the right one by mode.
    """
    if embed_model == DEFAULT_EMBED_MODEL:
        return f"embeddings_{corpus_digest}.jsonl"
    return f"embeddings_{corpus_digest}_{embed_model}.jsonl"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--corpus", type=Path, required=True, help="Path to corpus JSONL")
    parser.add_argument("--out-dir", type=Path, required=True, help="Output directory (deploy/kb)")
    parser.add_argument("--env", type=Path, default=Path(".env.local"))
    parser.add_argument("--expected-corpus-digest", type=str, default=None,
                        help="If provided, fail if computed corpus digest does not match")
    parser.add_argument("--embed-model", type=str, default=None,
                        help=f"Override embedding model (default: ${{MODELVERSE_EMBED_MODEL}} "
                             f"or '{DEFAULT_EMBED_MODEL}'). Non-default models append "
                             f"a _<model> suffix to the sidecar filename.")
    args = parser.parse_args(argv)

    env = load_env(args.env)
    base_url = env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
    api_key = env["MODELVERSE_API_KEY"]
    model = args.embed_model or env.get("MODELVERSE_EMBED_MODEL", DEFAULT_EMBED_MODEL)

    corpus_digest = compute_lf_sha256(args.corpus)
    if args.expected_corpus_digest and corpus_digest != args.expected_corpus_digest:
        print(
            f"[build-embeddings] corpus digest mismatch: got {corpus_digest} want {args.expected_corpus_digest}",
            file=sys.stderr,
        )
        return 2

    out_path = args.out_dir / sidecar_filename(corpus_digest, model)
    if out_path.exists():
        existing_digest = compute_lf_sha256(out_path)
        print(f"[build-embeddings] cache hit: {out_path} already exists (digest {existing_digest[:16]}...), skipping",
              file=sys.stderr)
        print(out_path)
        return 0

    chunks = read_jsonl(args.corpus)
    print(f"[build-embeddings] corpus={len(chunks)} chunks, digest={corpus_digest}", file=sys.stderr)
    print(f"[build-embeddings] model={model}", file=sys.stderr)
    print(f"[build-embeddings] embedding {len(chunks)} chunks via {base_url} ...", file=sys.stderr)

    texts = [chunk_repr(c) for c in chunks]
    vectors, dim = embed_batch(texts, base_url=base_url, api_key=api_key, model=model)
    print(f"[build-embeddings] dim={dim} (derived from first batch response)", file=sys.stderr)
    rows = [(str(c.get("chunk_id") or ""), v) for c, v in zip(chunks, vectors)]

    # sanity: chunk_ids unique
    ids = [r[0] for r in rows]
    if len(set(ids)) != len(ids):
        raise RuntimeError("corpus contains duplicate chunk_ids — sidecar key requires unique ids")
    if any(not i for i in ids):
        raise RuntimeError("corpus contains chunk with empty chunk_id")

    write_sidecar(out_path, corpus_digest=corpus_digest, embed_model=model, dim=dim, rows=rows)
    sidecar_digest = compute_lf_sha256(out_path)
    print(
        f"[build-embeddings] wrote {out_path} rows={len(rows)} dim={dim} sidecar_digest={sidecar_digest}",
        file=sys.stderr,
    )
    print(out_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
