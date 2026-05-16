#!/usr/bin/env python3
"""Retrofit W0 knowledge JSONL rows for schema v1.

Adds source_origin=official to production chunks, bumps kb_version, and keeps
surface_url/source_url exactly on their current paths:

- surface_url is preserved as-is, including null.
- source_url is not added when absent.

The transform is idempotent. Re-running it should produce byte-identical output.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path


DEFAULT_KB_VERSION = "kb.stage2b.w0.2026-05-16.schema1"
DEFAULT_SOURCE_ORIGIN = "official"


def retrofit_jsonl(path: Path, kb_version: str = DEFAULT_KB_VERSION) -> None:
    rows: list[str] = []
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        if not raw_line.strip():
            continue
        row = json.loads(raw_line)
        row["source_origin"] = row.get("source_origin") or DEFAULT_SOURCE_ORIGIN
        row["kb_version"] = kb_version
        rows.append(json.dumps(row, ensure_ascii=False))
    path.write_text("\n".join(rows) + "\n", encoding="utf-8", newline="\n")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "paths",
        nargs="*",
        default=["deploy/kb/stage2b_w0.jsonl"],
        help="JSONL corpus files to retrofit",
    )
    parser.add_argument("--kb-version", default=DEFAULT_KB_VERSION)
    args = parser.parse_args()

    for path_text in args.paths:
        retrofit_jsonl(Path(path_text), args.kb_version)


if __name__ == "__main__":
    main()
