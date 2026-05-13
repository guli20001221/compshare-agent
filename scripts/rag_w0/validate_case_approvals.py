#!/usr/bin/env python3
"""Validate approval records for mined internal-case candidates."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


REQUIRED_FIELDS = {
    "case_id",
    "source_hash",
    "redaction_status",
    "rewrite_path",
    "approved_by",
    "approved_at",
    "allowed_product_area",
    "blocked_phrases_checked",
    "final_runtime_chunk_id",
}


def validate_case_approvals(cases: list[dict[str, Any]], approvals: list[dict[str, Any]]) -> dict[str, int]:
    known_hashes = {case.get("redacted_case_hash") for case in cases}
    known_case_ids = {case.get("case_id") for case in cases}
    approved = 0
    seen: set[str] = set()
    for idx, approval in enumerate(approvals, start=1):
        for field in sorted(REQUIRED_FIELDS):
            if field not in approval:
                raise ValueError(f"approval {idx}: missing {field}")
        case_id = approval.get("case_id")
        if case_id not in known_case_ids:
            raise ValueError(f"approval {idx}: unknown case_id")
        source_hash = approval.get("source_hash")
        if not source_hash:
            raise ValueError(f"approval {idx}: missing source_hash")
        if source_hash not in known_hashes:
            raise ValueError(f"approval {idx}: unknown source_hash")
        if approval.get("redaction_status") != "redacted":
            raise ValueError(f"approval {idx}: redaction_status must be redacted")
        if not isinstance(approval.get("blocked_phrases_checked"), bool):
            raise ValueError(f"approval {idx}: blocked_phrases_checked must be boolean")
        if approval.get("approval_status") != "approved":
            raise ValueError(f"approval {idx}: approval_status must be approved")
        if not approval.get("approved_by"):
            raise ValueError(f"approval {idx}: approved_by is required")
        if not approval.get("approved_at"):
            raise ValueError(f"approval {idx}: approved_at is required")
        if source_hash in seen:
            raise ValueError(f"approval {idx}: duplicate source_hash")
        seen.add(str(source_hash))
        approved += 1
    return {"approved_count": approved}


def read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cases", type=Path, required=True)
    parser.add_argument("--approvals", type=Path, required=True)
    args = parser.parse_args(argv)
    validate_case_approvals(read_jsonl(args.cases), read_jsonl(args.approvals))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
