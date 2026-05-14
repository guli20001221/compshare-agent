#!/usr/bin/env python3
"""Verify W0 eval questions are not byte-equal to indexed chunk terms."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


def verify_eval_questions(questions_path: Path | str, chunks_path: Path | str) -> dict[str, Any]:
    chunks = _read_jsonl(chunks_path)
    questions = _read_jsonl(questions_path)
    by_id = {str(chunk.get("chunk_id") or ""): chunk for chunk in chunks}
    violations = _find_violations(questions, by_id)
    if violations:
        details = "; ".join(violations[:20])
        raise ValueError(f"PILLAR 0 FAIL: {len(violations)} byte-equal violation(s): {details}")
    return {"question_count": len(questions), "violations": 0}


def _find_violations(questions: list[dict[str, Any]], chunks_by_id: dict[str, dict[str, Any]]) -> list[str]:
    violations: list[str] = []
    for question in questions:
        text = str(question.get("question") or "")
        for chunk_id in question.get("expected_chunk_ids") or []:
            chunk = chunks_by_id.get(str(chunk_id))
            if not chunk:
                violations.append(f"{question.get('question_id') or '<missing>'}: expected chunk {chunk_id} not found")
                continue
            if text == str(chunk.get("title") or ""):
                violations.append(f"{question.get('question_id') or '<missing>'}: byte-equal to {chunk_id} title")
            if text in {str(pattern) for pattern in chunk.get("question_patterns") or []}:
                violations.append(f"{question.get('question_id') or '<missing>'}: byte-equal to {chunk_id} question_pattern")
    return violations


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


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--questions", type=Path, required=True)
    parser.add_argument("--chunks", type=Path, required=True)
    args = parser.parse_args(argv)
    summary = verify_eval_questions(args.questions, args.chunks)
    print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
