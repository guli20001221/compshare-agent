#!/usr/bin/env python3
"""Snapshot external W0 image assets before VL processing."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path
from typing import Any, Callable
from urllib.parse import urlparse
from urllib.request import Request, urlopen

try:
    from .common import read_json, stable_id, write_json
except ImportError:  # pragma: no cover
    from common import read_json, stable_id, write_json


Fetcher = Callable[[str], tuple[bytes, str]]


def snapshot_external_assets(
    asset_manifest: dict[str, Any],
    snapshot_root: Path | str,
    *,
    fetcher: Fetcher | None = None,
) -> dict[str, Any]:
    root = Path(snapshot_root)
    root.mkdir(parents=True, exist_ok=True)
    if fetcher is None:
        fetcher = _network_fetcher

    assets: list[dict[str, Any]] = []
    success_count = 0
    failure_count = 0
    for asset in asset_manifest.get("assets") or []:
        updated = dict(asset)
        if updated.get("final_state") != "external_asset_snapshot_required":
            assets.append(updated)
            continue
        url = str(updated.get("image_ref") or "")
        try:
            body, content_type = fetcher(url)
            digest = hashlib.sha256(body).hexdigest()
            rel = _snapshot_rel_path(updated, url, content_type)
            target = root / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(body)
            updated["final_state"] = "included_with_ocr_note"
            updated["include_in_rag"] = True
            updated["image_path"] = str(target)
            updated["sha256"] = digest
            updated["snapshot_status"] = "success"
            updated["snapshot_path"] = rel.as_posix()
            updated["snapshot_sha256"] = digest
            updated["snapshot_content_type"] = content_type
            updated["snapshot_byte_count"] = len(body)
            updated.pop("snapshot_failure_reason", None)
            success_count += 1
        except Exception as exc:  # pragma: no cover - exercised through injected fetcher in tests
            updated["snapshot_status"] = "failed"
            updated["snapshot_failure_reason"] = str(exc)
            failure_count += 1
        assets.append(updated)

    out = dict(asset_manifest)
    out["assets"] = assets
    out["asset_count"] = len(assets)
    out["snapshot_success_count"] = success_count
    out["snapshot_failure_count"] = failure_count
    return out


def _snapshot_rel_path(asset: dict[str, Any], url: str, content_type: str) -> Path:
    suffix = Path(urlparse(url).path).suffix.lower()
    if suffix not in {".jpg", ".jpeg", ".png", ".webp", ".gif"}:
        suffix = _suffix_for_content_type(content_type)
    asset_id = str(asset.get("asset_id") or stable_id(url))
    return Path(f"{asset_id}{suffix}")


def _suffix_for_content_type(content_type: str) -> str:
    lowered = content_type.lower()
    if "png" in lowered:
        return ".png"
    if "webp" in lowered:
        return ".webp"
    if "gif" in lowered:
        return ".gif"
    return ".jpg"


def _network_fetcher(raw_url: str) -> tuple[bytes, str]:
    request = Request(raw_url, headers={"User-Agent": "compshare-rag-w0-asset-snapshot/1.0"})
    with urlopen(request, timeout=30) as response:  # nosec: offline tool, explicit source allowlist
        return response.read(), response.headers.get_content_type()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--snapshot-root", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args(argv)

    updated = snapshot_external_assets(read_json(args.assets), args.snapshot_root)
    write_json(args.out, updated)
    if updated["snapshot_failure_count"]:
        raise RuntimeError(f"{updated['snapshot_failure_count']} external assets failed to snapshot")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
