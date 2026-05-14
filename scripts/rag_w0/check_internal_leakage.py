#!/usr/bin/env python3
"""Scan W0 chunks for internal data leakage before deploy promotion."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
from typing import Any


STAFF_NAMES_PATH = Path(__file__).with_name("staff_names.txt")
INTERNAL_CASE_RE = re.compile(r"\b(?:wxwork-spt-record-|internal_case[:/]|spt-\d{3,}|spt-record)\b", re.IGNORECASE)
INTERNAL_PATH_RE = re.compile(r"/(?:admin|workorder|internal)(?:/|$)", re.IGNORECASE)
INTERNAL_EMAIL_RE = re.compile(r"@[A-Za-z0-9._-]*(?:internal|staff)[A-Za-z0-9._-]*\.(?:compshare|ucloud)\.(?:cn|com)", re.IGNORECASE)


def check_internal_leakage(chunks_path: Path | str) -> dict[str, Any]:
    chunks = _read_jsonl(chunks_path)
    staff_names = _load_staff_names()
    flagged: list[dict[str, Any]] = []
    for chunk in chunks:
        findings = check_chunk(chunk, staff_names=staff_names)
        if findings:
            flagged.append({"chunk_id": str(chunk.get("chunk_id") or ""), "findings": findings})
    return {"chunk_count": len(chunks), "flagged_count": len(flagged), "flagged": flagged}


def check_chunk(chunk: dict[str, Any], *, staff_names: set[str] | None = None) -> list[str]:
    staff_names = staff_names if staff_names is not None else _load_staff_names()
    text = "\n".join(
        [
            str(chunk.get("title") or ""),
            str(chunk.get("content") or ""),
            " ".join(str(item) for item in chunk.get("question_patterns") or []),
            " ".join(str(item) for item in chunk.get("source_refs") or []),
        ]
    )
    findings: list[str] = []
    for name in sorted(staff_names):
        if name and name in text:
            findings.append(f"staff_name:{name}")
    for label, pattern in (
        ("internal_case", INTERNAL_CASE_RE),
        ("internal_path", INTERNAL_PATH_RE),
        ("internal_email", INTERNAL_EMAIL_RE),
    ):
        for match in pattern.finditer(text):
            findings.append(f"{label}:{match.group(0)}")
    return findings


def _load_staff_names() -> set[str]:
    if not STAFF_NAMES_PATH.exists():
        return set()
    return {line.strip() for line in STAFF_NAMES_PATH.read_text(encoding="utf-8").splitlines() if line.strip() and not line.strip().startswith("#")}


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
    parser.add_argument("--chunks", type=Path, required=True)
    parser.add_argument("--out", type=Path)
    parser.add_argument("--report-only", action="store_true")
    args = parser.parse_args(argv)
    summary = check_internal_leakage(args.chunks)
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps({"chunk_count": summary["chunk_count"], "flagged_count": summary["flagged_count"]}, ensure_ascii=False, sort_keys=True))
    if summary["flagged_count"] and not args.report_only:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
