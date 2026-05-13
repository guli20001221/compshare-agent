#!/usr/bin/env python3
"""Mine redacted candidate cases from Enterprise WeChat support exports."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
from typing import Any

try:
    from .safety_patterns import redact_customer_text
except ImportError:  # pragma: no cover
    from safety_patterns import redact_customer_text


def mine_cases(raw_text: str, *, source_id: str) -> list[dict[str, Any]]:
    cases: list[dict[str, Any]] = []
    for idx, block in enumerate(_blocks(raw_text), start=1):
        redacted = redact_case_text(block)
        label = _label_case(redacted)
        issue = _first_nonempty_line(redacted)
        case = {
            "case_id": f"{source_id}:case-{idx:04d}",
            "source_id": source_id,
            "label": label,
            "issue_pattern": issue,
            "symptoms": _symptoms(redacted),
            "likely_cause": "",
            "resolution": _resolution(redacted),
            "user_safe_answer_candidate": _answer_candidate(redacted, label),
            "redacted_text": redacted,
            "redacted_case_hash": _hash(redacted),
        }
        cases.append(case)
    return cases


def approval_record(case: dict[str, Any], *, reviewer: str, product_area: str = "") -> dict[str, Any]:
    return {
        "case_id": str(case["case_id"]),
        "source_hash": str(case["redacted_case_hash"]),
        "redaction_status": "redacted",
        "rewrite_path": "",
        "approved_by": reviewer,
        "approved_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat(),
        "allowed_product_area": product_area,
        "blocked_phrases_checked": True,
        "final_runtime_chunk_id": "",
        "approval_status": "approved",
    }


def approval_templates(cases: list[dict[str, Any]]) -> list[dict[str, Any]]:
    templates: list[dict[str, Any]] = []
    for case in cases:
        if case.get("label") != "faq_candidate":
            continue
        templates.append(
            {
                "case_id": str(case["case_id"]),
                "source_hash": str(case["redacted_case_hash"]),
                "redaction_status": "redacted",
                "rewrite_path": "",
                "approved_by": "",
                "approved_at": "",
                "allowed_product_area": "",
                "blocked_phrases_checked": False,
                "final_runtime_chunk_id": "",
                "approval_status": "needs_review",
            }
        )
    return templates


def write_jsonl(path: Path | str, rows: list[dict[str, Any]]) -> None:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")


def redact_case_text(text: str) -> str:
    text = redact_customer_text(text, redact_all_urls=True)
    return _collapse_blank_lines(text)


def _blocks(raw_text: str) -> list[str]:
    chunks = [chunk.strip() for chunk in re.split(r"\n\s*\n+", raw_text) if chunk.strip()]
    if chunks:
        return chunks
    return [raw_text.strip()] if raw_text.strip() else []


def _label_case(redacted: str) -> str:
    lowered = redacted.lower()
    faq_markers = (
        "buy",
        "purchase",
        "resource package",
        "billing",
        "invoice",
        "refund",
        "login",
        "rdp",
        "ssh",
        "jupyter",
        "\u8d2d\u4e70",
        "\u8d44\u6e90\u5305",
        "\u5957\u9910",
        "\u8ba1\u8d39",
        "\u767b\u5f55",
        "\u8fdc\u7a0b",
    )
    eval_markers = (
        "init failed",
        "cannot",
        "error",
        "failed",
        "gpu",
        "cuda",
        "driver",
        "\u5de5\u5355",
        "\u521d\u59cb\u5316\u5931\u8d25",
        "\u62a5\u9519",
    )
    if any(word in lowered or word in redacted for word in faq_markers):
        return "faq_candidate"
    if any(word in lowered or word in redacted for word in eval_markers):
        return "eval_only"
    return "internal_only"


def _first_nonempty_line(text: str) -> str:
    for line in text.splitlines():
        if line.strip():
            return line.strip()
    return ""


def _symptoms(text: str) -> list[str]:
    markers = ("fail", "failed", "error", "cannot", "how", "\u5931\u8d25", "\u62a5\u9519", "\u65e0\u6cd5", "\u4e0d\u80fd", "\u600e\u4e48")
    return [line.strip() for line in text.splitlines() if any(word in line.lower() or word in line for word in markers)][:5]


def _resolution(text: str) -> str:
    markers = ("guide", "restart", "check", "contact", "confirm", "\u5f15\u5bfc", "\u91cd\u542f", "\u67e5\u770b", "\u8054\u7cfb", "\u786e\u8ba4")
    for line in text.splitlines():
        if any(word in line.lower() or word in line for word in markers):
            return line.strip()
    return ""


def _answer_candidate(text: str, label: str) -> str:
    if label != "faq_candidate":
        return ""
    resolution = _resolution(text)
    return resolution or _first_nonempty_line(text)


def _hash(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _collapse_blank_lines(text: str) -> str:
    return re.sub(r"\n{3,}", "\n\n", text).strip()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--source-id", required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--approval-template-out", type=Path)
    args = parser.parse_args(argv)

    cases = mine_cases(args.source.read_text(encoding="utf-8", errors="replace"), source_id=args.source_id)
    write_jsonl(args.out, cases)
    if args.approval_template_out:
        write_jsonl(args.approval_template_out, approval_templates(cases))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
