#!/usr/bin/env python3
"""Validate W0 candidate chunk JSONL before runtime promotion."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

try:
    from .common import (
        ALLOWED_CONFIDENCE,
        ALLOWED_EVIDENCE_KIND,
        ALLOWED_PRODUCT_AREAS,
        ALLOWED_SOURCE_TYPES,
        contains_internal_pattern,
        surface_url_rejection_reason,
    )
except ImportError:  # pragma: no cover
    from common import (
        ALLOWED_CONFIDENCE,
        ALLOWED_EVIDENCE_KIND,
        ALLOWED_PRODUCT_AREAS,
        ALLOWED_SOURCE_TYPES,
        contains_internal_pattern,
        surface_url_rejection_reason,
    )


REQUIRED_FIELDS = {
    "chunk_id",
    "kb_version",
    "source_type",
    "product_area",
    "acl",
    "title",
    "content",
    "source_refs",
    "asset_refs",
    "confidence",
    "valid_from",
    "evidence_kind",
    "surface_url",
    "retrieval_score_hint",
}

INTERNAL_CASE_SOURCE_IDS = {"internal_case"}
INTERNAL_CASE_SOURCE_PREFIXES = (
    "wxwork-spt-record-",
    "internal_case:",
    "internal_case/",
)
ASSET_CAPTION_MARKER = "[\u56fe\u8bf4]"


def validate_chunks(path: Path | str) -> dict[str, Any]:
    chunks = _load_jsonl(path)
    if not chunks:
        raise ValueError("chunks file is empty")
    seen: set[str] = set()
    kb_version = None
    for row, chunk in chunks:
        _validate_chunk(row, chunk)
        chunk_id = chunk["chunk_id"]
        if chunk_id in seen:
            raise ValueError(f"row {row}: duplicate chunk_id {chunk_id}")
        seen.add(chunk_id)
        if kb_version is None:
            kb_version = chunk["kb_version"]
        elif kb_version != chunk["kb_version"]:
            raise ValueError(f"row {row}: mixed kb_version")
    return {"chunk_count": len(chunks), "kb_version": kb_version}


def _load_jsonl(path: Path | str) -> list[tuple[int, dict[str, Any]]]:
    rows: list[tuple[int, dict[str, Any]]] = []
    with Path(path).open("r", encoding="utf-8") as fh:
        for row, line in enumerate(fh, start=1):
            line = line.strip()
            if not line:
                continue
            try:
                value = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(f"row {row}: invalid JSONL: {exc}") from exc
            if not isinstance(value, dict):
                raise ValueError(f"row {row}: chunk must be an object")
            rows.append((row, value))
    return rows


def _validate_chunk(row: int, chunk: dict[str, Any]) -> None:
    missing = sorted(field for field in REQUIRED_FIELDS if field not in chunk)
    if missing:
        raise ValueError(f"row {row}: missing required fields: {', '.join(missing)}")
    for field in ("chunk_id", "kb_version", "source_type", "product_area", "acl", "title", "content", "confidence", "valid_from", "evidence_kind"):
        if not isinstance(chunk.get(field), str) or not chunk[field].strip():
            raise ValueError(f"row {row}: {field} must be a non-empty string")
    if chunk["source_type"] not in ALLOWED_SOURCE_TYPES:
        raise ValueError(f"row {row}: source_type must be faq or runbook")
    if chunk["product_area"] not in ALLOWED_PRODUCT_AREAS:
        raise ValueError(f"row {row}: invalid product_area {chunk['product_area']!r}")
    if chunk["acl"] != "customer_safe":
        raise ValueError(f"row {row}: acl must be customer_safe")
    if chunk["confidence"] not in ALLOWED_CONFIDENCE:
        raise ValueError(f"row {row}: confidence must be high, medium, or low")
    if chunk["evidence_kind"] not in ALLOWED_EVIDENCE_KIND:
        raise ValueError(f"row {row}: evidence_kind must be knowledge")
    if not isinstance(chunk["source_refs"], list):
        raise ValueError(f"row {row}: source_refs must be a list")
    if not isinstance(chunk["asset_refs"], list):
        raise ValueError(f"row {row}: asset_refs must be a list")
    _validate_asset_ref_caption_alignment(row, chunk)
    if "question_patterns" in chunk and not isinstance(chunk["question_patterns"], list):
        raise ValueError(f"row {row}: question_patterns must be a list")
    if chunk["retrieval_score_hint"] is not None and not isinstance(chunk["retrieval_score_hint"], (int, float)):
        raise ValueError(f"row {row}: retrieval_score_hint must be null or number")
    reason = surface_url_rejection_reason(chunk["surface_url"])
    if reason:
        raise ValueError(f"row {row}: invalid surface_url: {reason}")
    user_text_parts = [chunk["title"], chunk["content"]]
    user_text_parts.extend(str(item) for item in chunk.get("question_patterns") or [])
    if contains_internal_pattern("\n".join(user_text_parts)):
        raise ValueError(f"row {row}: internal pattern leak")
    if _has_internal_case_source_ref(chunk["source_refs"]) and not chunk.get("approval_record_hash"):
        raise ValueError(f"row {row}: approved case rewrite missing approval record")


def _has_internal_case_source_ref(source_refs: list[Any]) -> bool:
    for item in source_refs:
        if not isinstance(item, str):
            continue
        ref = item.lower()
        if ref in INTERNAL_CASE_SOURCE_IDS:
            return True
        if any(ref.startswith(prefix) for prefix in INTERNAL_CASE_SOURCE_PREFIXES):
            return True
    return False


def _validate_asset_ref_caption_alignment(row: int, chunk: dict[str, Any]) -> None:
    ref_count = len(chunk.get("asset_refs") or [])
    caption_count = str(chunk.get("content") or "").count(ASSET_CAPTION_MARKER)
    if ref_count != caption_count:
        chunk_id = chunk.get("chunk_id") or f"row {row}"
        raise ValueError(f"row {row}: chunk {chunk_id} asset_refs count {ref_count} does not match caption count {caption_count}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chunks", type=Path, required=True)
    args = parser.parse_args(argv)
    validate_chunks(args.chunks)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
