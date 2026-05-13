#!/usr/bin/env python3
"""Validate W0 asset_manifest.json and link_manifest.json."""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import Any

try:
    from .common import (
        ALLOWED_ASSET_FINAL_STATES,
        ALLOWED_LINK_FINAL_STATES,
        ASSET_MANIFEST_SCHEMA,
        LINK_MANIFEST_SCHEMA,
        read_json,
    )
except ImportError:  # pragma: no cover
    from common import (
        ALLOWED_ASSET_FINAL_STATES,
        ALLOWED_LINK_FINAL_STATES,
        ASSET_MANIFEST_SCHEMA,
        LINK_MANIFEST_SCHEMA,
        read_json,
    )


def validate_assets(asset_path: Path | str, link_path: Path | str) -> dict[str, int]:
    assets = read_json(asset_path)
    links = read_json(link_path)
    if assets.get("schema_version") != ASSET_MANIFEST_SCHEMA:
        raise ValueError(f"asset schema_version must be {ASSET_MANIFEST_SCHEMA}")
    if links.get("schema_version") != LINK_MANIFEST_SCHEMA:
        raise ValueError(f"link schema_version must be {LINK_MANIFEST_SCHEMA}")
    seen_assets: set[str] = set()
    for idx, asset in enumerate(assets.get("assets") or [], start=1):
        asset_id = asset.get("asset_id")
        if not asset_id:
            raise ValueError(f"asset {idx}: missing asset_id")
        if asset_id in seen_assets:
            raise ValueError(f"duplicate asset_id {asset_id}")
        seen_assets.add(asset_id)
        state = asset.get("final_state")
        if state not in ALLOWED_ASSET_FINAL_STATES:
            raise ValueError(f"asset {asset_id}: invalid final_state {state!r}")
        if state in {"included_with_ocr_note", "included_with_vl_note"} and not asset.get("image_path"):
            raise ValueError(f"asset {asset_id}: included asset must have image_path")
    seen_links: set[str] = set()
    for idx, link in enumerate(links.get("links") or [], start=1):
        link_id = link.get("link_id")
        if not link_id:
            raise ValueError(f"link {idx}: missing link_id")
        if link_id in seen_links:
            raise ValueError(f"duplicate link_id {link_id}")
        seen_links.add(link_id)
        if not link.get("link_type"):
            raise ValueError(f"link {link_id}: missing link_type")
        state = link.get("final_state")
        if state not in ALLOWED_LINK_FINAL_STATES:
            raise ValueError(f"link {link_id}: invalid final_state {state!r}")
    return {"asset_count": len(seen_assets), "link_count": len(seen_links)}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--links", type=Path, required=True)
    args = parser.parse_args(argv)
    validate_assets(args.assets, args.links)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
