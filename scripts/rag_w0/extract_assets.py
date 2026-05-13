#!/usr/bin/env python3
"""Extract first-pass asset and link manifests from W0 source documents."""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import Any

try:
    from .common import (
        ASSET_MANIFEST_SCHEMA,
        HEADING_RE,
        IMAGE_EXTENSIONS,
        IMAGE_RE,
        LINK_MANIFEST_SCHEMA,
        LINK_RE,
        MARKDOWN_EXTENSIONS,
        is_url,
        read_json,
        sha256_file,
        stable_id,
        strip_angle_brackets,
        write_json,
    )
except ImportError:  # pragma: no cover
    from common import (
        ASSET_MANIFEST_SCHEMA,
        HEADING_RE,
        IMAGE_EXTENSIONS,
        IMAGE_RE,
        LINK_MANIFEST_SCHEMA,
        LINK_RE,
        MARKDOWN_EXTENSIONS,
        is_url,
        read_json,
        sha256_file,
        stable_id,
        strip_angle_brackets,
        write_json,
    )


def extract_assets_and_links(source_manifest: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
    assets: list[dict[str, Any]] = []
    links: list[dict[str, Any]] = []
    seen_hashes: dict[str, str] = {}

    for source in source_manifest.get("sources") or []:
        source_id = source.get("id") or "unknown"
        paths = source.get("paths") or {}
        for doc_path in _markdown_files(paths):
            doc_assets, doc_links = _extract_from_markdown(source_id, doc_path, seen_hashes)
            assets.extend(doc_assets)
            links.extend(doc_links)
        for image_path in _extracted_images(paths):
            asset = _asset_from_extracted_image(source_id, image_path, seen_hashes)
            assets.append(asset)

    return (
        {
            "schema_version": ASSET_MANIFEST_SCHEMA,
            "asset_count": len(assets),
            "assets": assets,
        },
        {
            "schema_version": LINK_MANIFEST_SCHEMA,
            "link_count": len(links),
            "links": links,
        },
    )


def _markdown_files(paths: dict[str, str]) -> list[Path]:
    out: list[Path] = []
    for key in ("path", "extracted_path"):
        value = paths.get(key)
        if not value:
            continue
        path = Path(value)
        if path.is_file() and path.suffix.lower() in MARKDOWN_EXTENSIONS:
            out.append(path)
        elif path.is_dir():
            out.extend(sorted(p for p in path.rglob("*") if p.suffix.lower() in MARKDOWN_EXTENSIONS and p.is_file()))
    return out


def _extracted_images(paths: dict[str, str]) -> list[Path]:
    value = paths.get("extracted_path")
    if not value:
        return []
    root = Path(value)
    if not root.is_dir():
        return []
    return sorted(p for p in root.rglob("*") if p.suffix.lower() in IMAGE_EXTENSIONS and p.is_file())


def _extract_from_markdown(
    source_id: str,
    doc_path: Path,
    seen_hashes: dict[str, str],
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    assets: list[dict[str, Any]] = []
    links: list[dict[str, Any]] = []
    headings: list[str] = []
    lines = doc_path.read_text(encoding="utf-8", errors="replace").splitlines()
    for line_no, line in enumerate(lines, start=1):
        heading = HEADING_RE.match(line)
        if heading:
            level = len(heading.group(1))
            headings = headings[: level - 1] + [heading.group(2).strip()]
        for match in IMAGE_RE.finditer(line):
            alt, ref = match.groups()
            ref = strip_angle_brackets(ref)
            assets.append(_asset_from_markdown_ref(source_id, doc_path, line_no, headings, line, alt, ref, seen_hashes))
        for match in LINK_RE.finditer(line):
            text, url = match.groups()
            url = strip_angle_brackets(url)
            links.append(
                {
                    "link_id": stable_id(source_id, doc_path, line_no, url),
                    "source_id": source_id,
                    "source_path": str(doc_path),
                    "line": line_no,
                    "heading_path": headings[:],
                    "text": text,
                    "url": url,
                    "link_type": "unknown",
                    "final_state": "unknown",
                }
            )
    return assets, links


def _asset_from_markdown_ref(
    source_id: str,
    doc_path: Path,
    line_no: int,
    headings: list[str],
    line: str,
    alt: str,
    ref: str,
    seen_hashes: dict[str, str],
) -> dict[str, Any]:
    asset_id = stable_id(source_id, doc_path, line_no, ref)
    resolved_path: Path | None = None
    digest = None
    duplicate_of = None
    if is_url(ref):
        final_state = "external_asset_snapshot_required"
    else:
        resolved_path = (doc_path.parent / ref).resolve()
        if not resolved_path.exists():
            final_state = "missing_asset"
        else:
            digest = sha256_file(resolved_path)
            low_value_state = _low_value_state(alt, ref)
            if low_value_state:
                final_state = low_value_state
            elif digest in seen_hashes:
                final_state = "duplicate_asset"
                duplicate_of = seen_hashes[digest]
            else:
                final_state = "included_with_ocr_note"
                seen_hashes[digest] = asset_id
    return {
        "asset_id": asset_id,
        "source_id": source_id,
        "source_doc_id": str(doc_path),
        "source_path": str(doc_path),
        "line": line_no,
        "heading_path": headings[:],
        "image_ref": ref,
        "image_path": str(resolved_path) if resolved_path else None,
        "nearby_text": line.strip(),
        "visual_type": "unknown",
        "description": "",
        "highlighted_ui": "",
        "user_action": "",
        "expected_input": "",
        "next_step": "",
        "caveats": "",
        "confidence": "low",
        "include_in_rag": final_state in {"included_with_ocr_note", "included_with_vl_note"},
        "final_state": final_state,
        "exclusion_reason": _exclusion_reason(final_state),
        "sha256": digest,
        "duplicate_of": duplicate_of,
    }


def _asset_from_extracted_image(source_id: str, image_path: Path, seen_hashes: dict[str, str]) -> dict[str, Any]:
    digest = sha256_file(image_path)
    asset_id = stable_id(source_id, image_path)
    low_value_state = _low_value_state("", image_path.name)
    duplicate_of = None
    if low_value_state:
        final_state = low_value_state
    elif digest in seen_hashes:
        final_state = "duplicate_asset"
        duplicate_of = seen_hashes[digest]
    else:
        final_state = "included_with_ocr_note"
        seen_hashes[digest] = asset_id
    return {
        "asset_id": asset_id,
        "source_id": source_id,
        "source_doc_id": None,
        "source_path": str(image_path),
        "line": None,
        "heading_path": [],
        "image_ref": image_path.name,
        "image_path": str(image_path),
        "nearby_text": "",
        "visual_type": "unknown",
        "description": "",
        "highlighted_ui": "",
        "user_action": "",
        "expected_input": "",
        "next_step": "",
        "caveats": "",
        "confidence": "low",
        "include_in_rag": final_state in {"included_with_ocr_note", "included_with_vl_note"},
        "final_state": final_state,
        "exclusion_reason": _exclusion_reason(final_state),
        "sha256": digest,
        "duplicate_of": duplicate_of,
    }


def _low_value_state(alt: str, ref: str) -> str | None:
    text = f"{alt} {ref}".lower()
    if any(word in text for word in ("qr", "qrcode", "二维码", "群", "group")):
        return "excluded_qr_or_group"
    if any(word in text for word in ("logo", "decorative", "placeholder")):
        return "excluded_low_value"
    return None


def _exclusion_reason(final_state: str) -> str:
    return {
        "excluded_qr_or_group": "qr_or_group",
        "excluded_low_value": "low_value",
        "missing_asset": "missing_asset",
        "external_asset_snapshot_required": "external_snapshot_required",
        "external_asset_snapshot_failed": "external_snapshot_failed",
        "duplicate_asset": "duplicate_asset",
    }.get(final_state, "")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-manifest", type=Path, required=True)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--links", type=Path, required=True)
    args = parser.parse_args(argv)

    asset_manifest, link_manifest = extract_assets_and_links(read_json(args.source_manifest))
    write_json(args.assets, asset_manifest)
    write_json(args.links, link_manifest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
