#!/usr/bin/env python3
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime
import hashlib
import json
from pathlib import Path
import time
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from scripts.rag_w0.build_corpus_embeddings import chunk_repr, embed_batch, load_env
from scripts.rag_w0.evaluate_retrieval import (
    BM25Index,
    CONFIDENCE_RANK,
    RERANKER_POOL_SIZE,
    RRF_BM25_POOL,
    RRF_DENSE_POOL,
    RRF_K,
    _chunk_repr_for_rerank,
    _load_chunk_embedding_sidecar,
    _retrieve,
    cosine_similarity,
)


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def extract_queries(paths: list[Path], start: datetime, end: datetime) -> list[dict[str, str]]:
    traces: dict[str, dict[str, Any]] = {}
    for path in paths:
        for row in read_jsonl(path):
            created = datetime.fromisoformat(str(row["created_at"]).replace("Z", "+00:00"))
            if start <= created <= end:
                traces[str(row["id"])] = row
    by_query: dict[str, dict[str, str]] = {}
    for row in traces.values():
        retrieval = ((row.get("trace_json") or {}).get("retrieval") or {})
        if not retrieval.get("enabled"):
            continue
        query = str(retrieval.get("query_normalized") or retrieval.get("query_raw") or "").strip()
        if not query:
            continue
        created = datetime.fromisoformat(str(row["created_at"]).replace("Z", "+00:00"))
        item = {
            "case_id": "real-" + hashlib.sha256(query.encode("utf-8")).hexdigest()[:16],
            "query": query,
            "date": created.date().isoformat(),
        }
        old = by_query.get(query)
        if old is None or item["date"] < old["date"]:
            by_query[query] = item
    return list(by_query.values())


def stratified_sample(cases: list[dict[str, str]], per_day: int) -> list[dict[str, str]]:
    days: dict[str, list[dict[str, str]]] = defaultdict(list)
    for case in cases:
        days[case["date"]].append(case)
    sample: list[dict[str, str]] = []
    for day in sorted(days):
        sample.extend(sorted(days[day], key=lambda item: item["case_id"])[:per_day])
    return sample


def rrf_pool(query: str, vector: list[float], chunks: list[dict[str, Any]], index: BM25Index, embeddings: dict[str, list[float]]) -> list[dict[str, Any]]:
    bm25 = _retrieve(question=query, product_area="", chunks=chunks, index=index, top_k=RRF_BM25_POOL, threshold=0.0)
    dense = []
    for chunk in chunks:
        cvec = embeddings[str(chunk["chunk_id"])]
        dense.append((chunk, cosine_similarity(vector, cvec)))
    dense.sort(key=lambda item: (-item[1], -CONFIDENCE_RANK.get(str(item[0].get("confidence") or ""), 0), str(item[0]["chunk_id"])))
    scores: dict[str, float] = {}
    lookup: dict[str, dict[str, Any]] = {}
    for rank, (chunk, _) in enumerate(bm25):
        cid = str(chunk["chunk_id"]); scores[cid] = scores.get(cid, 0.0) + 1 / (RRF_K + rank + 1); lookup[cid] = chunk
    for rank, (chunk, _) in enumerate(dense[:RRF_DENSE_POOL]):
        cid = str(chunk["chunk_id"]); scores[cid] = scores.get(cid, 0.0) + 1 / (RRF_K + rank + 1); lookup[cid] = chunk
    return [lookup[cid] for cid in sorted(scores, key=lambda cid: (-scores[cid], cid))[:RERANKER_POOL_SIZE]]


def post_json(url: str, body: dict[str, Any], api_key: str, timeout: int = 120) -> dict[str, Any]:
    encoded = json.dumps(body, ensure_ascii=False).encode("utf-8")
    last: Exception | None = None
    for attempt in range(4):
        try:
            request = Request(url, data=encoded, method="POST", headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"})
            with urlopen(request, timeout=timeout) as response:
                return json.loads(response.read().decode("utf-8"))
        except (HTTPError, URLError, TimeoutError, ConnectionError) as exc:
            last = exc
            if attempt < 3:
                time.sleep(2 ** attempt)
    raise RuntimeError(f"request failed after retries: {last}")


def rerank_case(case: dict[str, Any], *, base_url: str, api_key: str, model: str) -> dict[str, Any]:
    docs = [_chunk_repr_for_rerank(chunk) for chunk in case["pool"]]
    response = post_json(base_url + "/rerank", {"model": model, "query": case["query"], "documents": docs, "top_n": 3}, api_key, timeout=45)
    results = sorted(response.get("results") or [], key=lambda item: -float(item.get("relevance_score", 0)))
    case["top3"] = [case["pool"][int(item["index"])] for item in results[:3]]
    case.pop("pool", None)
    return case


def judge_batch(batch: list[dict[str, Any]], *, base_url: str, api_key: str, model: str) -> list[dict[str, Any]]:
    payload = []
    for case in batch:
        payload.append({
            "case_id": case["case_id"],
            "question": case["query"],
            "top3": [{"chunk_id": row["chunk_id"], "title": row.get("title"), "content": str(row.get("content") or "")[:700]} for row in case["top3"]],
        })
    prompt = (
        "你是严格的真实 RAG 检索裁判。逐项判断，不生成答案，不复述问题。"
        "kb_applicable 表示该问题是否能由公开平台/技术文档回答；账号实时状态、执行操作、寒暄为 false。"
        "retrieval_grade 只能是 full/partial/miss：前三段足以完整回答为 full，提供关键但不完整证据为 partial，无有效证据为 miss。"
        "输出 JSON，仅含 results 数组；每项仅含 case_id、kb_applicable(boolean)、retrieval_grade、reason_category。"
        "reason_category 只能是 grounded/partial_evidence/no_evidence/not_knowledge_question。\n"
        + json.dumps(payload, ensure_ascii=False)
    )
    raw = post_json(base_url + "/chat/completions", {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0,
        "max_tokens": 1800,
        "response_format": {"type": "json_object"},
    }, api_key, timeout=240)
    parsed = json.loads(raw["choices"][0]["message"]["content"])
    return list(parsed.get("results") or [])


def main() -> int:
    parser = argparse.ArgumentParser(description="Evaluate RAG V2 on real production retrieval queries without committing raw chat text.")
    parser.add_argument("--traces", type=Path, action="append", required=True)
    parser.add_argument("--corpus", type=Path, action="append", required=True)
    parser.add_argument("--embeddings", type=Path, action="append", required=True)
    parser.add_argument("--start", required=True)
    parser.add_argument("--end", required=True)
    parser.add_argument("--per-day", type=int, default=10)
    parser.add_argument("--selected-cases", type=Path)
    parser.add_argument("--env", type=Path, required=True)
    parser.add_argument("--private-output", type=Path, required=True)
    parser.add_argument("--public-output", type=Path, required=True)
    parser.add_argument("--reranker-model", default="qwen3-reranker-8b")
    parser.add_argument("--judge-model", default="doubao-seed-2-1-turbo-260628")
    parser.add_argument("--workers", type=int, default=8)
    args = parser.parse_args()

    start = datetime.fromisoformat(args.start); end = datetime.fromisoformat(args.end)
    population = extract_queries(args.traces, start, end)
    selected_meta: dict[str, dict[str, Any]] = {}
    if args.selected_cases:
        selected_rows = json.loads(args.selected_cases.read_text(encoding="utf-8"))["cases"]
        selected_meta = {str(row["case_id"]): row for row in selected_rows}
        population_by_id = {row["case_id"]: row for row in population}
        missing = sorted(set(selected_meta) - set(population_by_id))
        if missing:
            raise ValueError(f"selected case IDs are absent from real query population: {missing}")
        cases = [population_by_id[case_id] | {"category": selected_meta[case_id]["category"]} for case_id in selected_meta]
    else:
        cases = stratified_sample(population, args.per_day)
    print(f"population={len(population)} sample={len(cases)}", flush=True)
    chunks = [row for path in args.corpus for row in read_jsonl(path)]
    embeddings: dict[str, list[float]] = {}
    for path in args.embeddings:
        embeddings.update(_load_chunk_embedding_sidecar(path))
    if set(embeddings) != {str(row["chunk_id"]) for row in chunks}:
        raise ValueError("combined corpus and embedding sidecars are not a bijection")

    env = load_env(args.env); base_url = env.get("MODELVERSE_BASE_URL", "https://api.modelverse.cn/v1").rstrip("/"); api_key = env["MODELVERSE_API_KEY"]
    vectors, _ = embed_batch([case["query"] for case in cases], base_url=base_url, api_key=api_key, model="qwen3-embedding-8b")
    print(f"embedded={len(vectors)}", flush=True)
    index = BM25Index(chunks)
    for case, vector in zip(cases, vectors):
        case["pool"] = rrf_pool(case["query"], vector, chunks, index, embeddings)
    rerank_cache_path = args.private_output.parent / "rag_v2_real_chat_rerank_cache.jsonl"
    rerank_cache = {row["case_id"]: row for row in read_jsonl(rerank_cache_path)} if rerank_cache_path.exists() else {}
    pending_cases = [case for case in cases if case["case_id"] not in rerank_cache]
    with ThreadPoolExecutor(max_workers=args.workers) as pool, rerank_cache_path.open("a", encoding="utf-8") as cache_file:
        futures = [pool.submit(rerank_case, case, base_url=base_url, api_key=api_key, model=args.reranker_model) for case in pending_cases]
        for completed, future in enumerate(as_completed(futures), start=1):
            case = future.result(); rerank_cache[case["case_id"]] = case
            cache_file.write(json.dumps(case, ensure_ascii=False) + "\n"); cache_file.flush()
            if completed % 20 == 0 or completed == len(futures): print(f"reranked_new={completed}/{len(futures)}", flush=True)
    cases = [rerank_cache[case["case_id"]] | ({"category": selected_meta[case["case_id"]]["category"]} if selected_meta else {}) for case in cases]
    cases.sort(key=lambda item: item["case_id"])

    batches = [cases[i:i + 6] for i in range(0, len(cases), 6)]
    judge_cache_path = args.private_output.parent / "rag_v2_real_chat_judge_cache.jsonl"
    case_ids = {case["case_id"] for case in cases}
    cached_verdicts = {row["case_id"]: row for row in read_jsonl(judge_cache_path) if row.get("case_id") in case_ids} if judge_cache_path.exists() else {}
    batches = [[case for case in batch if case["case_id"] not in cached_verdicts] for batch in batches]
    batches = [batch for batch in batches if batch]
    verdicts: list[dict[str, Any]] = list(cached_verdicts.values())
    with ThreadPoolExecutor(max_workers=min(args.workers, len(batches) or 1)) as pool, judge_cache_path.open("a", encoding="utf-8") as cache_file:
        futures = [pool.submit(judge_batch, batch, base_url=base_url, api_key=api_key, model=args.judge_model) for batch in batches]
        for completed, future in enumerate(as_completed(futures), start=1):
            items = future.result(); verdicts.extend(items)
            for item in items: cache_file.write(json.dumps(item, ensure_ascii=False) + "\n")
            cache_file.flush(); print(f"judged_batches={completed}/{len(futures)}", flush=True)
    verdict_by_id = {str(item["case_id"]): item for item in verdicts}
    if set(verdict_by_id) != {case["case_id"] for case in cases}:
        raise ValueError("judge did not return exactly one verdict per sampled case")

    private_rows = []
    for case in cases:
        verdict = verdict_by_id[case["case_id"]]
        private_rows.append({"case_id": case["case_id"], "date": case["date"], "category": case.get("category"), "query": case["query"], "top3_chunk_ids": [row["chunk_id"] for row in case["top3"]], "judge": verdict})
    args.private_output.parent.mkdir(parents=True, exist_ok=True)
    args.private_output.write_text("\n".join(json.dumps(row, ensure_ascii=False) for row in private_rows) + "\n", encoding="utf-8")

    applicable = [row for row in private_rows if row["judge"].get("kb_applicable") is True]
    grades = Counter(str(row["judge"].get("retrieval_grade")) for row in applicable)
    public = {
        "schema_version": "compshare.rag.real-chat-eval.v1",
        "window": {"start": args.start, "end": args.end},
        "privacy": "Raw queries and customer identifiers are not committed; case IDs are one-way query hashes. Private details remain in ignored local output.",
        "source": {"deduplicated_real_retrieval_queries": len(population), "sample_method": "manual RAG-relevance and topic-diversity review" if args.selected_cases else f"deterministic hash sample, up to {args.per_day} unique queries per day", "sampled": len(cases), "days": len(set(case["date"] for case in cases))},
        "models": {"embedding": "qwen3-embedding-8b", "reranker": args.reranker_model, "judge": args.judge_model},
        "metrics": {
            "kb_applicable": len(applicable),
            "not_kb_applicable": len(cases) - len(applicable),
            "full": grades["full"],
            "partial": grades["partial"],
            "miss": grades["miss"],
            "full_rate": grades["full"] / len(applicable) if applicable else None,
            "coverage_rate_full_or_partial": (grades["full"] + grades["partial"]) / len(applicable) if applicable else None,
        },
        "per_day": dict(sorted(Counter(case["date"] for case in cases).items())),
        "per_category": dict(sorted(Counter(str(case.get("category") or "unclassified") for case in cases).items())),
        "cases": [{"case_id": row["case_id"], "date": row["date"], "category": row["category"], "top3_chunk_ids": row["top3_chunk_ids"], "judge": row["judge"]} for row in private_rows],
    }
    args.public_output.parent.mkdir(parents=True, exist_ok=True)
    args.public_output.write_text(json.dumps(public, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(public["source"] | public["metrics"], ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
