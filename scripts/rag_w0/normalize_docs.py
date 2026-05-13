#!/usr/bin/env python3
"""Normalize W0 source Markdown and insert deterministic asset notes."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

try:
    from .common import IMAGE_RE, MARKDOWN_EXTENSIONS, read_json
    from .describe_images import read_jsonl
except ImportError:  # pragma: no cover
    from common import IMAGE_RE, MARKDOWN_EXTENSIONS, read_json
    from describe_images import read_jsonl


def normalize_documents(
    source_manifest: dict[str, Any],
    asset_manifest: dict[str, Any],
    link_manifest: dict[str, Any],
    asset_notes: list[dict[str, Any]],
    out_dir: Path | str,
) -> dict[str, int]:
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    notes_by_asset = {note.get("asset_id"): note for note in asset_notes}
    assets_by_doc_line: dict[tuple[str, int], list[dict[str, Any]]] = {}
    standalone_by_source: dict[str, list[dict[str, Any]]] = {}
    for asset in asset_manifest.get("assets") or []:
        note = notes_by_asset.get(asset.get("asset_id"))
        if not note or not note.get("include_in_rag"):
            continue
        doc_id = asset.get("source_doc_id")
        line = asset.get("line")
        if doc_id and line:
            assets_by_doc_line.setdefault((str(doc_id), int(line)), []).append({**asset, "note": note})
        elif asset.get("source_id"):
            standalone_by_source.setdefault(str(asset.get("source_id")), []).append({**asset, "note": note})

    count = 0
    for source in source_manifest.get("sources") or []:
        if source.get("type") == "internal_case_chat_export" or str(source.get("customer_safe")) == "false":
            continue
        source_id = str(source.get("id") or "unknown")
        for doc_path in _source_markdown_files(source):
            normalized = _normalize_markdown_doc(source, doc_path, assets_by_doc_line)
            target = out / f"{_safe_name(source_id)}__{_safe_name(doc_path.stem)}.md"
            target.write_text(normalized, encoding="utf-8")
            count += 1
        standalone = standalone_by_source.get(source_id) or []
        if standalone:
            target = out / f"{_safe_name(source_id)}__standalone-assets.md"
            target.write_text(_standalone_asset_doc(source, standalone), encoding="utf-8")
            count += 1
    return {"normalized_count": count, "link_count": len(link_manifest.get("links") or [])}


def _source_markdown_files(source: dict[str, Any]) -> list[Path]:
    paths = source.get("paths") or {}
    out: list[Path] = []
    for key in ("path", "extracted_path"):
        value = paths.get(key)
        if not value:
            continue
        path = Path(value)
        if path.is_file() and path.suffix.lower() in MARKDOWN_EXTENSIONS:
            out.append(path)
        elif path.is_dir():
            out.extend(sorted(p for p in path.rglob("*") if p.is_file() and p.suffix.lower() in MARKDOWN_EXTENSIONS))
    return out


def _normalize_markdown_doc(
    source: dict[str, Any],
    doc_path: Path,
    assets_by_doc_line: dict[tuple[str, int], list[dict[str, Any]]],
) -> str:
    lines = doc_path.read_text(encoding="utf-8", errors="replace").splitlines()
    output: list[str] = [_front_matter(source, doc_path)]
    for line_no, line in enumerate(lines, start=1):
        assets = assets_by_doc_line.get((str(doc_path), line_no), [])
        if assets and IMAGE_RE.search(line):
            for asset in assets:
                output.append(_asset_note_block(asset))
            continue
        output.append(line.rstrip())
    return "\n".join(output).rstrip() + "\n"


def _standalone_asset_doc(source: dict[str, Any], assets: list[dict[str, Any]]) -> str:
    output = [_front_matter(source, None), "# Standalone asset notes", ""]
    for asset in assets:
        output.append(_asset_note_block(asset))
    return "\n".join(output).rstrip() + "\n"


def _front_matter(source: dict[str, Any], doc_path: Path | None) -> str:
    values = {
        "source_id": source.get("id"),
        "source_type": source.get("type"),
        "safety_state": source.get("customer_safe"),
        "include_status": source.get("include_status"),
        "source_path": str(doc_path) if doc_path else "",
    }
    body = ["---"]
    for key, value in values.items():
        body.append(f"{key}: {value or ''}")
    body.extend(["---", ""])
    return "\n".join(body)


def _asset_note_block(asset: dict[str, Any]) -> str:
    note = asset["note"]
    payload = {
        "asset_id": asset.get("asset_id"),
        "image_ref": asset.get("image_ref"),
        "description": note.get("description"),
        "include_in_rag": note.get("include_in_rag"),
        "visual_type": note.get("visual_type"),
        "highlighted_ui": note.get("highlighted_ui"),
        "user_action": note.get("user_action"),
        "expected_input": note.get("expected_input"),
        "next_step": note.get("next_step"),
        "caveats": note.get("caveats"),
        "confidence": note.get("confidence"),
    }
    return "<!-- asset_note: " + json.dumps(payload, ensure_ascii=False, sort_keys=True) + " -->"


def _safe_name(value: str) -> str:
    allowed = []
    for ch in value:
        if ch.isalnum() or ch in {"-", "_"}:
            allowed.append(ch)
        else:
            allowed.append("_")
    return "".join(allowed).strip("_") or "doc"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-manifest", type=Path, required=True)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--links", type=Path, required=True)
    parser.add_argument("--asset-notes", type=Path, required=True)
    parser.add_argument("--out-dir", type=Path, required=True)
    args = parser.parse_args(argv)

    normalize_documents(
        read_json(args.source_manifest),
        read_json(args.assets),
        read_json(args.links),
        read_jsonl(args.asset_notes),
        args.out_dir,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
