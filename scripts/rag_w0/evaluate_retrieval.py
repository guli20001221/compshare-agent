#!/usr/bin/env python3
"""Evaluate deterministic W0 retrieval quality.

Two modes:

  baseline (default): BM25 char 2/3-gram top-3, matches the production
    runtime when RAG_HYBRID_ENABLED is unset/disabled.

  hybrid: BM25 top-20 -> query-embedding cosine rerank -> top-3, matches
    the production runtime when RAG_HYBRID_ENABLED=1. Requires an
    embedding sidecar produced by build_corpus_embeddings.py and either an
    in-process ModelVerse client (reads .env.local) or a precomputed
    query-embedding cache.
"""

from __future__ import annotations

import argparse
from collections import defaultdict
import json
import os
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

try:
    from .retrieval_scoring import (
        BM25Index,
        DEFAULT_THRESHOLD,
        cosine_similarity,
        normalize_text,
        tokenize_text,
    )
    from .validate_chunks import validate_chunks
except ImportError:  # pragma: no cover
    from retrieval_scoring import (
        BM25Index,
        DEFAULT_THRESHOLD,
        cosine_similarity,
        normalize_text,
        tokenize_text,
    )
    from validate_chunks import validate_chunks


DEFAULT_TOP_K = 3
HYBRID_BM25_POOL = 20
ANSWER_BEHAVIOR = "answer"
CONFIDENCE_RANK = {"high": 2, "medium": 1, "low": 0}
DEFAULT_EMBED_MODEL = "text-embedding-3-large"
DEFAULT_BASE_URL = "https://api.modelverse.cn/v1"


def evaluate_retrieval(
    chunks_path: Path | str,
    questions_path: Path | str,
    out_path: Path | str,
    *,
    top_k: int = DEFAULT_TOP_K,
    threshold: float = DEFAULT_THRESHOLD,
    mode: str = "baseline",
    embeddings_path: Path | str | None = None,
    query_embedding_cache_path: Path | str | None = None,
    env_path: Path | str | None = None,
) -> dict[str, Any]:
    validate_chunks(chunks_path)
    chunks = _read_jsonl(chunks_path)
    index = BM25Index(chunks)
    questions = _read_jsonl(questions_path)

    chunk_embeddings: dict[str, list[float]] | None = None
    query_embedder: _QueryEmbedder | None = None
    if mode == "hybrid":
        if embeddings_path is None:
            raise ValueError("--embeddings-path required when --mode hybrid")
        chunk_embeddings = _load_chunk_embedding_sidecar(embeddings_path)
        query_embedder = _QueryEmbedder(
            cache_path=Path(query_embedding_cache_path) if query_embedding_cache_path else None,
            env_path=Path(env_path) if env_path else Path(".env.local"),
        )

    trace_records: list[dict[str, Any]] = []
    failed_questions: list[dict[str, Any]] = []
    per_group: dict[str, dict[str, int]] = defaultdict(lambda: {"hit": 0, "total": 0})
    evaluated = 0
    excluded = 0

    for question in questions:
        behavior = str(question.get("expected_behavior") or "")
        if behavior != ANSWER_BEHAVIOR:
            excluded += 1
            continue
        evaluated += 1
        if mode == "hybrid":
            assert query_embedder is not None and chunk_embeddings is not None
            scored = _retrieve_hybrid(
                question=str(question.get("question") or ""),
                question_id=str(question.get("question_id") or ""),
                product_area=str(question.get("product_area") or ""),
                chunks=chunks,
                index=index,
                top_k=top_k,
                threshold=threshold,
                chunk_embeddings=chunk_embeddings,
                query_embedder=query_embedder,
            )
        else:
            scored = _retrieve(
                question=str(question.get("question") or ""),
                product_area=str(question.get("product_area") or ""),
                chunks=chunks,
                index=index,
                top_k=top_k,
                threshold=threshold,
            )
        hit_items = [
            {"chunk_id": chunk["chunk_id"], "score": float(score), "kept": True}
            for chunk, score in scored
        ]
        expected_ids = [str(item) for item in question.get("expected_chunk_ids") or []]
        actual_ids = [item["chunk_id"] for item in hit_items]
        hit = bool(set(expected_ids).intersection(actual_ids))
        group = str(question.get("group") or "ungrouped")
        per_group[group]["total"] += 1
        if hit:
            per_group[group]["hit"] += 1
        else:
            failed_questions.append(
                {
                    "question_id": str(question.get("question_id") or ""),
                    "question": str(question.get("question") or ""),
                    "expected_chunk_ids": expected_ids,
                    "actual_top3_ids": actual_ids,
                    "group": group,
                }
            )
        trace_records.append(
            {
                "question_id": str(question.get("question_id") or ""),
                "query_raw": str(question.get("question") or ""),
                "query_normalized": normalize_text(str(question.get("question") or "")),
                "query_expansions": [],
                "hits": len(hit_items),
                "hit_items": hit_items,
                "refused_reason": "",
                "weak_evidence": False,
            }
        )

    if query_embedder is not None:
        query_embedder.flush()
    top_3_hit_rate = None if evaluated == 0 else (evaluated - len(failed_questions)) / evaluated
    summary = {
        "mode": mode,
        "questions_evaluated": evaluated,
        "questions_excluded_non_answer_behavior": excluded,
        "top_3_hit_rate": top_3_hit_rate,
        "per_group_hit_rate": _per_group_hit_rate(per_group),
        "trace_records": trace_records,
        "failed_questions": failed_questions,
    }
    _write_json(out_path, summary)
    return summary


def verify_psa_propagation(chunks_path: Path | str) -> dict[str, Any]:
    chunks = _read_jsonl(chunks_path)
    matches = [
        chunk
        for chunk in chunks
        if "compshareerrorcode" in " ".join(str(item).lower() for item in chunk.get("source_refs") or [])
        or "error-code" in str(chunk.get("chunk_id") or "")
    ]
    if not matches:
        raise ValueError("no compshareerrorcode/error-code chunk found for PSA verification")
    failures = [chunk for chunk in matches if chunk.get("product_area") != "init_failure"]
    if failures:
        ids = ", ".join(str(chunk.get("chunk_id") or "<missing>") for chunk in failures)
        raise ValueError(f"PSA propagation failed for chunks: {ids}")
    return {"checked": len(matches), "product_area": "init_failure"}


def _retrieve(
    *,
    question: str,
    product_area: str,
    chunks: list[dict[str, Any]],
    index: BM25Index,
    top_k: int,
    threshold: float,
) -> list[tuple[dict[str, Any], float]]:
    scored: list[tuple[dict[str, Any], float]] = []
    query_tokens = tokenize_text(question)
    product_area = product_area.strip().lower()
    for chunk_index, chunk in enumerate(chunks):
        if chunk.get("confidence") == "low":
            continue
        score = index.score_chunk(
            query_tokens=query_tokens,
            product_area=product_area,
            chunk_index=chunk_index,
            chunk=chunk,
        )
        if score < threshold:
            continue
        scored.append((chunk, score))
    scored.sort(
        key=lambda item: (
            -item[1],
            -CONFIDENCE_RANK.get(str(item[0].get("confidence") or ""), 0),
            str(item[0].get("chunk_id") or ""),
        )
    )
    return scored[:top_k]


def _retrieve_hybrid(
    *,
    question: str,
    question_id: str,
    product_area: str,
    chunks: list[dict[str, Any]],
    index: BM25Index,
    top_k: int,
    threshold: float,
    chunk_embeddings: dict[str, list[float]],
    query_embedder: "_QueryEmbedder",
) -> list[tuple[dict[str, Any], float]]:
    """BM25 top-20 candidates -> embedding cosine rerank -> top_k.

    Must stay byte-equivalent to internal/knowledge/retriever.go retrieveHybrid
    on the same inputs. In eval mode, embedding failure is fatal — runtime keeps
    a BM25 fallback for latency safety but eval must not silently mask
    regressions (see brief §D risk 4 / memory feedback_eval_must_reflect_runtime_no_coercion).
    """
    candidates = _retrieve(
        question=question,
        product_area=product_area,
        chunks=chunks,
        index=index,
        top_k=HYBRID_BM25_POOL,
        threshold=threshold,
    )
    if not candidates:
        return []
    query_vec = query_embedder.embed(question_id, question)
    reranked: list[tuple[dict[str, Any], float]] = []
    for chunk, _bm25_score in candidates:
        chunk_id = str(chunk.get("chunk_id") or "")
        cvec = chunk_embeddings.get(chunk_id)
        if cvec is None:
            raise KeyError(f"chunk_id {chunk_id} missing from embedding sidecar")
        reranked.append((chunk, cosine_similarity(query_vec, cvec)))
    reranked.sort(
        key=lambda item: (
            -item[1],
            -CONFIDENCE_RANK.get(str(item[0].get("confidence") or ""), 0),
            str(item[0].get("chunk_id") or ""),
        )
    )
    return reranked[:top_k]


def _load_chunk_embedding_sidecar(path: Path | str) -> dict[str, list[float]]:
    """Read embeddings_<digest>.jsonl into {chunk_id: vector}. First line is _meta."""
    out: dict[str, list[float]] = {}
    meta_seen = False
    dim: int | None = None
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for row, line in enumerate(fh, start=1):
            s = line.strip()
            if not s:
                continue
            value = json.loads(s)
            if "_meta" in value:
                if meta_seen:
                    raise ValueError(f"{path}:{row}: duplicate _meta header")
                meta_seen = True
                dim = int(value["_meta"].get("dim") or 0)
                continue
            chunk_id = str(value.get("chunk_id") or "")
            vec = value.get("vector")
            if not chunk_id or not isinstance(vec, list):
                raise ValueError(f"{path}:{row}: malformed row (chunk_id or vector missing)")
            if dim is not None and len(vec) != dim:
                raise ValueError(f"{path}:{row}: dim mismatch (got {len(vec)} expected {dim})")
            out[chunk_id] = [float(x) for x in vec]
    if not meta_seen:
        raise ValueError(f"{path}: missing _meta header on row 1")
    return out


class _QueryEmbedder:
    """Embeds queries via ModelVerse with on-disk cache keyed by question_id.

    Cache file is a single jsonl whose rows are {"question_id", "question_sha256",
    "vector"}. question_sha256 lets us detect cache poisoning if the same id
    binds to a different question across runs.
    """

    def __init__(self, *, cache_path: Path | None, env_path: Path) -> None:
        self.cache_path = cache_path
        self.cache: dict[str, dict[str, Any]] = {}
        self._dirty = False
        env = _load_env(env_path)
        self.base_url = env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
        self.api_key = env["MODELVERSE_API_KEY"]
        self.model = env.get("MODELVERSE_EMBED_MODEL", DEFAULT_EMBED_MODEL)
        if cache_path is not None and cache_path.exists():
            for line in cache_path.read_text(encoding="utf-8").splitlines():
                if not line.strip():
                    continue
                row = json.loads(line)
                qid = str(row.get("question_id") or "")
                if qid:
                    self.cache[qid] = row

    def embed(self, question_id: str, question: str) -> list[float]:
        import hashlib
        q_sha = hashlib.sha256(question.encode("utf-8")).hexdigest()
        existing = self.cache.get(question_id)
        if existing and str(existing.get("question_sha256")) == q_sha:
            # Backfill the question text on cache hit so retriever_parity_test
            # can do text-keyed lookups even when this cache was created by an
            # older script version that did not persist the field.
            if existing.get("question") != question:
                existing["question"] = question
                self._dirty = True
            return [float(x) for x in existing.get("vector") or []]
        vec = self._call(question)
        self.cache[question_id] = {
            "question_id": question_id,
            "question": question,  # consumed by Go retriever_parity_test fixture
            "question_sha256": q_sha,
            "vector": vec,
        }
        self._dirty = True
        return vec

    def _call(self, text: str) -> list[float]:
        body = json.dumps({"model": self.model, "input": [text]}, ensure_ascii=False).encode("utf-8")
        url = f"{self.base_url}/embeddings"
        headers = {
            "Authorization": f"Bearer {self.api_key}",
            "Content-Type": "application/json",
        }
        backoff = 1.0
        # ModelVerse occasionally returns transient 308 redirects and SSL EOF errors;
        # both are retryable. urllib's default redirect handler refuses POST->308
        # so we re-issue the original POST against the (same) URL after a backoff.
        for attempt in range(5):
            try:
                req = urllib.request.Request(url, data=body, method="POST", headers=headers)
                with urllib.request.urlopen(req, timeout=60) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
                vectors = data.get("data") or []
                if not vectors:
                    raise RuntimeError("empty embedding response")
                return [float(x) for x in vectors[0].get("embedding") or []]
            except urllib.error.HTTPError as exc:
                msg = exc.read().decode("utf-8", errors="replace")
                if exc.code in (308, 429, 500, 502, 503, 504) and attempt < 4:
                    time.sleep(backoff)
                    backoff *= 2
                    continue
                raise RuntimeError(f"embedding HTTP {exc.code}: {msg[:400]}") from exc
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                if attempt < 4:
                    time.sleep(backoff)
                    backoff *= 2
                    continue
                raise
        raise RuntimeError("embedding call retry exhausted")

    def flush(self) -> None:
        if not (self._dirty and self.cache_path is not None):
            return
        self.cache_path.parent.mkdir(parents=True, exist_ok=True)
        with self.cache_path.open("w", encoding="utf-8", newline="\n") as fh:
            for qid in sorted(self.cache.keys()):
                fh.write(json.dumps(self.cache[qid], ensure_ascii=False) + "\n")
        self._dirty = False


def _load_env(path: Path) -> dict[str, str]:
    env = dict(os.environ)
    if path.exists():
        for line in path.read_text(encoding="utf-8-sig").splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            env[key.strip()] = value.strip().strip('"').strip("'")
    if not env.get("MODELVERSE_API_KEY"):
        raise ValueError("MODELVERSE_API_KEY missing from env / .env.local for hybrid eval")
    return env


def _per_group_hit_rate(per_group: dict[str, dict[str, int]]) -> dict[str, dict[str, Any]]:
    out: dict[str, dict[str, Any]] = {}
    for group, counts in sorted(per_group.items()):
        total = counts["total"]
        hit = counts["hit"]
        out[group] = {
            "hit": hit,
            "total": total,
            "hit_rate": None if total == 0 else hit / total,
        }
    return out


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for row, line in enumerate(fh, start=1):
            if not line.strip():
                continue
            value = json.loads(line)
            if not isinstance(value, dict):
                raise ValueError(f"{path}:{row}: expected object")
            rows.append(value)
    return rows


def _write_json(path: Path | str, value: dict[str, Any]) -> None:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chunks", type=Path, required=True)
    parser.add_argument("--questions", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--mode", choices=("baseline", "hybrid"), default="baseline")
    parser.add_argument("--embeddings-path", type=Path, default=None,
                        help="Required when --mode hybrid: corpus embedding sidecar from build_corpus_embeddings.py")
    parser.add_argument("--query-embedding-cache", type=Path, default=None,
                        help="Optional jsonl cache for query embeddings keyed by question_id; speeds up re-runs")
    parser.add_argument("--env", type=Path, default=None,
                        help="Path to .env.local (defaults to .env.local in CWD); only read in --mode hybrid")
    args = parser.parse_args(argv)
    verify_psa_propagation(args.chunks)
    evaluate_retrieval(
        args.chunks,
        args.questions,
        args.out,
        mode=args.mode,
        embeddings_path=args.embeddings_path,
        query_embedding_cache_path=args.query_embedding_cache,
        env_path=args.env,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
