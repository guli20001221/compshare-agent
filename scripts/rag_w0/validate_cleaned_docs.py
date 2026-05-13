#!/usr/bin/env python3
"""Validate cleaned W0 Markdown before chunking."""

from __future__ import annotations

import argparse
from pathlib import Path

try:
    from .safety_patterns import unsafe_cleaned_matches
except ImportError:  # pragma: no cover
    from safety_patterns import unsafe_cleaned_matches


def validate_cleaned_docs(path: Path | str) -> dict[str, int]:
    root = Path(path)
    if not root.exists():
        raise ValueError(f"cleaned docs path does not exist: {root}")
    docs = [root] if root.is_file() else sorted(root.glob("*.md"))
    checked = 0
    for doc_path in docs:
        text = doc_path.read_text(encoding="utf-8", errors="replace")
        body = _body_without_front_matter(text)
        matches = unsafe_cleaned_matches(body)
        if matches:
            raise ValueError(f"unsafe cleaned doc {doc_path}: {', '.join(matches)}")
        checked += 1
    return {"checked_count": checked}


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


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dir", type=Path, required=True)
    args = parser.parse_args(argv)
    validate_cleaned_docs(args.dir)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
