#!/usr/bin/env python3
"""Snapshot W0 review-required links when network access is explicitly allowed."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path
from typing import Any, Callable
from urllib.request import Request, urlopen

try:
    from .common import read_json, stable_id, write_json
except ImportError:  # pragma: no cover
    from common import read_json, stable_id, write_json


Fetcher = Callable[[str], tuple[bytes, str]]


def snapshot_link_manifest(
    link_manifest: dict[str, Any],
    snapshot_root: Path | str,
    *,
    fetcher: Fetcher | None = None,
    allow_network: bool = False,
) -> dict[str, Any]:
    root = Path(snapshot_root)
    root.mkdir(parents=True, exist_ok=True)
    if fetcher is None and allow_network:
        fetcher = _network_fetcher

    links: list[dict[str, Any]] = []
    for link in link_manifest.get("links") or []:
        updated = dict(link)
        if updated.get("final_state") != "review_required":
            links.append(updated)
            continue
        if fetcher is None:
            updated["snapshot_status"] = "not_attempted"
            updated["snapshot_failure_reason"] = "network_disabled"
            links.append(updated)
            continue
        try:
            body, content_type = fetcher(str(updated.get("url") or ""))
            rel = Path(f"{stable_id(updated.get('link_id'), updated.get('url'))}.snapshot")
            (root / rel).write_bytes(body)
            updated["final_state"] = "snapshotted"
            updated["snapshot_status"] = "success"
            updated["snapshot_path"] = rel.as_posix()
            updated["snapshot_sha256"] = hashlib.sha256(body).hexdigest()
            updated["snapshot_content_type"] = content_type
            updated["snapshot_byte_count"] = len(body)
            updated.pop("snapshot_failure_reason", None)
        except Exception as exc:  # pragma: no cover - exercised by tests via injected fetcher
            updated["snapshot_status"] = "failed"
            updated["snapshot_failure_reason"] = str(exc)
        links.append(updated)

    out = dict(link_manifest)
    out["links"] = links
    out["link_count"] = len(links)
    return out


def _network_fetcher(raw_url: str) -> tuple[bytes, str]:
    request = Request(raw_url, headers={"User-Agent": "compshare-rag-w0-snapshot/1.0"})
    with urlopen(request, timeout=15) as response:  # nosec: offline tool, explicit allow_network
        return response.read(), response.headers.get_content_type()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--links", type=Path, required=True)
    parser.add_argument("--snapshot-root", type=Path, required=True)
    parser.add_argument("--out", type=Path, help="output path; defaults to overwriting --links")
    parser.add_argument("--allow-network", action="store_true")
    args = parser.parse_args(argv)

    out_path = args.out or args.links
    updated = snapshot_link_manifest(read_json(args.links), args.snapshot_root, allow_network=args.allow_network)
    write_json(out_path, updated)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
