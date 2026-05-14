#!/usr/bin/env python3
"""Verify W0 section-label acceptance gates."""

from __future__ import annotations

import argparse
from collections import Counter
import json
from pathlib import Path
import sys
from typing import Any

try:
    from .common import ALLOWED_PRODUCT_AREAS
    from .label_sections import EMPTY_LABEL_REASONS
except ImportError:  # pragma: no cover
    from common import ALLOWED_PRODUCT_AREAS
    from label_sections import EMPTY_LABEL_REASONS


def verify_acceptance(
    *,
    pinned_path: Path | str,
    labels_path: Path | str,
    needs_split_path: Path | str,
    chunks_path: Path | str,
) -> dict[str, Any]:
    pinned = _read_json(pinned_path)
    if not isinstance(pinned, list):
        raise ValueError(f"{pinned_path}: expected a JSON array")
    labels = _read_jsonl(labels_path)
    needs_split = _read_jsonl(needs_split_path)
    chunks = _read_jsonl(chunks_path)

    _verify_pinned_sections(pinned, labels)
    _verify_empty_label_reasons(labels)
    needs_split_warning = _verify_needs_split(needs_split)
    product_area_counts = _verify_chunk_distribution(chunks)

    return {
        "pinned_count": len(pinned),
        "label_count": len(labels),
        "needs_split_count": len(needs_split),
        "needs_split_warning": needs_split_warning,
        "chunk_count": len(chunks),
        "product_area_counts": dict(sorted(product_area_counts.items())),
    }


def _verify_pinned_sections(pinned: list[Any], labels: list[dict[str, Any]]) -> None:
    for index, item in enumerate(pinned, start=1):
        if not isinstance(item, dict):
            raise ValueError(f"pinned[{index}]: expected object")
        source_doc_id = str(item.get("source_doc_id") or "")
        title_part = str(item.get("section_title_substring") or "")
        expected = str(item.get("expected_product_area") or "")
        if expected not in ALLOWED_PRODUCT_AREAS:
            raise ValueError(f"pinned[{index}]: invalid expected_product_area {expected!r}")
        matches = [
            row
            for row in labels
            if _row_source_doc_id(row) == source_doc_id and title_part and title_part in str(row.get("section_title") or "")
        ]
        if len(matches) != 1:
            raise ValueError(f"pinned[{index}]: expected exactly one match, got {len(matches)} for {source_doc_id!r} / {title_part!r}")
        actual = str(matches[0].get("selected_area") or "")
        if actual != expected:
            raise ValueError(f"pinned[{index}]: expected {expected}, got {actual or '<empty>'} for {title_part!r}")


def _verify_empty_label_reasons(labels: list[dict[str, Any]]) -> None:
    for index, row in enumerate(labels, start=1):
        selected_area = str(row.get("selected_area") or "")
        reason = str(row.get("empty_label_reason") or "")
        if selected_area:
            if selected_area not in ALLOWED_PRODUCT_AREAS:
                raise ValueError(f"labels[{index}]: invalid selected_area {selected_area!r}")
            if reason:
                raise ValueError(f"labels[{index}]: empty_label_reason must be empty when selected_area is set")
            continue
        if reason not in EMPTY_LABEL_REASONS:
            raise ValueError(f"labels[{index}]: invalid empty_label_reason {reason!r}")


def _verify_needs_split(rows: list[dict[str, Any]]) -> bool:
    seen: set[str] = set()
    for index, row in enumerate(rows, start=1):
        for field in ("section_id", "source_doc_id", "section_title", "preview_text", "flagged_at"):
            if not str(row.get(field) or "").strip():
                raise ValueError(f"needs_split[{index}]: missing {field}")
        preview_text = str(row["preview_text"])
        if len(preview_text) < 50:
            raise ValueError(f"needs_split[{index}]: preview_text must be at least 50 characters")
        section_id = str(row["section_id"])
        if section_id in seen:
            raise ValueError(f"needs_split[{index}]: duplicate section_id {section_id!r}")
        seen.add(section_id)
    if len(rows) > 5:
        print(f"warning: needs_split has {len(rows)} rows; explain this in the PR description", file=sys.stderr)
        return True
    return False


def _verify_chunk_distribution(chunks: list[dict[str, Any]]) -> Counter[str]:
    counts: Counter[str] = Counter()
    for index, chunk in enumerate(chunks, start=1):
        area = str(chunk.get("product_area") or "")
        if area not in ALLOWED_PRODUCT_AREAS:
            raise ValueError(f"chunks[{index}]: invalid product_area {area!r}")
        counts[area] += 1
    if len(chunks) < 100:
        raise ValueError(f"chunks: expected at least 100 chunks, got {len(chunks)}")
    if counts["init_failure"] < 2:
        raise ValueError(f"chunks: init_failure must be >= 2, got {counts['init_failure']}")
    if counts["billing_rule"] < 2:
        raise ValueError(f"chunks: billing_rule must be >= 2, got {counts['billing_rule']}")
    non_empty = sum(1 for area in ALLOWED_PRODUCT_AREAS if counts[area] > 0)
    if non_empty < 6:
        raise ValueError(f"chunks: expected at least 6 non-empty product areas, got {non_empty}")
    return counts


def _row_source_doc_id(row: dict[str, Any]) -> str:
    key = row.get("key")
    if isinstance(key, dict):
        return str(key.get("source_doc_id") or "")
    return str(row.get("source_doc_id") or "")


def _read_json(path: Path | str) -> Any:
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        return json.load(fh)


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line_number, line in enumerate(fh, start=1):
            if not line.strip():
                continue
            row = json.loads(line)
            if not isinstance(row, dict):
                raise ValueError(f"{path}:{line_number}: expected object")
            rows.append(row)
    return rows


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pinned", type=Path, default=Path(__file__).with_name("pinned_sections.json"))
    parser.add_argument("--labels", type=Path, required=True)
    parser.add_argument("--needs-split", type=Path, required=True)
    parser.add_argument("--chunks", type=Path, required=True)
    args = parser.parse_args(argv)
    summary = verify_acceptance(
        pinned_path=args.pinned,
        labels_path=args.labels,
        needs_split_path=args.needs_split,
        chunks_path=args.chunks,
    )
    print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
