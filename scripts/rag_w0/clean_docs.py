#!/usr/bin/env python3
"""Clean normalized W0 Markdown into customer-facing drafts."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path

try:
    from .safety_patterns import redact_customer_text, unsafe_cleaned_matches
except ImportError:  # pragma: no cover
    from safety_patterns import redact_customer_text, unsafe_cleaned_matches


def clean_documents(normalized_dir: Path | str, out_dir: Path | str) -> dict[str, int]:
    src = Path(normalized_dir)
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    cleaned_count = 0
    skipped_count = 0
    review_count = 0
    for doc_path in sorted(src.glob("*.md")):
        text = doc_path.read_text(encoding="utf-8", errors="replace")
        meta = _front_matter_values(text)
        if meta.get("include_status") != "include_after_cleaning":
            skipped_count += 1
            continue
        body = _body_without_front_matter(text)
        cleaned_body, needs_review = clean_text(body)
        cleaned = _safe_front_matter(meta) + cleaned_body.lstrip()
        if needs_review:
            review_count += 1
        (out / doc_path.name).write_text(cleaned, encoding="utf-8")
        cleaned_count += 1
    return {"cleaned_count": cleaned_count, "skipped_count": skipped_count, "review_count": review_count}


def clean_text(text: str) -> tuple[str, bool]:
    cleaned = redact_customer_text(text)
    remaining = unsafe_cleaned_matches(cleaned)
    needs_review = bool(remaining)
    if needs_review:
        cleaned = cleaned.rstrip() + "\n\n<!-- review_required: unsafe_pattern_remaining -->\n"
    return cleaned, needs_review


def _front_matter_values(text: str) -> dict[str, str]:
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return {}
    out: dict[str, str] = {}
    for line in lines[1:]:
        if line.strip() == "---":
            break
        if ":" in line:
            key, value = line.split(":", 1)
            out[key.strip()] = value.strip()
    return out


def _body_without_front_matter(text: str) -> str:
    lines = text.splitlines(keepends=True)
    while lines and lines[0].strip() == "---":
        end = None
        for idx, line in enumerate(lines[1:], start=1):
            if line.strip() == "---":
                end = idx
                break
        if end is None:
            return "".join(lines)
        lines = lines[end + 1 :]
        while lines and not lines[0].strip():
            lines = lines[1:]
    return "".join(lines)


def _safe_front_matter(meta: dict[str, str]) -> str:
    raw = "|".join(
        [
            meta.get("source_id", ""),
            meta.get("source_type", ""),
            meta.get("source_path", ""),
        ]
    )
    source_hash = hashlib.sha256(raw.encode("utf-8")).hexdigest()
    return "\n".join(
        [
            "---",
            f"source_trace_hash: {source_hash}",
            "safety_state: customer_safe_cleaned",
            f"include_status: {meta.get('include_status', '')}",
            "---",
            "",
        ]
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--normalized-dir", type=Path, required=True)
    parser.add_argument("--out-dir", type=Path, required=True)
    args = parser.parse_args(argv)
    clean_documents(args.normalized_dir, args.out_dir)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
