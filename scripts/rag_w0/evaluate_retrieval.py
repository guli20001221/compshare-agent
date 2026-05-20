#!/usr/bin/env python3
"""Evaluate deterministic W0 retrieval quality.

Four modes (mirror internal/knowledge/retriever.go retrieval pipelines):

  baseline (default): BM25 char 2/3-gram top-3. Matches the production
    runtime when RAG_RETRIEVAL_MODE=bm25_only or unset+RAG_HYBRID_ENABLED
    unset.

  hybrid: BM25 top-20 -> text-embedding-3-large cosine rerank -> top-3.
    Matches RAG_RETRIEVAL_MODE=hybrid_cosine. Requires the text-emb-3
    sidecar from build_corpus_embeddings.py (default --embed-model).

  hybrid_rerank: BM25 top-20 -> text-emb-3 cosine top-10 ->
    qwen3-reranker-8b cross-encoder -> top-3. Matches
    RAG_RETRIEVAL_MODE=hybrid_rerank.

  qwen3_full: BM25 top-20 -> qwen3-embedding-8b cosine top-10 ->
    qwen3-reranker-8b cross-encoder -> top-3. Matches
    RAG_RETRIEVAL_MODE=qwen3_full. Requires the qwen3 sidecar from
    build_corpus_embeddings.py --embed-model qwen3-embedding-8b.

  qwen3_rrf: BM25 top-50 fused with qwen3-embedding-8b dense-full-corpus
    top-50 via Reciprocal Rank Fusion (k=60) -> top-10 -> qwen3-reranker-8b
    cross-encoder -> top-3. Matches RAG_RETRIEVAL_MODE=qwen3_rrf. Reuses
    the same qwen3 sidecar as qwen3_full; recovers BM25-zero-hit queries
    via the dense leg (cascade path skips embedder when BM25 returns 0).

Eval fails loud on embedding/reranker API errors so regressions are
visible (the runtime swallows these for latency safety; eval must not
silently mask them — see memory feedback_eval_must_reflect_runtime_no_coercion).
"""

from __future__ import annotations

import argparse
from collections import defaultdict
import hashlib
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
# Mirror internal/knowledge/retriever.go:rerankerPoolSize. The reranker
# stage scores this many cosine-top candidates and the final top-K is
# drawn from the reranker's ordering. Larger pool = more recall headroom
# but higher reranker latency / tokens.
RERANKER_POOL_SIZE = 10
# Mirror internal/knowledge/retriever.go:rrfBM25PoolSize / rrfDensePoolSize /
# rrfK. RRF fusion uses a wider BM25/dense window than cascade modes
# (Elastic + OpenSearch + Azure recommend 50-100). k=60 is the canonical
# smoothing constant from Cormack et al. 2009 SIGIR.
RRF_BM25_POOL = 50
RRF_DENSE_POOL = 50
RRF_K = 60
# Mirror internal/knowledge/retriever.go:chunkReprForRerank max content.
RERANKER_MAX_CONTENT_RUNES = 1800
ANSWER_BEHAVIOR = "answer"
CONFIDENCE_RANK = {"high": 2, "medium": 1, "low": 0}
DEFAULT_EMBED_MODEL = "text-embedding-3-large"
DEFAULT_QWEN3_EMBED_MODEL = "qwen3-embedding-8b"
DEFAULT_RERANKER_MODEL = "qwen3-reranker-8b"
DEFAULT_BASE_URL = "https://api.modelverse.cn/v1"

# Modes that require an embedding sidecar + query embedder.
_EMBED_MODES = {"hybrid", "hybrid_rerank", "qwen3_full", "qwen3_rrf"}
# Modes that require a reranker client.
_RERANK_MODES = {"hybrid_rerank", "qwen3_full", "qwen3_rrf"}


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
    reranker_cache_path: Path | str | None = None,
    embed_model: str | None = None,
    reranker_model: str | None = None,
    env_path: Path | str | None = None,
) -> dict[str, Any]:
    validate_chunks(chunks_path)
    chunks = _read_jsonl(chunks_path)
    index = BM25Index(chunks)
    questions = _read_jsonl(questions_path)

    chunk_embeddings: dict[str, list[float]] | None = None
    query_embedder: _QueryEmbedder | None = None
    reranker: _Reranker | None = None
    if mode in _EMBED_MODES:
        if embeddings_path is None:
            raise ValueError(f"--embeddings-path required when --mode {mode}")
        chunk_embeddings = _load_chunk_embedding_sidecar(embeddings_path)
        resolved_embed_model = embed_model or _default_embed_model_for_mode(mode)
        query_embedder = _QueryEmbedder(
            cache_path=Path(query_embedding_cache_path) if query_embedding_cache_path else None,
            env_path=Path(env_path) if env_path else Path(".env.local"),
            model_override=resolved_embed_model,
        )
    if mode in _RERANK_MODES:
        reranker = _Reranker(
            cache_path=Path(reranker_cache_path) if reranker_cache_path else None,
            env_path=Path(env_path) if env_path else Path(".env.local"),
            model=reranker_model or DEFAULT_RERANKER_MODEL,
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
        if mode == "qwen3_rrf":
            assert query_embedder is not None and chunk_embeddings is not None and reranker is not None
            scored = _retrieve_qwen3_rrf(
                question=str(question.get("question") or ""),
                question_id=str(question.get("question_id") or ""),
                product_area=str(question.get("product_area") or ""),
                chunks=chunks,
                index=index,
                top_k=top_k,
                threshold=threshold,
                chunk_embeddings=chunk_embeddings,
                query_embedder=query_embedder,
                reranker=reranker,
            )
        elif mode in _RERANK_MODES:
            assert query_embedder is not None and chunk_embeddings is not None and reranker is not None
            scored = _retrieve_hybrid_rerank(
                question=str(question.get("question") or ""),
                question_id=str(question.get("question_id") or ""),
                product_area=str(question.get("product_area") or ""),
                chunks=chunks,
                index=index,
                top_k=top_k,
                threshold=threshold,
                chunk_embeddings=chunk_embeddings,
                query_embedder=query_embedder,
                reranker=reranker,
                mode=mode,
            )
        elif mode in _EMBED_MODES:
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
    if reranker is not None:
        reranker.flush()
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


def _default_embed_model_for_mode(mode: str) -> str:
    """Pick the embedder model that matches the retrieval mode.

    qwen3_full and qwen3_rrf both pair with qwen3-embedding-8b (and the
    qwen3 sidecar); other hybrid modes use text-embedding-3-large.
    Callers may override via --embed-model, but the default here makes
    the script self-consistent when only --mode is set.
    """
    if mode in {"qwen3_full", "qwen3_rrf"}:
        return DEFAULT_QWEN3_EMBED_MODEL
    return DEFAULT_EMBED_MODEL


def _chunk_repr_for_rerank(chunk: dict[str, Any]) -> str:
    """Build the per-chunk text the reranker scores.

    Must stay byte-equivalent to internal/knowledge/retriever.go
    chunkReprForRerank and scripts/rag_w0/build_corpus_embeddings.py
    chunk_repr — all three score the same chunk representation so cosine
    and cross-encoder signals remain comparable.
    """
    title = str(chunk.get("title") or "")
    patterns = " | ".join(str(p) for p in (chunk.get("question_patterns") or []))
    content = str(chunk.get("content") or "")
    # Python str slicing is by code point, matching Go's []rune slicing.
    if len(content) > RERANKER_MAX_CONTENT_RUNES:
        content = content[:RERANKER_MAX_CONTENT_RUNES]
    return f"标题: {title}\n常见问法: {patterns}\n正文: {content}"


def _retrieve_hybrid_rerank(
    *,
    question: str,
    question_id: str,
    product_area: str,
    chunks: list[dict[str, Any]],
    index: "BM25Index",
    top_k: int,
    threshold: float,
    chunk_embeddings: dict[str, list[float]],
    query_embedder: "_QueryEmbedder",
    reranker: "_Reranker",
    mode: str,
) -> list[tuple[dict[str, Any], float]]:
    """BM25 top-20 -> cosine top-RERANKER_POOL_SIZE -> reranker top-K.

    Must stay byte-equivalent to internal/knowledge/retriever.go Retrieve
    when r.mode is hybrid_rerank or qwen3_full. Eval fails loud on
    reranker errors (same convention as the hybrid embedding path); the
    runtime keeps a cosine fallback for latency safety but eval must
    surface regressions, not mask them.
    """
    cosine_pool = _retrieve_hybrid(
        question=question,
        question_id=question_id,
        product_area=product_area,
        chunks=chunks,
        index=index,
        top_k=RERANKER_POOL_SIZE,
        threshold=threshold,
        chunk_embeddings=chunk_embeddings,
        query_embedder=query_embedder,
    )
    if not cosine_pool:
        return []
    docs = [_chunk_repr_for_rerank(chunk) for chunk, _ in cosine_pool]
    results = reranker.rerank(
        question_id=question_id,
        question=question,
        docs=docs,
        top_n=top_k,
        mode=mode,
    )
    reranked: list[tuple[dict[str, Any], float]] = []
    for idx, score in results:
        if idx < 0 or idx >= len(cosine_pool):
            # Server returned an out-of-range index. Skip with a warning;
            # this is the same defensive guard as Go retriever.go.
            print(
                f"  rerank: skipping out-of-range index {idx} (pool={len(cosine_pool)})",
                flush=True,
            )
            continue
        chunk, _ = cosine_pool[idx]
        reranked.append((chunk, float(score)))
    if not reranked:
        # All indices were out of range or results was empty. Surface as
        # an error per the fail-loud eval convention.
        raise RuntimeError(
            f"reranker returned no usable results for question_id={question_id!r} "
            f"(pool_size={len(cosine_pool)}, server_results={len(results)})"
        )
    # Reranker returns desc-sorted but we sort defensively, same as Go
    # client's safety re-sort.
    reranked.sort(key=lambda item: -item[1])
    return reranked[:top_k]


def _retrieve_qwen3_rrf(
    *,
    question: str,
    question_id: str,
    product_area: str,
    chunks: list[dict[str, Any]],
    index: "BM25Index",
    top_k: int,
    threshold: float,
    chunk_embeddings: dict[str, list[float]],
    query_embedder: "_QueryEmbedder",
    reranker: "_Reranker",
) -> list[tuple[dict[str, Any], float]]:
    """BM25 top-50 + dense-full-corpus top-50 -> RRF fusion -> top-10
    -> qwen3-reranker-8b -> top-K.

    Must stay byte-equivalent to internal/knowledge/retriever.go Retrieve
    when r.mode is qwen3_rrf. The dense leg iterates the ACTIVE chunk set
    (post-confidence/validity filtering) just like the Go side, NOT the
    raw sidecar map.

    Eval is fail-loud on embedder + reranker errors (same convention as
    _retrieve_hybrid_rerank); the runtime keeps fallbacks for latency
    safety but eval must surface regressions.
    """
    bm25_pool = _retrieve(
        question=question,
        product_area=product_area,
        chunks=chunks,
        index=index,
        top_k=RRF_BM25_POOL,
        threshold=threshold,
    )
    # Dense full-corpus scan. CRITICAL: iterate `chunks` (active set after
    # confidence filter) not `chunk_embeddings.keys()` (raw sidecar) so
    # filtered chunks don't leak back in via dense — mirrors Go's
    # denseFullSearch invariant from rrf_test.go::TestDenseFullSearchIteratesCorpusNotSidecar.
    query_vec = query_embedder.embed(question_id, question)
    dense_scored: list[tuple[dict[str, Any], float]] = []
    for chunk in chunks:
        if chunk.get("confidence") == "low":
            continue
        chunk_id = str(chunk.get("chunk_id") or "")
        cvec = chunk_embeddings.get(chunk_id)
        if cvec is None:
            # Sidecar bijection violation (LoadPinnedCorpusWithEmbeddings
            # is supposed to guarantee this); defensive skip.
            continue
        dense_scored.append((chunk, cosine_similarity(query_vec, cvec)))
    dense_scored.sort(
        key=lambda item: (
            -item[1],
            -CONFIDENCE_RANK.get(str(item[0].get("confidence") or ""), 0),
            str(item[0].get("chunk_id") or ""),
        )
    )
    dense_pool = dense_scored[:RRF_DENSE_POOL]

    # Reciprocal Rank Fusion with k=60.
    rrf_scores: dict[str, float] = {}
    rrf_chunk: dict[str, dict[str, Any]] = {}
    for rank, (chunk, _score) in enumerate(bm25_pool):
        cid = str(chunk.get("chunk_id") or "")
        rrf_scores[cid] = rrf_scores.get(cid, 0.0) + 1.0 / (RRF_K + rank + 1)
        rrf_chunk[cid] = chunk
    for rank, (chunk, _score) in enumerate(dense_pool):
        cid = str(chunk.get("chunk_id") or "")
        rrf_scores[cid] = rrf_scores.get(cid, 0.0) + 1.0 / (RRF_K + rank + 1)
        rrf_chunk[cid] = chunk
    fused = [
        (rrf_chunk[cid], score) for cid, score in rrf_scores.items()
    ]
    fused.sort(key=lambda item: (-item[1], str(item[0].get("chunk_id") or "")))
    fused = fused[:RERANKER_POOL_SIZE]
    if not fused:
        return []

    docs = [_chunk_repr_for_rerank(chunk) for chunk, _ in fused]
    results = reranker.rerank(
        question_id=question_id,
        question=question,
        docs=docs,
        top_n=top_k,
        mode="qwen3_rrf",
    )
    reranked: list[tuple[dict[str, Any], float]] = []
    for idx, score in results:
        if idx < 0 or idx >= len(fused):
            print(
                f"  rerank: skipping out-of-range index {idx} (pool={len(fused)})",
                flush=True,
            )
            continue
        chunk, _ = fused[idx]
        reranked.append((chunk, float(score)))
    if not reranked:
        raise RuntimeError(
            f"reranker returned no usable results for question_id={question_id!r} "
            f"(pool_size={len(fused)}, server_results={len(results)})"
        )
    reranked.sort(key=lambda item: -item[1])
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

    def __init__(self, *, cache_path: Path | None, env_path: Path, model_override: str | None = None) -> None:
        self.cache_path = cache_path
        self.cache: dict[str, dict[str, Any]] = {}
        self._dirty = False
        env = _load_env(env_path)
        self.base_url = env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
        self.api_key = env["MODELVERSE_API_KEY"]
        # model_override wins so callers can pin per-mode (qwen3_full uses
        # qwen3-embedding-8b even if the env says text-embedding-3-large).
        self.model = model_override or env.get("MODELVERSE_EMBED_MODEL") or DEFAULT_EMBED_MODEL
        if cache_path is not None and cache_path.exists():
            for line in cache_path.read_text(encoding="utf-8").splitlines():
                if not line.strip():
                    continue
                row = json.loads(line)
                qid = str(row.get("question_id") or "")
                if qid:
                    self.cache[qid] = row

    def embed(self, question_id: str, question: str) -> list[float]:
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


class _Reranker:
    """Calls ModelVerse /v1/rerank with optional on-disk cache.

    Cache schema (jsonl): {"question_id","mode","docs_sha256","results":
    [{"index","score"}, ...]}. docs_sha256 lets us detect cache poisoning
    if the cosine top-N composition shifts (different mode or sidecar
    drift). Mirrors _QueryEmbedder design.
    """

    def __init__(self, *, cache_path: Path | None, env_path: Path, model: str) -> None:
        self.cache_path = cache_path
        self.cache: dict[tuple[str, str], dict[str, Any]] = {}
        self._dirty = False
        env = _load_env(env_path)
        self.base_url = env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
        self.api_key = env["MODELVERSE_API_KEY"]
        self.model = model
        if cache_path is not None and cache_path.exists():
            for line in cache_path.read_text(encoding="utf-8").splitlines():
                if not line.strip():
                    continue
                row = json.loads(line)
                qid = str(row.get("question_id") or "")
                mode = str(row.get("mode") or "")
                if qid and mode:
                    self.cache[(qid, mode)] = row

    def rerank(
        self,
        *,
        question_id: str,
        question: str,
        docs: list[str],
        top_n: int,
        mode: str,
    ) -> list[tuple[int, float]]:
        # chr(0) separator prevents collisions between docs lists that share
        # string-prefix boundaries (e.g. ["AB","C"] vs ["A","BC"]). NUL won't
        # appear in normal chunk text so the separator is unambiguous.
        docs_sha = hashlib.sha256(
            (chr(0).join(docs)).encode("utf-8")
        ).hexdigest()
        existing = self.cache.get((question_id, mode))
        if existing and str(existing.get("docs_sha256")) == docs_sha:
            cached = existing.get("results") or []
            # Defensive desc sort matches live-path safety re-sort below.
            # Protects against manual cache edits or older script versions.
            cached_results = [(int(r["index"]), float(r["score"])) for r in cached]
            cached_results.sort(key=lambda item: -item[1])
            return cached_results
        results = self._call(question, docs, top_n)
        self.cache[(question_id, mode)] = {
            "question_id": question_id,
            "mode": mode,
            "docs_sha256": docs_sha,
            "results": [{"index": idx, "score": score} for idx, score in results],
        }
        self._dirty = True
        return results

    def _call(self, query: str, docs: list[str], top_n: int) -> list[tuple[int, float]]:
        payload: dict[str, Any] = {
            "model": self.model,
            "query": query,
            "documents": docs,
        }
        if top_n > 0:
            payload["top_n"] = top_n
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        url = f"{self.base_url}/rerank"
        headers = {
            "Authorization": f"Bearer {self.api_key}",
            "Content-Type": "application/json",
        }
        backoff = 1.0
        for attempt in range(5):
            try:
                req = urllib.request.Request(url, data=body, method="POST", headers=headers)
                with urllib.request.urlopen(req, timeout=60) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
                results = data.get("results") or []
                if not results:
                    raise RuntimeError("empty reranker response")
                return [
                    (int(r.get("index", -1)), float(r.get("relevance_score", 0.0)))
                    for r in results
                ]
            except urllib.error.HTTPError as exc:
                msg = exc.read().decode("utf-8", errors="replace")
                if exc.code in (308, 429, 500, 502, 503, 504) and attempt < 4:
                    time.sleep(backoff)
                    backoff *= 2
                    continue
                raise RuntimeError(f"reranker HTTP {exc.code}: {msg[:400]}") from exc
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                if attempt < 4:
                    time.sleep(backoff)
                    backoff *= 2
                    continue
                raise
        raise RuntimeError("reranker call retry exhausted")

    def flush(self) -> None:
        if not (self._dirty and self.cache_path is not None):
            return
        self.cache_path.parent.mkdir(parents=True, exist_ok=True)
        with self.cache_path.open("w", encoding="utf-8", newline="\n") as fh:
            for key in sorted(self.cache.keys()):
                fh.write(json.dumps(self.cache[key], ensure_ascii=False) + "\n")
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
    parser.add_argument(
        "--mode",
        choices=("baseline", "hybrid", "hybrid_rerank", "qwen3_full", "qwen3_rrf"),
        default="baseline",
        help="Retrieval pipeline. See module docstring for stage breakdown.",
    )
    parser.add_argument("--embeddings-path", type=Path, default=None,
                        help="Required for --mode hybrid/hybrid_rerank/qwen3_full/qwen3_rrf: "
                             "corpus embedding sidecar from build_corpus_embeddings.py. "
                             "qwen3_full and qwen3_rrf both expect the qwen3-embedding-8b sidecar.")
    parser.add_argument("--query-embedding-cache", type=Path, default=None,
                        help="Optional jsonl cache for query embeddings keyed by question_id; speeds up re-runs")
    parser.add_argument("--reranker-cache", type=Path, default=None,
                        help="Optional jsonl cache for reranker results keyed by (question_id, mode); "
                             "speeds up re-runs of --mode hybrid_rerank/qwen3_full/qwen3_rrf")
    parser.add_argument("--embed-model", type=str, default=None,
                        help="Override embedder model. Defaults: text-embedding-3-large for "
                             "hybrid/hybrid_rerank, qwen3-embedding-8b for qwen3_full/qwen3_rrf.")
    parser.add_argument("--reranker-model", type=str, default=None,
                        help="Override reranker model. Default: qwen3-reranker-8b.")
    parser.add_argument("--env", type=Path, default=None,
                        help="Path to .env.local (defaults to .env.local in CWD); read for any mode that hits an API")
    args = parser.parse_args(argv)
    verify_psa_propagation(args.chunks)
    evaluate_retrieval(
        args.chunks,
        args.questions,
        args.out,
        mode=args.mode,
        embeddings_path=args.embeddings_path,
        query_embedding_cache_path=args.query_embedding_cache,
        reranker_cache_path=args.reranker_cache,
        embed_model=args.embed_model,
        reranker_model=args.reranker_model,
        env_path=args.env,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
