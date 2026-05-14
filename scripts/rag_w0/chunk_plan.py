#!/usr/bin/env python3
"""Produce LLM-assisted W0 chunk plans from parsed cleaned-doc sections."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import re
from typing import Any, Callable

try:
    from .common import ALLOWED_PRODUCT_AREAS
    from .model_smoke import DEFAULT_BASE_URL, DEFAULT_DS_MODEL, ModelVerseClient, _extract_json, _load_env
    from .parse_sections import load_sections
except ImportError:  # pragma: no cover
    from common import ALLOWED_PRODUCT_AREAS
    from model_smoke import DEFAULT_BASE_URL, DEFAULT_DS_MODEL, ModelVerseClient, _extract_json, _load_env
    from parse_sections import load_sections


PROMPT_VERSION = "chunk_plan_v1"
STRATEGIES = {
    "single_topic_procedure",
    "single_topic_reference",
    "staged_long_procedure",
    "multi_topic_faq",
    "errorcode_table",
    "mixed_needs_review",
}
CHUNK_PLAN_ERRORS = {
    "classifier_returned_empty",
    "classifier_uncertain",
    "invalid_section_range",
    "invalid_strategy_enum",
    "classifier_error",
}
PRODUCT_AREA_ALIASES = {
    "image_types_gpu_specs": "image",
    "remote_login_ssh_jupyter": "login",
    "cuda_nvidia_driver": "driver_cuda",
    "invoice_refund_arrears": "billing_rule",
}
Classifier = Callable[[dict[str, Any]], dict[str, Any]]


def plan_chunks(
    cleaned_dir: Path | str,
    sections_path: Path | str,
    out_path: Path | str,
    *,
    classifier: Classifier | None = None,
    env_path: Path = Path(".env.local"),
    model: str | None = None,
    prompt_version: str = PROMPT_VERSION,
    bootstrap_plans_path: Path | str | None = None,
) -> dict[str, int]:
    sections_by_source = load_sections(sections_path)
    bootstrap_examples = _read_bootstrap_plans(bootstrap_plans_path)
    bootstrap_by_source = _bootstrap_index(bootstrap_examples)
    if classifier is None:
        classifier = _modelverse_classifier(env_path=env_path, model=model, bootstrap_examples=bootstrap_examples)

    rows: list[dict[str, Any]] = []
    for doc_path in sorted(Path(cleaned_dir).glob("*.md")):
        source_ref = doc_path.stem
        sections = sections_by_source.get(source_ref) or []
        payload = {
            "source_ref": source_ref,
            "source_doc_id": _source_doc_id(source_ref),
            "sections": _public_sections(sections),
            "doc_body": _body_without_front_matter(doc_path.read_text(encoding="utf-8", errors="replace"))[:12000],
        }
        bootstrap_plan = bootstrap_by_source.get(source_ref) or bootstrap_by_source.get(payload["source_doc_id"])
        if bootstrap_plan:
            row = _normalize_plan(payload, bootstrap_plan)
            row.update(
                {
                    "rationale": str(bootstrap_plan.get("rationale") or row.get("rationale") or ""),
                    "attempts": 0,
                    "model": str(bootstrap_plan.get("bootstrap_model") or "bootstrap"),
                    "prompt_version": str(bootstrap_plan.get("prompt_version") or "bootstrap"),
                    "planned_at": datetime.now(timezone.utc).isoformat(),
                    "bootstrap_source": True,
                }
            )
            rows.append(row)
            continue
        rows.append(_plan_one_doc(payload, classifier=classifier, model=model, prompt_version=prompt_version))

    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    _write_jsonl(out, rows)
    return {
        "doc_count": len(rows),
        "chunk_count": sum(len(row.get("chunks") or []) for row in rows if row.get("strategy") != "mixed_needs_review"),
        "mixed_needs_review": sum(1 for row in rows if row.get("strategy") == "mixed_needs_review"),
    }


def _plan_one_doc(
    payload: dict[str, Any],
    *,
    classifier: Classifier,
    model: str | None,
    prompt_version: str,
) -> dict[str, Any]:
    attempts = 1
    try:
        raw_plan = classifier(payload)
    except Exception as exc:
        return _mixed_row(payload, "classifier_error", f"{type(exc).__name__}: {exc}", attempts=1, model=model, prompt_version=prompt_version)
    if not raw_plan:
        attempts = 2
        try:
            raw_plan = classifier(payload)
        except Exception as exc:
            return _mixed_row(payload, "classifier_error", f"{type(exc).__name__}: {exc}", attempts=2, model=model, prompt_version=prompt_version)
        if not raw_plan:
            return _mixed_row(payload, "classifier_returned_empty", "classifier returned empty plan twice", attempts=2, model=model, prompt_version=prompt_version)

    normalized = _normalize_plan(payload, raw_plan)
    if normalized.get("chunk_plan_error"):
        normalized["attempts"] = attempts
        normalized["model"] = str(raw_plan.get("model") or model or DEFAULT_DS_MODEL)
        normalized["prompt_version"] = prompt_version
        normalized["planned_at"] = datetime.now(timezone.utc).isoformat()
        return normalized
    normalized.update(
        {
            "attempts": attempts,
            "model": str(raw_plan.get("model") or model or DEFAULT_DS_MODEL),
            "prompt_version": prompt_version,
            "planned_at": datetime.now(timezone.utc).isoformat(),
        }
    )
    return normalized


def _normalize_plan(payload: dict[str, Any], raw_plan: dict[str, Any]) -> dict[str, Any]:
    strategy = str(raw_plan.get("strategy") or "").strip()
    if strategy not in STRATEGIES:
        return _mixed_row(payload, "invalid_strategy_enum", f"invalid strategy {strategy!r}")
    if strategy == "mixed_needs_review":
        return _mixed_row(payload, "classifier_uncertain", str(raw_plan.get("rationale") or "mixed_needs_review"))

    sections = payload.get("sections") or []
    max_index = len(sections) - 1
    chunks: list[dict[str, Any]] = []
    for index, item in enumerate(raw_plan.get("chunks") or []):
        if not isinstance(item, dict):
            return _mixed_row(payload, "classifier_returned_empty", f"chunk {index} is not an object")
        chunk = dict(item)
        chunk["chunk_index"] = int(chunk.get("chunk_index", index))
        if chunk.get("section_index_range") is not None:
            section_range = chunk["section_index_range"]
            if not _valid_section_range(section_range, max_index=max_index):
                return _mixed_row(payload, "invalid_section_range", f"invalid section_index_range {section_range!r}")
            chunk["section_index_range"] = [int(section_range[0]), int(section_range[1])]
        elif not (chunk.get("section_anchor_text_start") and chunk.get("section_anchor_text_end")):
            derived_range = _section_range_from_included_headings(chunk, sections)
            if derived_range is None:
                return _mixed_row(payload, "invalid_section_range", "chunk lacks section_index_range or anchor text boundaries")
            chunk["section_index_range"] = derived_range
        chunk["title"] = str(chunk.get("title") or f"{payload['source_doc_id']} chunk {index + 1}").strip()
        chunk["product_area"] = normalize_product_area(chunk.get("product_area"))
        if chunk["product_area"] not in ALLOWED_PRODUCT_AREAS:
            return _mixed_row(payload, "classifier_returned_empty", f"invalid product_area {chunk.get('product_area')!r}")
        patterns = chunk.get("question_patterns")
        chunk["question_patterns"] = [str(item).strip() for item in patterns if str(item).strip()] if isinstance(patterns, list) else []
        if not chunk["question_patterns"]:
            chunk["question_patterns"] = [chunk["title"]]
        chunks.append(chunk)
    if not chunks:
        return _mixed_row(payload, "classifier_returned_empty", "no chunks")
    chunks = _expand_section_range_gaps(chunks, max_index=max_index)
    return {
        "source_ref": payload["source_ref"],
        "source_doc_id": str(raw_plan.get("source_doc_id") or payload["source_doc_id"]),
        "strategy": strategy,
        "rationale": str(raw_plan.get("rationale") or ""),
        "chunks": chunks,
        "chunk_plan_error": "",
    }


def _valid_section_range(value: Any, *, max_index: int) -> bool:
    if not isinstance(value, list) or len(value) != 2:
        return False
    try:
        start = int(value[0])
        end = int(value[1])
    except (TypeError, ValueError):
        return False
    return 0 <= start <= end <= max_index


def _section_range_from_included_headings(chunk: dict[str, Any], sections: list[dict[str, Any]]) -> list[int] | None:
    if len(sections) == 1:
        return [0, 0]
    raw_headings = chunk.get("section_headings_included") or []
    if not isinstance(raw_headings, list):
        return None
    wanted = [_heading_key(value) for value in raw_headings if _heading_key(value)]
    if not wanted:
        return None

    matches: list[int] = []
    cursor = 0
    for target in wanted:
        for index in range(cursor, len(sections)):
            section = sections[index]
            if _section_matches_heading(section, target):
                matches.append(index)
                cursor = index + 1
                break
    if not matches:
        return None
    return [min(matches), max(matches)]


def _expand_section_range_gaps(chunks: list[dict[str, Any]], *, max_index: int) -> list[dict[str, Any]]:
    if max_index < 0 or not chunks:
        return chunks
    ranged = [chunk for chunk in chunks if chunk.get("section_index_range") is not None]
    if len(ranged) != len(chunks):
        return chunks
    ordered = sorted(ranged, key=lambda chunk: (int(chunk["section_index_range"][0]), int(chunk["chunk_index"])))
    for current, following in zip(ordered, ordered[1:]):
        current_range = current["section_index_range"]
        following_range = following["section_index_range"]
        current_end = int(current_range[1])
        following_start = int(following_range[0])
        if current_end + 1 < following_start:
            current["section_index_range"] = [int(current_range[0]), following_start - 1]
    last = ordered[-1]
    last_range = last["section_index_range"]
    if int(last_range[1]) < max_index:
        last["section_index_range"] = [int(last_range[0]), max_index]
    return chunks


def _section_matches_heading(section: dict[str, Any], target: str) -> bool:
    candidates = []
    heading_text = section.get("heading_text")
    if heading_text:
        candidates.append(str(heading_text))
    heading_path = section.get("heading_path") or []
    if isinstance(heading_path, list):
        candidates.extend(str(item) for item in heading_path)
        candidates.append(" > ".join(str(item) for item in heading_path))
    keys = [_heading_key(candidate) for candidate in candidates if _heading_key(candidate)]
    if target in keys:
        return True
    # Opus bootstrap headings occasionally shorten long Markdown headings.
    # Allow a conservative substring match for longer labels only.
    return any(len(target) >= 6 and (target in key or key in target) for key in keys)


def _heading_key(value: Any) -> str:
    text = str(value or "").strip().lstrip("#").strip().lower()
    return re.sub(r"\s+", "", text)


def normalize_product_area(value: Any) -> str:
    area = str(value or "").strip()
    return PRODUCT_AREA_ALIASES.get(area, area)


def _mixed_row(
    payload: dict[str, Any],
    error: str,
    reason: str,
    *,
    attempts: int = 1,
    model: str | None = None,
    prompt_version: str = PROMPT_VERSION,
) -> dict[str, Any]:
    if error not in CHUNK_PLAN_ERRORS:
        error = "classifier_error"
    return {
        "source_ref": payload["source_ref"],
        "source_doc_id": payload["source_doc_id"],
        "strategy": "mixed_needs_review",
        "rationale": reason[:500],
        "chunks": [],
        "chunk_plan_error": error,
        "attempts": attempts,
        "model": str(model or DEFAULT_DS_MODEL),
        "prompt_version": prompt_version,
        "planned_at": datetime.now(timezone.utc).isoformat(),
    }


def _modelverse_classifier(*, env_path: Path, model: str | None, bootstrap_examples: list[dict[str, Any]]) -> Classifier:
    env = _load_env(env_path)
    client = ModelVerseClient(base_url=env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL), api_key=env["MODELVERSE_API_KEY"])
    selected_model = model or env.get("MODELVERSE_DS_V4_PRO_MODEL", DEFAULT_DS_MODEL)

    def classify(payload: dict[str, Any]) -> dict[str, Any]:
        content = client.chat(
            model=selected_model,
            messages=[{"role": "user", "content": _planning_prompt(payload, bootstrap_examples)}],
            max_tokens=3000,
            json_mode=True,
        )
        parsed = _extract_json(content)
        parsed["model"] = selected_model
        return parsed

    return classify


def _planning_prompt(payload: dict[str, Any], bootstrap_examples: list[dict[str, Any]]) -> str:
    examples = []
    for item in bootstrap_examples[:5]:
        examples.append(
            {
                "source_doc_id": item.get("source_doc_id"),
                "strategy": item.get("strategy"),
                "chunks": [
                    {
                        "title": chunk.get("title"),
                        "product_area": chunk.get("product_area"),
                        "question_patterns": chunk.get("question_patterns", [])[:3],
                        "section_index_range": chunk.get("section_index_range"),
                        "section_anchor_text_start": chunk.get("section_anchor_text_start"),
                        "section_anchor_text_end": chunk.get("section_anchor_text_end"),
                    }
                    for chunk in item.get("chunks", [])[:3]
                ],
            }
        )
    return (
        "You decide how to split one cleaned CompShare knowledge document. Return ONLY JSON.\n"
        "Never rewrite chunk content. Only decide strategy and boundaries.\n"
        f"Allowed strategies: {', '.join(sorted(STRATEGIES))}.\n"
        f"Allowed product_area values: {', '.join(sorted(ALLOWED_PRODUCT_AREAS | set(PRODUCT_AREA_ALIASES)))}.\n"
        "If the document has no usable markdown sections, use section_anchor_text_start/end boundaries from visible text.\n"
        "Each chunk needs title, product_area, question_patterns, and either section_index_range [start,end] or anchor text boundaries.\n"
        "Use mixed_needs_review only if you cannot produce safe boundaries.\n\n"
        f"Examples:\n{json.dumps(examples, ensure_ascii=False)}\n\n"
        f"source_doc_id: {payload['source_doc_id']}\n"
        f"sections:\n{json.dumps(payload['sections'], ensure_ascii=False)}\n"
        f"document:\n{payload['doc_body']}\n"
    )


def _read_bootstrap_plans(path: Path | str | None) -> list[dict[str, Any]]:
    if not path:
        return []
    return _read_jsonl(path)


def _bootstrap_index(rows: list[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    out: dict[str, dict[str, Any]] = {}
    for row in rows:
        source_doc_id = str(row.get("source_doc_id") or "")
        source_ref = str(row.get("source_ref") or "")
        if source_doc_id:
            out[source_doc_id] = row
        if source_ref:
            out[source_ref] = row
    return out


def _public_sections(sections: list[dict[str, Any]]) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for section in sections:
        out.append(
            {
                "section_index": section.get("section_index"),
                "heading_text": section.get("heading_text"),
                "heading_path": section.get("heading_path"),
                "content_preview": str(section.get("content") or "")[:900],
                "risk_flags": section.get("risk_flags") or [],
            }
        )
    return out


def _source_doc_id(source_ref: str) -> str:
    return str(source_ref).split("__", 1)[0]


def _body_without_front_matter(text: str) -> str:
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return "\n".join(lines).strip()
    for idx, line in enumerate(lines[1:], start=1):
        if line.strip() == "---":
            return "\n".join(lines[idx + 1 :]).strip()
    return "\n".join(lines).strip()


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def _write_jsonl(path: Path, rows: list[dict[str, Any]]) -> None:
    with path.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cleaned-dir", type=Path, required=True)
    parser.add_argument("--sections", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--env", type=Path, default=Path(".env.local"))
    parser.add_argument("--model", default=None)
    parser.add_argument("--bootstrap-plans", type=Path)
    args = parser.parse_args(argv)
    plan_chunks(
        args.cleaned_dir,
        args.sections,
        args.out,
        env_path=args.env,
        model=args.model,
        bootstrap_plans_path=args.bootstrap_plans,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
