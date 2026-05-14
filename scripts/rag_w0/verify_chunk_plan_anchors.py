#!/usr/bin/env python3
"""Verify live W0 chunk plans against pinned Opus bootstrap anchors."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

try:
    from .chunk_plan import normalize_product_area
except ImportError:  # pragma: no cover
    from chunk_plan import normalize_product_area


def verify_chunk_plan_anchors(plans_path: Path | str, pinned_path: Path | str) -> dict[str, int]:
    plans = {str(row.get("source_doc_id") or ""): row for row in _read_jsonl(plans_path)}
    pinned = _read_pinned(pinned_path)
    for anchor in pinned:
        source_doc_id = str(anchor.get("source_doc_id") or "")
        plan = plans.get(source_doc_id)
        if not plan:
            raise ValueError(f"{source_doc_id}: missing live chunk plan")
        expected_strategy = str(anchor.get("expected_strategy") or "")
        if plan.get("strategy") != expected_strategy:
            raise ValueError(f"{source_doc_id}: strategy mismatch expected {expected_strategy!r}, got {plan.get('strategy')!r}")
        expected_count = anchor.get("expected_chunk_count")
        if expected_count is not None and len(plan.get("chunks") or []) != int(expected_count):
            raise ValueError(f"{source_doc_id}: chunk_count mismatch expected {expected_count}, got {len(plan.get('chunks') or [])}")
        expected_areas = _expected_areas(anchor)
        if expected_areas:
            live_areas = {normalize_product_area(chunk.get("product_area")) for chunk in plan.get("chunks") or []}
            if live_areas != expected_areas:
                raise ValueError(f"{source_doc_id}: product_area mismatch expected {sorted(expected_areas)}, got {sorted(live_areas)}")
    return {"anchor_count": len(pinned)}


def _expected_areas(anchor: dict[str, Any]) -> set[str]:
    raw = anchor.get("expected_product_areas_set") or anchor.get("expected_product_areas") or []
    return {normalize_product_area(value) for value in raw}


def _read_pinned(path: Path | str) -> list[dict[str, Any]]:
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        data = json.load(fh)
    anchors = data.get("anchors") if isinstance(data, dict) else data
    if not isinstance(anchors, list):
        raise ValueError(f"{path}: expected anchors list")
    return anchors


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plans", type=Path, required=True)
    parser.add_argument("--pinned", type=Path, required=True)
    args = parser.parse_args(argv)
    verify_chunk_plan_anchors(args.plans, args.pinned)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
