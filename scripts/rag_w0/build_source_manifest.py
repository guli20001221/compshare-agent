#!/usr/bin/env python3
"""Build and validate the Stage 2B RAG W0 source manifest."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

try:
    from .common import SOURCE_MANIFEST_SCHEMA, read_json, tree_digest, write_json
except ImportError:  # pragma: no cover - direct script execution
    from common import SOURCE_MANIFEST_SCHEMA, read_json, tree_digest, write_json


SPT_SOURCE_ID = "wxwork-spt-record-2026-05"


def build_source_manifest(manifest_path: Path | str) -> dict[str, Any]:
    manifest_path = Path(manifest_path)
    raw = read_json(manifest_path)
    sources = raw.get("sources")
    if not isinstance(sources, list):
        raise ValueError("manifest sources must be a list")

    built_sources: list[dict[str, Any]] = []
    spt_seen = False
    total_files = 0
    total_bytes = 0
    for source in sources:
        built = _build_source_entry(source)
        built_sources.append(built)
        total_files += built["file_count"]
        total_bytes += built["byte_count"]
        if built["id"] == SPT_SOURCE_ID:
            spt_seen = True
            _validate_spt_source(source, built)

    if not spt_seen:
        raise ValueError(f"required internal case source {SPT_SOURCE_ID!r} is missing")

    return {
        "schema_version": SOURCE_MANIFEST_SCHEMA,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "bundle_root": raw.get("bundle_root") or str(manifest_path.parent),
        "bundle_manifest_path": str(manifest_path),
        "source_count": len(built_sources),
        "total_file_count": total_files,
        "total_byte_count": total_bytes,
        "sources": built_sources,
    }


def _build_source_entry(source: dict[str, Any]) -> dict[str, Any]:
    source_id = _required_str(source, "id")
    paths = _declared_paths(source)
    if not paths:
        raise ValueError(f"source {source_id}: no path, zip_path, or extracted_path")

    path_stats: dict[str, dict[str, Any]] = {}
    file_count = 0
    byte_count = 0
    for key, value in paths.items():
        path = Path(value)
        if not path.exists():
            label = "source path" if key == "path" else key
            raise ValueError(f"source {source_id}: missing {label}: {path}")
        count, size, digest = tree_digest(path)
        path_stats[key] = {
            "path": str(path),
            "kind": "dir" if path.is_dir() else "file",
            "file_count": count,
            "byte_count": size,
            "sha256": digest,
        }
        if key in {"path", "extracted_path"} or len(paths) == 1:
            file_count += count
            byte_count += size

    return {
        "id": source_id,
        "type": source.get("type", ""),
        "include_status": source.get("include_status", ""),
        "customer_safe": str(source.get("customer_safe", "")),
        "paths": paths,
        "path_stats": path_stats,
        "file_count": file_count,
        "byte_count": byte_count,
        "revision": source.get("revision"),
        "title": source.get("title"),
    }


def _declared_paths(source: dict[str, Any]) -> dict[str, str]:
    out: dict[str, str] = {}
    for key in ("path", "zip_path", "extracted_path"):
        value = source.get(key)
        if isinstance(value, str) and value.strip():
            out[key] = value
    return out


def _validate_spt_source(source: dict[str, Any], built: dict[str, Any]) -> None:
    if source.get("type") != "internal_case_chat_export":
        raise ValueError(f"{SPT_SOURCE_ID}: type must be internal_case_chat_export")
    if str(source.get("customer_safe")) != "false":
        raise ValueError(f"{SPT_SOURCE_ID}: customer_safe must be false")
    if source.get("include_status") != "internal_reference_only_needs_customer_safe_split":
        raise ValueError(f"{SPT_SOURCE_ID}: include_status must be internal_reference_only_needs_customer_safe_split")
    if "path" not in built["paths"]:
        raise ValueError(f"{SPT_SOURCE_ID}: path is required")


def _required_str(source: dict[str, Any], key: str) -> str:
    value = source.get(key)
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"source missing required field {key}")
    return value


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, help="source bundle directory containing manifest.json")
    parser.add_argument("--manifest", type=Path, help="explicit source bundle manifest path")
    parser.add_argument("--out", type=Path, required=True, help="output source_manifest.json path")
    args = parser.parse_args(argv)

    if args.manifest:
        manifest_path = args.manifest
    elif args.bundle:
        manifest_path = args.bundle / "manifest.json"
    else:
        parser.error("--bundle or --manifest is required")

    write_json(args.out, build_source_manifest(manifest_path))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
