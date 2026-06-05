#!/usr/bin/env python3
"""Run retrieval eval on the EXTERNAL corpus, bypassing the platform-only PSA
guard in evaluate_retrieval.main (verify_psa_propagation expects a CompShare
error-code chunk, which the external corpus deliberately has none of).

Calls evaluate_retrieval(...) directly — same scoring pipeline as the platform
eval — and prints top_3_hit_rate + failures. Gate: top_3_hit_rate >= 0.85.
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))  # scripts/ on path
from rag_w0.evaluate_retrieval import evaluate_retrieval  # noqa: E402


def _assert_golden_targets_exist(chunks_path: str, questions_path: str) -> None:
    """Fail loud if any golden expected_chunk_id is absent from the corpus.

    Without this, a renamed/dropped chunk turns into a silent permanent miss that
    looks like a retrieval-quality problem (it happened once: ext-vllm-cuda-error
    / -startup-hang were dropped from the corpus but left in the golden).
    """
    corpus_ids = {
        json.loads(line)["chunk_id"]
        for line in open(chunks_path, encoding="utf-8")
        if line.strip()
    }
    missing = []
    for line in open(questions_path, encoding="utf-8"):
        if not line.strip():
            continue
        row = json.loads(line)
        for cid in row.get("expected_chunk_ids") or []:
            if cid not in corpus_ids:
                missing.append((row.get("question_id"), cid))
    if missing:
        raise SystemExit(f"golden references chunk_ids not in corpus: {missing}")


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--chunks", required=True)
    p.add_argument("--questions", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--embeddings-path", required=True)
    p.add_argument("--mode", default="qwen3_rrf")
    p.add_argument("--env", default=None)
    a = p.parse_args()
    _assert_golden_targets_exist(a.chunks, a.questions)
    summary = evaluate_retrieval(
        a.chunks,
        a.questions,
        a.out,
        mode=a.mode,
        embeddings_path=a.embeddings_path,
        env_path=a.env,
    )
    rate = summary.get("top_3_hit_rate")
    print(json.dumps({
        "mode": summary.get("mode"),
        "questions_evaluated": summary.get("questions_evaluated"),
        "top_3_hit_rate": rate,
        "per_group_hit_rate": summary.get("per_group_hit_rate"),
        "failed_questions": [
            {"q": f["question"], "expected": f["expected_chunk_ids"], "got": f["actual_top3_ids"]}
            for f in summary.get("failed_questions") or []
        ],
    }, ensure_ascii=False, indent=2))
    gate = 0.85
    if rate is None or rate < gate:
        print(f"GATE FAIL: top_3_hit_rate {rate} < {gate}")
        return 1
    print(f"GATE PASS: top_3_hit_rate {rate} >= {gate}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
