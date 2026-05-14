#!/usr/bin/env python3
"""Offline semantic product-area labels for W0 multi-topic sections."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
from typing import Any, Callable

try:
    from . import chunk_docs
    from .common import ALLOWED_PRODUCT_AREAS
    from .model_smoke import DEFAULT_BASE_URL, DEFAULT_DS_MODEL, ModelVerseClient, _extract_json, _load_env
except ImportError:  # pragma: no cover
    import chunk_docs
    from common import ALLOWED_PRODUCT_AREAS
    from model_smoke import DEFAULT_BASE_URL, DEFAULT_DS_MODEL, ModelVerseClient, _extract_json, _load_env


PROMPT_VERSION = "label_v1"
AUTO_ACCEPT_CONFIDENCE = 0.85
REVIEW_MIN_CONFIDENCE = 0.70
DEFAULT_MULTI_TOPIC_SOURCES = Path(__file__).with_name("multi_topic_sources.json")
CONTENT_EMPTY_LABEL_REASONS = frozenset(
    {
        "out_of_scope",
        "mixed_topic",
        "too_short",
        "unclear",
        "unsafe",
    }
)
EMPTY_LABEL_REASONS = CONTENT_EMPTY_LABEL_REASONS | frozenset(
    {
        "classifier_returned_empty",
        "classifier_error",
    }
)
Classifier = Callable[[dict[str, Any]], dict[str, Any]]


def label_sections(
    cleaned_docs_dir: Path | str,
    multi_topic_sources_config: Path | str,
    out_path: Path | str,
    *,
    classifier: Classifier | None = None,
    env_path: Path = Path(".env.local"),
    model: str | None = None,
    prompt_version: str = PROMPT_VERSION,
    smoke_run_id: str | None = None,
) -> dict[str, int]:
    if classifier is None:
        classifier = _modelverse_classifier(env_path=env_path, model=model)

    multi_topic_ids = _load_multi_topic_source_ids(multi_topic_sources_config)
    rows: list[dict[str, Any]] = []
    for target in iter_label_targets(cleaned_docs_dir, multi_topic_ids=multi_topic_ids):
        rows.append(
            _label_one_section(
                target,
                classifier=classifier,
                model=model,
                prompt_version=prompt_version,
                smoke_run_id=smoke_run_id,
            )
        )

    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    _write_jsonl(out, rows)
    _write_review_queues(out.parent, rows)
    return {
        "labeled": len(rows),
        "needs_review": sum(1 for row in rows if row.get("needs_review") is True),
        "needs_split": sum(1 for row in rows if row.get("needs_split") is True),
    }


def iter_label_targets(cleaned_docs_dir: Path | str, *, multi_topic_ids: set[str]) -> list[dict[str, Any]]:
    targets: list[dict[str, Any]] = []
    for doc_path in sorted(Path(cleaned_docs_dir).glob("*.md")):
        source_ref = doc_path.stem
        source_doc_id = chunk_docs._source_doc_id(source_ref)
        if source_doc_id not in multi_topic_ids:
            continue

        text = doc_path.read_text(encoding="utf-8", errors="replace")
        body = chunk_docs._body_without_front_matter(text)
        sections = chunk_docs._split_sections(body, fallback_title=chunk_docs._fallback_title(body, source_ref))
        for section_index, section in enumerate(sections):
            content = chunk_docs._clean_chunk_content(section["content"], strict_asset_notes=False)
            if len(content.strip()) < 20:
                continue
            content_hash = chunk_docs._content_hash(content)
            targets.append(
                {
                    "key": {
                        "source_doc_id": source_doc_id,
                        "section_index": section_index,
                        "content_sha256_prefix": content_hash,
                    },
                    "source_ref": source_ref,
                    "section_title": section["title"],
                    "content": content,
                }
            )
    return targets


def _label_one_section(
    target: dict[str, Any],
    *,
    classifier: Classifier,
    model: str | None,
    prompt_version: str,
    smoke_run_id: str | None,
) -> dict[str, Any]:
    try:
        classified = classifier(target)
    except Exception as exc:
        return _label_row(
            target,
            classified={
                "selected_area": "",
                "confidence": 0,
                "empty_label_reason": "classifier_error",
                "reasoning": f"classifier_error: {type(exc).__name__}: {exc}",
                "needs_review": True,
            },
            model=model,
            prompt_version=prompt_version,
            smoke_run_id=smoke_run_id,
            attempts=1,
        )

    if _needs_empty_label_retry(classified):
        try:
            classified = classifier(target)
            attempts = 2
        except Exception as exc:
            return _label_row(
                target,
                classified={
                    "selected_area": "",
                    "confidence": 0,
                    "empty_label_reason": "classifier_error",
                    "reasoning": f"classifier_error: {type(exc).__name__}: {exc}",
                    "needs_review": True,
                },
                model=model,
                prompt_version=prompt_version,
                smoke_run_id=smoke_run_id,
                attempts=2,
            )
        if _needs_empty_label_retry(classified):
            classified = {
                **classified,
                "selected_area": "",
                "empty_label_reason": "classifier_returned_empty",
                "reasoning": str(classified)[:200],
                "needs_review": True,
            }
    else:
        attempts = 1

    return _label_row(
        target,
        classified=classified,
        model=model,
        prompt_version=prompt_version,
        smoke_run_id=smoke_run_id,
        attempts=attempts,
    )


def _needs_empty_label_retry(classified: dict[str, Any]) -> bool:
    area = str(classified.get("selected_area") or classified.get("product_area") or "").strip()
    if area in ALLOWED_PRODUCT_AREAS:
        return False
    reason = str(classified.get("empty_label_reason") or "").strip()
    return reason not in CONTENT_EMPTY_LABEL_REASONS


def _label_row(
    target: dict[str, Any],
    *,
    classified: dict[str, Any],
    model: str | None,
    prompt_version: str,
    smoke_run_id: str | None,
    attempts: int,
) -> dict[str, Any]:

    area = str(classified.get("selected_area") or classified.get("product_area") or "").strip()
    confidence = _confidence(classified.get("confidence"))
    needs_split = classified.get("needs_split") is True
    selected_area = area if area in ALLOWED_PRODUCT_AREAS else ""
    empty_label_reason = "" if selected_area else _empty_label_reason(classified.get("empty_label_reason"))
    needs_review = (
        bool(classified.get("needs_review"))
        or needs_split
        or selected_area == ""
        or confidence < AUTO_ACCEPT_CONFIDENCE
    )

    return {
        "key": target["key"],
        "source_ref": target["source_ref"],
        "section_title": target["section_title"],
        "selected_area": selected_area,
        "empty_label_reason": empty_label_reason,
        "confidence": confidence,
        "reasoning": str(classified.get("reasoning") or "")[:500],
        "alternates": classified.get("alternates") if isinstance(classified.get("alternates"), list) else [],
        "needs_split": needs_split,
        "needs_review": needs_review,
        "status": "needs_review" if needs_review else "accepted",
        "model": str(classified.get("model") or model or DEFAULT_DS_MODEL),
        "prompt_version": prompt_version,
        "labeled_at": datetime.now(timezone.utc).isoformat(),
        "smoke_run_id": smoke_run_id or "",
        "attempts": attempts,
        "content_preview": target["content"][:500],
    }


def _empty_label_reason(value: Any) -> str:
    reason = str(value or "").strip()
    return reason if reason in EMPTY_LABEL_REASONS else "classifier_returned_empty"


def _load_multi_topic_source_ids(path: Path | str) -> set[str]:
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        data = json.load(fh)
    sources = data.get("sources") if isinstance(data, dict) else None
    if not isinstance(sources, list):
        raise ValueError(f"{path}: sources must be a list")
    out: set[str] = set()
    for index, source in enumerate(sources, start=1):
        if not isinstance(source, dict) or not str(source.get("source_id") or "").strip():
            raise ValueError(f"{path}: sources[{index}] must include source_id")
        out.add(str(source["source_id"]).strip())
    return out


def _modelverse_classifier(*, env_path: Path, model: str | None) -> Classifier:
    env = _load_env(env_path)
    client = ModelVerseClient(
        base_url=env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL),
        api_key=env["MODELVERSE_API_KEY"],
    )
    selected_model = model or env.get("MODELVERSE_DS_V4_PRO_MODEL", DEFAULT_DS_MODEL)

    def classify(target: dict[str, Any]) -> dict[str, Any]:
        content = client.chat(
            model=selected_model,
            messages=[{"role": "user", "content": _classification_prompt(target)}],
            max_tokens=800,
            json_mode=True,
        )
        parsed = _extract_json(content)
        parsed["model"] = selected_model
        return parsed

    return classify


def _classification_prompt(target: dict[str, Any]) -> str:
    areas = ", ".join(sorted(ALLOWED_PRODUCT_AREAS))
    empty_reasons = ", ".join(sorted(CONTENT_EMPTY_LABEL_REASONS))
    return (
        "You are labeling one cleaned CompShare RAG section. Return only a JSON object.\n"
        f"selected_area must be one of: {areas}.\n"
        "Use the user's main issue, not incidental words, to choose the area.\n"
        "Area guide:\n"
        "- login: SSH, Jupyter, VNC, FinalShell, VS Code, remote login or connection.\n"
        "- billing_rule: billing, arrears, invoice, refund, shutdown charging rules.\n"
        "- monitor: CPU, memory, GPU memory or monitoring metrics.\n"
        "- image: platform/community/private images and image concepts.\n"
        "- driver_cuda: NVIDIA driver, CUDA installation or version issues.\n"
        "- windows: Windows RDP, remote desktop, Windows audio.\n"
        "- modelverse: model package, model quota, Coding Plan, ModelVerse.\n"
        "- init_failure: instance stuck initializing, startup failure, GPU unavailable during startup, image load failure.\n"
        "- resource_purchase: GPU spec, inventory, purchase flow or resource package.\n"
        "If the section is clearly mixed and should be split before labeling, set needs_split=true.\n"
        "If the section cannot be assigned to exactly one product area, leave selected_area empty and set empty_label_reason "
        f"to one of: {empty_reasons}.\n"
        "Never return an empty selected_area with an empty empty_label_reason.\n"
        "Always include reasoning as a short free-text explanation. Return confidence from 0 to 1 and up to two alternates.\n\n"
        f"source_ref: {target['source_ref']}\n"
        f"section_title: {target['section_title']}\n"
        f"content:\n{target['content'][:3000]}\n"
    )


def _confidence(value: Any) -> float:
    try:
        confidence = float(value)
    except (TypeError, ValueError):
        return 0.0
    return max(0.0, min(confidence, 1.0))


def _write_jsonl(path: Path, rows: list[dict[str, Any]]) -> None:
    with path.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")


def _write_review_queues(out_dir: Path, rows: list[dict[str, Any]]) -> None:
    _write_jsonl(out_dir / "needs_review.jsonl", [row for row in rows if row.get("needs_review") is True])
    _write_jsonl(out_dir / "needs_split.jsonl", [_needs_split_row(row) for row in rows if row.get("needs_split") is True])


def _needs_split_row(row: dict[str, Any]) -> dict[str, Any]:
    key = row.get("key") if isinstance(row.get("key"), dict) else {}
    source_doc_id = str(key.get("source_doc_id") or "")
    section_index = key.get("section_index")
    return {
        "section_id": f"{source_doc_id}::{section_index}",
        "source_doc_id": source_doc_id,
        "section_title": str(row.get("section_title") or ""),
        "current_label_attempt": str(row.get("selected_area") or ""),
        "current_confidence": _confidence(row.get("confidence")),
        "preview_text": str(row.get("content_preview") or row.get("reasoning") or row.get("section_title") or "")[:500],
        "flagged_at": str(row.get("labeled_at") or ""),
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cleaned-dir", type=Path, required=True)
    parser.add_argument("--multi-topic-sources", type=Path, default=DEFAULT_MULTI_TOPIC_SOURCES)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--env", type=Path, default=Path(".env.local"))
    parser.add_argument("--model", default=None)
    parser.add_argument("--smoke-run-id", default=None)
    args = parser.parse_args(argv)
    label_sections(
        args.cleaned_dir,
        args.multi_topic_sources,
        args.out,
        env_path=args.env,
        model=args.model,
        smoke_run_id=args.smoke_run_id,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
