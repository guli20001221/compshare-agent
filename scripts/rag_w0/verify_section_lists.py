#!/usr/bin/env python3
"""Verify parsed W0 section lists against pinned Opus bootstrap anchors."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


def verify_section_lists(sections_path: Path | str, pinned_path: Path | str) -> dict[str, int]:
    live = _section_index(sections_path)
    pinned = _read_pinned(pinned_path)
    for source in pinned:
        source_ref = str(source.get("source_ref") or "")
        if source_ref not in live:
            raise ValueError(f"{source_ref}: missing live section list")
        live_sections = live[source_ref]
        expected_sections = source.get("sections") or []
        expected_count = int(source.get("expected_section_count", len(expected_sections)))
        if len(live_sections) != expected_count:
            raise ValueError(f"{source_ref}: section_count mismatch expected {expected_count}, got {len(live_sections)}")
        for expected, actual in zip(expected_sections, live_sections):
            index = int(expected.get("section_index"))
            if int(actual.get("section_index")) != index:
                raise ValueError(f"{source_ref}: section_index mismatch expected {index}, got {actual.get('section_index')}")
            expected_heading = str(expected.get("heading_text") or "")
            actual_heading = str(actual.get("heading_text") or "")
            if actual_heading != expected_heading:
                raise ValueError(f"{source_ref}: section {index} heading mismatch expected {expected_heading!r}, got {actual_heading!r}")
            if list(actual.get("heading_path") or []) != list(expected.get("heading_path") or []):
                raise ValueError(f"{source_ref}: section {index} heading_path mismatch")
            if sorted(actual.get("risk_flags") or []) != sorted(expected.get("risk_flags") or []):
                raise ValueError(f"{source_ref}: section {index} risk_flags mismatch")
    return {
        "source_count": len(pinned),
        "section_count": sum(len(source.get("sections") or []) for source in pinned),
    }


def _section_index(path: Path | str) -> dict[str, list[dict[str, Any]]]:
    out: dict[str, list[dict[str, Any]]] = {}
    for row in _read_jsonl(path):
        out.setdefault(str(row.get("source_ref") or ""), []).append(row)
    for values in out.values():
        values.sort(key=lambda item: int(item["section_index"]))
    return out


def _read_pinned(path: Path | str) -> list[dict[str, Any]]:
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        data = json.load(fh)
    sources = data.get("sources") if isinstance(data, dict) else data
    if not isinstance(sources, list):
        raise ValueError(f"{path}: expected sources list")
    return sources


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sections", type=Path, required=True)
    parser.add_argument("--pinned", type=Path, required=True)
    args = parser.parse_args(argv)
    verify_section_lists(args.sections, args.pinned)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
