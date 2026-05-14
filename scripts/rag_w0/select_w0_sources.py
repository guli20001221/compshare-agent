#!/usr/bin/env python3
"""Select the curated W0 normalized docs before cleaning."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

try:
    from .common import ALLOWED_PRODUCT_AREAS, read_json, write_json
except ImportError:  # pragma: no cover
    from common import ALLOWED_PRODUCT_AREAS, read_json, write_json


ALLOWLIST_SCHEMA = "rag_w0_gitlab_source_allowlist.v1"
GITLAB_SOURCE_ID = "gitlab-compshare-docs"


def select_w0_sources(
    normalized_dir: Path | str,
    allowlist_path: Path | str,
    out_dir: Path | str,
    summary_path: Path | str | None = None,
    *,
    links_path: Path | str | None = None,
    links_out_path: Path | str | None = None,
    assets_path: Path | str | None = None,
    assets_out_path: Path | str | None = None,
) -> dict[str, Any]:
    src = Path(normalized_dir)
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    allowlist = _load_allowlist(allowlist_path)
    selected_keys = set(allowlist)
    matched_keys: set[tuple[str, str]] = set()
    product_area_counts: dict[str, int] = {}
    copied_existing_count = 0
    selected_gitlab_count = 0
    skipped_gitlab_count = 0
    selected_source_paths: set[str] = set()

    for doc_path in sorted(src.glob("*.md")):
        text = doc_path.read_text(encoding="utf-8", errors="replace")
        meta = _front_matter_values(text)
        source_path = meta.get("source_path", "")
        source_id = meta.get("source_id", "")
        if source_id != GITLAB_SOURCE_ID:
            if meta.get("include_status") == "include_after_cleaning":
                (out / doc_path.name).write_text(text, encoding="utf-8")
                copied_existing_count += 1
                if source_path:
                    selected_source_paths.add(source_path)
            continue

        key = _allowlist_key_for_doc(meta, selected_keys)
        if not key:
            skipped_gitlab_count += 1
            continue
        entry = allowlist[key]
        updated = _rewrite_front_matter(
            text,
            {
                "include_status": "include_after_cleaning",
                "source_selection_product_area": entry["product_area"],
                "source_selection_reason": entry["reason"],
            },
        )
        (out / doc_path.name).write_text(updated, encoding="utf-8")
        matched_keys.add(key)
        selected_gitlab_count += 1
        if source_path:
            selected_source_paths.add(source_path)
        product_area_counts[entry["product_area"]] = product_area_counts.get(entry["product_area"], 0) + 1

    missing = sorted(selected_keys - matched_keys)
    if missing:
        missing_text = ", ".join(f"{source_id}:{doc_path}" for source_id, doc_path in missing)
        raise ValueError(f"allowlisted docs were not found in normalized input: {missing_text}")

    summary = {
        "schema_version": "rag_w0.selected_sources.v1",
        "copied_existing_count": copied_existing_count,
        "selected_gitlab_count": selected_gitlab_count,
        "skipped_gitlab_count": skipped_gitlab_count,
        "selected_source_paths": sorted(selected_source_paths),
        "product_area_counts": dict(sorted(product_area_counts.items())),
    }
    selected_normalized_paths = {_normalize_doc_path(path) for path in selected_source_paths}
    if links_path or links_out_path:
        if not links_path or not links_out_path:
            raise ValueError("links_path and links_out_path must be provided together")
        _write_filtered_links(links_path, links_out_path, selected_normalized_paths)
    if assets_path or assets_out_path:
        if not assets_path or not assets_out_path:
            raise ValueError("assets_path and assets_out_path must be provided together")
        _write_filtered_assets(assets_path, assets_out_path, selected_normalized_paths)
    if summary_path:
        write_json(summary_path, summary)
    return summary


def _load_allowlist(path: Path | str) -> dict[tuple[str, str], dict[str, str]]:
    data = read_json(path)
    if data.get("schema_version") != ALLOWLIST_SCHEMA:
        raise ValueError(f"{path}: schema_version must be {ALLOWLIST_SCHEMA}")
    rows = data.get("sources")
    if not isinstance(rows, list) or not rows:
        raise ValueError(f"{path}: sources must be a non-empty list")
    out: dict[tuple[str, str], dict[str, str]] = {}
    for idx, row in enumerate(rows, start=1):
        if not isinstance(row, dict):
            raise ValueError(f"{path}: source row {idx} must be an object")
        source_id = _required(row, "source_id", path, idx)
        doc_path = _normalize_doc_path(_required(row, "doc_path", path, idx))
        product_area = _required(row, "product_area", path, idx)
        reason = _required(row, "reason", path, idx)
        if product_area not in ALLOWED_PRODUCT_AREAS:
            raise ValueError(f"{path}: row {idx}: invalid product_area {product_area!r}")
        key = (source_id, doc_path)
        if key in out:
            raise ValueError(f"{path}: duplicate allowlist entry {source_id}:{doc_path}")
        out[key] = {
            "source_id": source_id,
            "doc_path": doc_path,
            "product_area": product_area,
            "reason": reason,
        }
    return out


def _required(row: dict[str, Any], key: str, path: Path | str, idx: int) -> str:
    value = row.get(key)
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{path}: row {idx}: missing {key}")
    return value.strip()


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


def _rewrite_front_matter(text: str, updates: dict[str, str]) -> str:
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        raise ValueError("normalized doc is missing front matter")
    end = None
    for idx, line in enumerate(lines[1:], start=1):
        if line.strip() == "---":
            end = idx
            break
    if end is None:
        raise ValueError("normalized doc has unterminated front matter")

    seen: set[str] = set()
    front = ["---"]
    for line in lines[1:end]:
        if ":" not in line:
            front.append(line)
            continue
        key, _ = line.split(":", 1)
        key = key.strip()
        if key in updates:
            front.append(f"{key}: {updates[key]}")
            seen.add(key)
        else:
            front.append(line)
    for key, value in updates.items():
        if key not in seen:
            front.append(f"{key}: {value}")
    front.append("---")
    return "\n".join(front + lines[end + 1 :]).rstrip() + "\n"


def _allowlist_key_for_doc(meta: dict[str, str], selected_keys: set[tuple[str, str]]) -> tuple[str, str] | None:
    source_id = meta.get("source_id", "")
    source_path = _normalize_doc_path(meta.get("source_path", ""))
    for key_source_id, key_doc_path in selected_keys:
        if source_id == key_source_id and source_path.endswith(key_doc_path):
            return (key_source_id, key_doc_path)
    return None


def _write_filtered_links(path: Path | str, out_path: Path | str, selected_source_paths: set[str]) -> None:
    data = read_json(path)
    links = []
    for link in data.get("links") or []:
        source_path = _normalize_doc_path(str(link.get("source_path") or ""))
        if source_path in selected_source_paths:
            links.append(link)
    out = dict(data)
    out["links"] = links
    out["link_count"] = len(links)
    write_json(out_path, out)


def _write_filtered_assets(path: Path | str, out_path: Path | str, selected_source_paths: set[str]) -> None:
    data = read_json(path)
    assets = []
    for asset in data.get("assets") or []:
        source_doc_id = _normalize_doc_path(str(asset.get("source_doc_id") or ""))
        if source_doc_id in selected_source_paths:
            assets.append(asset)
    out = dict(data)
    out["assets"] = assets
    out["asset_count"] = len(assets)
    write_json(out_path, out)


def _normalize_doc_path(value: str) -> str:
    return value.replace("\\", "/").strip().strip("/").lower()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--normalized-dir", type=Path, required=True)
    parser.add_argument("--allowlist", type=Path, required=True)
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--summary", type=Path)
    parser.add_argument("--links", type=Path)
    parser.add_argument("--links-out", type=Path)
    parser.add_argument("--assets", type=Path)
    parser.add_argument("--assets-out", type=Path)
    args = parser.parse_args(argv)
    select_w0_sources(
        args.normalized_dir,
        args.allowlist,
        args.out_dir,
        args.summary,
        links_path=args.links,
        links_out_path=args.links_out,
        assets_path=args.assets,
        assets_out_path=args.assets_out,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
