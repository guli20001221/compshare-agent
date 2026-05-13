#!/usr/bin/env python3
"""Apply deterministic W0 link policy to link_manifest.json."""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

try:
    from .common import (
        INTERNAL_PATH_RE,
        denied_internal_host,
        has_signed_query,
        is_temporary_download,
        read_json,
        write_json,
    )
except ImportError:  # pragma: no cover
    from common import (
        INTERNAL_PATH_RE,
        denied_internal_host,
        has_signed_query,
        is_temporary_download,
        read_json,
        write_json,
    )


def classify_link_manifest(link_manifest: dict[str, Any]) -> dict[str, Any]:
    links = []
    for link in link_manifest.get("links") or []:
        updated = dict(link)
        updated.update(classify_url(str(link.get("url") or "")))
        links.append(updated)
    out = dict(link_manifest)
    out["links"] = links
    out["link_count"] = len(links)
    return out


def classify_url(url: str) -> dict[str, str]:
    parsed = urlparse(url)
    host = parsed.netloc.lower()
    path = parsed.path or "/"
    if parsed.scheme not in {"http", "https"}:
        return {
            "link_type": "local_source_candidate",
            "final_state": "local_source_resolved",
            "policy_reason": "relative_or_local_link",
        }
    if denied_internal_host(host):
        return {
            "link_type": "internal_source_provenance",
            "final_state": "excluded",
            "policy_reason": "denied_internal_host",
        }
    if INTERNAL_PATH_RE.search(path):
        return {
            "link_type": "internal_admin_or_workorder",
            "final_state": "excluded",
            "policy_reason": "internal_path",
        }
    if has_signed_query(parsed) or is_temporary_download(parsed):
        return {
            "link_type": "temporary_download",
            "final_state": "excluded",
            "policy_reason": "temporary_or_signed_url",
        }
    if parsed.scheme == "https" and host == "console.compshare.cn":
        return {
            "link_type": "official_console_route",
            "final_state": "navigation_only",
            "policy_reason": "console_navigation",
        }
    if parsed.scheme == "https" and host == "www.compshare.cn" and path.startswith("/docs/"):
        return {
            "link_type": "public_official_docs",
            "final_state": "review_required",
            "policy_reason": "public_docs_require_review_or_snapshot",
        }
    return {
        "link_type": "unknown_external",
        "final_state": "review_required",
        "policy_reason": "unknown_external_url",
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--links", type=Path, required=True)
    parser.add_argument("--out", type=Path, help="output path; defaults to overwriting --links")
    args = parser.parse_args(argv)

    out_path = args.out or args.links
    write_json(out_path, classify_link_manifest(read_json(args.links)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
