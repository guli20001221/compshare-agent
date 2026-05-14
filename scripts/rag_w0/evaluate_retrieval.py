#!/usr/bin/env python3
"""Evaluate deterministic W0 retrieval quality."""

from __future__ import annotations

import argparse
from collections import defaultdict
import json
from pathlib import Path
from typing import Any

try:
    from .retrieval_scoring import BM25Index, DEFAULT_THRESHOLD, normalize_text, tokenize_text
    from .validate_chunks import validate_chunks
except ImportError:  # pragma: no cover
    from retrieval_scoring import BM25Index, DEFAULT_THRESHOLD, normalize_text, tokenize_text
    from validate_chunks import validate_chunks


DEFAULT_TOP_K = 3
ANSWER_BEHAVIOR = "answer"
CONFIDENCE_RANK = {"high": 2, "medium": 1, "low": 0}


def evaluate_retrieval(
    chunks_path: Path | str,
    questions_path: Path | str,
    out_path: Path | str,
    *,
    top_k: int = DEFAULT_TOP_K,
    threshold: float = DEFAULT_THRESHOLD,
) -> dict[str, Any]:
    validate_chunks(chunks_path)
    chunks = _read_jsonl(chunks_path)
    index = BM25Index(chunks)
    questions = _read_jsonl(questions_path)
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

    top_3_hit_rate = None if evaluated == 0 else (evaluated - len(failed_questions)) / evaluated
    summary = {
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
    args = parser.parse_args(argv)
    verify_psa_propagation(args.chunks)
    evaluate_retrieval(args.chunks, args.questions, args.out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
