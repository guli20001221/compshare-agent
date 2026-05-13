#!/usr/bin/env python3
"""Validate a generated Stage 2B RAG W0 source_manifest.json."""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import Any

try:
    from .build_source_manifest import SPT_SOURCE_ID
    from .common import SOURCE_MANIFEST_SCHEMA, read_json
except ImportError:  # pragma: no cover
    from build_source_manifest import SPT_SOURCE_ID
    from common import SOURCE_MANIFEST_SCHEMA, read_json


def validate_source_manifest(path: Path | str) -> dict[str, Any]:
    manifest = read_json(path)
    if manifest.get("schema_version") != SOURCE_MANIFEST_SCHEMA:
        raise ValueError(f"schema_version must be {SOURCE_MANIFEST_SCHEMA}")
    sources = manifest.get("sources")
    if not isinstance(sources, list) or not sources:
        raise ValueError("sources must be a non-empty list")
    seen_ids: set[str] = set()
    spt_seen = False
    for idx, source in enumerate(sources, start=1):
        source_id = source.get("id")
        if not source_id:
            raise ValueError(f"source {idx}: missing id")
        if source_id in seen_ids:
            raise ValueError(f"duplicate source id {source_id}")
        seen_ids.add(source_id)
        paths = source.get("paths")
        if not isinstance(paths, dict) or not paths:
            raise ValueError(f"source {source_id}: missing paths")
        for key, value in paths.items():
            if not Path(value).exists():
                raise ValueError(f"source {source_id}: missing {key}: {value}")
        if source_id == SPT_SOURCE_ID:
            spt_seen = True
            if source.get("type") != "internal_case_chat_export":
                raise ValueError(f"{SPT_SOURCE_ID}: type must be internal_case_chat_export")
            if source.get("include_status") != "internal_reference_only_needs_customer_safe_split":
                raise ValueError(f"{SPT_SOURCE_ID}: include_status must be internal_reference_only_needs_customer_safe_split")
            if source.get("customer_safe") != "false":
                raise ValueError(f"{SPT_SOURCE_ID}: customer_safe must be false")
    if not spt_seen:
        raise ValueError(f"required internal case source {SPT_SOURCE_ID!r} is missing")
    return {"source_count": len(sources)}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    args = parser.parse_args(argv)
    validate_source_manifest(args.manifest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
