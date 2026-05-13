#!/usr/bin/env python3
"""Create deterministic W0 image notes from asset_manifest.json."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

try:
    from .common import read_json
except ImportError:  # pragma: no cover
    from common import read_json


VISUAL_TYPES = {
    "operation_screenshot",
    "error_screenshot",
    "console_state",
    "diagram",
    "logo",
    "qr_code",
    "decorative",
    "unknown",
}


def describe_asset_notes(asset_manifest: dict[str, Any]) -> list[dict[str, Any]]:
    notes: list[dict[str, Any]] = []
    for asset in asset_manifest.get("assets") or []:
        state = asset.get("final_state") or "unknown"
        include = bool(asset.get("include_in_rag")) and state in {"included_with_ocr_note", "included_with_vl_note"}
        nearby_text = _clean_text(asset.get("nearby_text") or "")
        heading_path = list(asset.get("heading_path") or [])
        heading = " / ".join(str(item) for item in heading_path)
        visual_type = _visual_type(asset, nearby_text, heading)
        base = {
            "asset_id": asset.get("asset_id"),
            "source_id": asset.get("source_id"),
            "source_doc_id": asset.get("source_doc_id") or asset.get("source_id") or "",
            "heading_path": heading_path,
            "image_ref": asset.get("image_ref"),
            "image_path": asset.get("image_path"),
            "nearby_text": nearby_text,
            "visual_type": visual_type,
            "exclusion_reason": asset.get("exclusion_reason") or "",
            "final_state": state,
        }
        if include:
            note = {
                **base,
                "note_type": "operation_note",
                "include_in_rag": True,
                "description": _description(asset, nearby_text, heading),
                "highlighted_ui": _highlighted_ui(nearby_text, heading),
                "user_action": _user_action(nearby_text),
                "expected_input": _expected_input(nearby_text),
                "next_step": _next_step(nearby_text),
                "caveats": "Generated from nearby text only; VL/OCR is deferred and this note requires review before chunking.",
                "confidence": "low",
                "requires_review": True,
                "model_metadata": {
                    "method": "deterministic_nearby_text",
                    "model": None,
                    "vl_executed": False,
                    "vl_status": "deferred_to_followup",
                },
            }
        else:
            note = {
                **base,
                "note_type": "excluded",
                "include_in_rag": False,
                "description": "",
                "confidence": "high" if state.startswith("excluded_") else "medium",
                "requires_review": state in {"missing_asset", "external_asset_snapshot_required", "external_asset_snapshot_failed"},
                "model_metadata": {
                    "method": "deterministic_asset_state",
                    "model": None,
                    "vl_executed": False,
                    "vl_status": "not_applicable",
                },
            }
        notes.append(note)
    return notes


def write_jsonl(path: Path | str, rows: list[dict[str, Any]]) -> None:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")


def read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def _visual_type(asset: dict[str, Any], nearby_text: str, heading: str) -> str:
    text = f"{asset.get('image_ref', '')} {nearby_text} {heading}".lower()
    if any(word in text for word in ("qr", "qrcode", "\u4e8c\u7ef4\u7801", "\u7fa4")):
        return "qr_code"
    if any(word in text for word in ("logo", "brand")):
        return "logo"
    if any(word in text for word in ("decor", "banner", "background", "\u88c5\u9970")):
        return "decorative"
    if any(word in text for word in ("error", "fail", "failed", "\u62a5\u9519", "\u5931\u8d25")):
        return "error_screenshot"
    if any(word in text for word in ("diagram", "architecture", "flow", "\u6d41\u7a0b", "\u67b6\u6784")):
        return "diagram"
    if any(word in text for word in ("console", "\u63a7\u5236\u53f0", "login", "\u767b\u5f55", "instance", "\u5b9e\u4f8b", "windows", "rdp")):
        return "console_state"
    if any(word in text for word in ("click", "select", "input", "open", "install", "download", "\u70b9\u51fb", "\u9009\u62e9", "\u8f93\u5165", "\u5b89\u88c5")):
        return "operation_screenshot"
    return "unknown"


def _description(asset: dict[str, Any], nearby_text: str, heading: str) -> str:
    basis = nearby_text or heading or str(asset.get("image_ref") or "image")
    return f"Image note derived from nearby text: {basis}"


def _highlighted_ui(nearby_text: str, heading: str) -> str:
    text = nearby_text or heading
    for keyword in ("console", "login", "instance", "image", "driver", "CUDA", "NVIDIA", "Windows", "\u63a7\u5236\u53f0", "\u767b\u5f55"):
        if keyword.lower() in text.lower():
            return keyword
    return ""


def _user_action(nearby_text: str) -> str:
    if not nearby_text:
        return ""
    action_words = ("click", "select", "input", "open", "login", "connect", "install", "run", "download", "\u70b9\u51fb", "\u9009\u62e9", "\u8f93\u5165")
    if any(word in nearby_text.lower() for word in action_words):
        return nearby_text
    return ""


def _expected_input(nearby_text: str) -> str:
    for marker in ("input", "fill", "select", "\u8f93\u5165", "\u586b\u5199", "\u9009\u62e9"):
        if marker in nearby_text.lower():
            return nearby_text
    return ""


def _next_step(nearby_text: str) -> str:
    for marker in ("then", "next", "after", "\u7136\u540e", "\u4e0b\u4e00\u6b65", "\u5b8c\u6210\u540e"):
        if marker in nearby_text.lower():
            return nearby_text
    return ""


def _clean_text(text: str) -> str:
    return " ".join(str(text).split())


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args(argv)

    write_jsonl(args.out, describe_asset_notes(read_json(args.assets)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
