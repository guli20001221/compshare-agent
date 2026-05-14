#!/usr/bin/env python3
"""Chunk cleaned W0 Markdown and approved case rewrites."""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import re
from pathlib import Path
from typing import Any

try:
    from .common import ALLOWED_PRODUCT_AREAS
    from .validate_case_approvals import validate_case_approvals
    from .validate_chunks import validate_chunks
except ImportError:  # pragma: no cover
    from common import ALLOWED_PRODUCT_AREAS
    from validate_case_approvals import validate_case_approvals
    from validate_chunks import validate_chunks


DEFAULT_KB_VERSION = "kb.stage2b.w0.2026-05-13"
DEFAULT_VALID_FROM = "2026-05-13"
LOGGER = logging.getLogger(__name__)
# Must stay in sync with internal/knowledge/loader.go MaxKnowledgeContentRunes.
MAX_CHUNK_CONTENT_RUNES = 4000
ASSET_NOTE_MAX_CHARS = 300
AUTO_ACCEPT_SECTION_LABEL_CONFIDENCE = 0.85
REVIEW_SECTION_LABEL_CONFIDENCE = 0.70
SHA256_PREFIX_LEN = 16
ASSET_NOTE_RE = re.compile(r"<!--\s*asset_note:\s*(\{.*?\})\s*-->", re.DOTALL)
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$")
HTML_COMMENT_RE = re.compile(r"<!--.*?-->", re.DOTALL)
REDACTION_MARKER_RE = re.compile(r"\[(?:PERSON|RESOURCE_ID|LINK|SECRET|PRIVATE_PROCESS|PRIVATE_SYSTEM)_REDACTED\]")
PROCEDURE_LINE_RE = re.compile(r"^(?:\d+[.、]\s+|步骤\s*\d+|第\s*[一二三四五六七八九十0-9]+\s*步)")
ASSET_NOTE_EXCLUDED_TYPES = {"logo", "decorative", "qr_code"}


def chunk_documents(
    cleaned_dir: Path | str,
    out_path: Path | str,
    *,
    kb_version: str = DEFAULT_KB_VERSION,
    valid_from: str = DEFAULT_VALID_FROM,
    cases_path: Path | str | None = None,
    approvals_path: Path | str | None = None,
    asset_notes_path: Path | str | None = None,
    link_manifest_path: Path | str | None = None,
    chunk_labels_path: Path | str | None = None,
    section_labels_path: Path | str | None = None,
    require_complete_inputs: bool = True,
    max_chars: int = 2000,
) -> dict[str, int]:
    if require_complete_inputs:
        _validate_ready_for_chunking(asset_notes_path, link_manifest_path, Path(cleaned_dir))

    _validate_output_target(out_path, require_complete_inputs=require_complete_inputs)
    section_labels = _read_section_label_index(section_labels_path or chunk_labels_path)

    chunks: list[dict[str, Any]] = []
    for doc_path in sorted(Path(cleaned_dir).glob("*.md")):
        chunks.extend(
            _chunks_for_doc(
                doc_path,
                kb_version=kb_version,
                valid_from=valid_from,
                max_chars=max_chars,
                strict_asset_notes=require_complete_inputs,
                section_labels=section_labels,
            )
        )

    case_chunk_count = 0
    if cases_path:
        case_chunks = _chunks_for_approved_cases(cases_path, approvals_path, kb_version=kb_version, valid_from=valid_from)
        chunks.extend(case_chunks)
        case_chunk_count = len(case_chunks)

    chunks = _dedupe_chunk_ids(chunks)
    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        for chunk in chunks:
            fh.write(json.dumps(chunk, ensure_ascii=False, sort_keys=True) + "\n")
    validate_chunks(out)
    return {"chunk_count": len(chunks), "case_chunk_count": case_chunk_count}


def _chunks_for_doc(
    doc_path: Path,
    *,
    kb_version: str,
    valid_from: str,
    max_chars: int,
    strict_asset_notes: bool,
    section_labels: dict[str, dict[str, Any]] | None = None,
) -> list[dict[str, Any]]:
    text = doc_path.read_text(encoding="utf-8", errors="replace")
    meta = _front_matter_values(text)
    body = _body_without_front_matter(text)
    selected_area = _selected_product_area(meta)
    source_ref = doc_path.stem
    sections = _split_sections(body, fallback_title=_fallback_title(body, doc_path.stem))
    chunks: list[dict[str, Any]] = []
    for section_index, section in enumerate(sections, start=1):
        title = section["title"]
        content = section["content"]
        if not content.strip():
            continue
        asset_refs = _asset_refs(content, strict_asset_notes=strict_asset_notes)
        content = _clean_chunk_content(content, strict_asset_notes=strict_asset_notes)
        label_area, low_confidence_label, needs_split = _section_label_decision(
            section_labels,
            _source_doc_id(source_ref),
            section_index - 1,
            content,
        )
        if needs_split:
            LOGGER.info("section label requested split; skipping source=%s section=%s title=%s", source_ref, section_index, title)
            continue
        for part_index, part in enumerate(_split_long_text(content, max_chars=max_chars), start=1):
            if len(part.strip()) < 20:
                continue
            chunk_title = title if part_index == 1 else f"{title} ({part_index})"
            product_area = label_area or selected_area or _infer_product_area(f"{source_ref} {chunk_title} {part}")
            if label_area:
                labeled_via = "section-sidecar"
            elif selected_area:
                labeled_via = "doc-psa"
            else:
                labeled_via = "keyword"
            LOGGER.debug(
                "section_id=%s::%s via=%s product_area=%s",
                _source_doc_id(source_ref),
                section_index - 1,
                labeled_via,
                product_area,
            )
            chunk = _base_chunk(
                chunk_id=_chunk_id(product_area, source_ref, section_index, part_index),
                kb_version=kb_version,
                source_type=_infer_source_type(source_ref, product_area),
                product_area=product_area,
                title=chunk_title,
                content=part,
                source_refs=[source_ref],
                asset_refs=asset_refs,
                confidence="medium" if "REDACTED" in part else "high",
                valid_from=valid_from,
            )
            if low_confidence_label:
                chunk["low_confidence_label"] = True
            chunks.append(chunk)
    return chunks


def _selected_product_area(meta: dict[str, str]) -> str:
    area = str(meta.get("source_selection_product_area") or "").strip()
    return area if area in ALLOWED_PRODUCT_AREAS else ""


def _read_section_label_index(path: Path | str | None) -> dict[str, dict[str, Any]]:
    if not path:
        return {}
    labels: dict[str, dict[str, Any]] = {}
    for row in _read_jsonl(path):
        key = _section_label_key_from_row(row)
        if key:
            labels[key] = row
    return labels


def _section_label_decision(
    labels: dict[str, dict[str, Any]] | None,
    source_doc_id: str,
    section_index: int,
    content: str,
) -> tuple[str, bool, bool]:
    if not labels:
        return "", False, False
    key = _section_label_key(source_doc_id, section_index, _content_hash(content))
    row = labels.get(key)
    if not row:
        return "", False, False
    if row.get("needs_split") is True:
        return "", False, True
    area = str(row.get("selected_area") or row.get("product_area") or "")
    confidence = _label_confidence(row.get("confidence"))
    if area not in ALLOWED_PRODUCT_AREAS or confidence < REVIEW_SECTION_LABEL_CONFIDENCE:
        return "", False, False
    return area, confidence < AUTO_ACCEPT_SECTION_LABEL_CONFIDENCE, False


def _label_confidence(value: Any) -> float:
    try:
        return float(value)
    except (TypeError, ValueError):
        return 0.0


def _chunks_for_approved_cases(
    cases_path: Path | str,
    approvals_path: Path | str | None,
    *,
    kb_version: str,
    valid_from: str,
) -> list[dict[str, Any]]:
    cases = _read_jsonl(cases_path)
    approvals = _read_jsonl(approvals_path) if approvals_path else []
    if not approvals:
        return []
    validate_case_approvals(cases, approvals)
    cases_by_hash = {case.get("redacted_case_hash"): case for case in cases}
    chunks: list[dict[str, Any]] = []
    for approval in approvals:
        case = cases_by_hash.get(approval.get("source_hash"))
        if not case or case.get("label") != "faq_candidate":
            continue
        content = str(case.get("user_safe_answer_candidate") or case.get("resolution") or "").strip()
        if not content:
            continue
        area = str(approval.get("allowed_product_area") or _infer_product_area(content))
        chunk = _base_chunk(
            chunk_id=str(approval.get("final_runtime_chunk_id") or _chunk_id(area, str(case["case_id"]), 1, 1)),
            kb_version=kb_version,
            source_type="faq",
            product_area=area,
            title=str(case.get("issue_pattern") or "Approved case rewrite"),
            content=content,
            source_refs=[str(case["case_id"])],
            asset_refs=[],
            confidence="medium",
            valid_from=valid_from,
        )
        chunk["approval_record_hash"] = _approval_record_hash(approval)
        chunks.append(chunk)
    return chunks


def _validate_ready_for_chunking(asset_notes_path: Path | str | None, link_manifest_path: Path | str | None, cleaned_dir: Path | str) -> None:
    if not asset_notes_path:
        raise ValueError("asset notes are required before chunking")
    if not link_manifest_path:
        raise ValueError("link manifest is required before chunking")

    notes = _read_jsonl(asset_notes_path)
    for idx, note in enumerate(notes, start=1):
        if not note.get("include_in_rag"):
            continue
        asset_id = note.get("asset_id") or f"row {idx}"
        metadata = note.get("model_metadata") or {}
        if note.get("final_state") != "included_with_vl_note":
            raise ValueError(f"asset {asset_id}: not VL-ready for chunking")
        if metadata.get("vl_executed") is not True:
            raise ValueError(f"asset {asset_id}: missing VL execution")
        if note.get("requires_review") is True:
            raise ValueError(f"asset {asset_id}: still requires review")
        if not str(note.get("description") or "").strip():
            raise ValueError(f"asset {asset_id}: missing VL description")

    cleaned_source_ids = _cleaned_source_ids(cleaned_dir)
    links = _read_json(link_manifest_path)
    pending = {"unknown", "review_required"}
    for idx, link in enumerate(links.get("links") or [], start=1):
        state = link.get("final_state")
        if state in pending and _link_applies_to_cleaned_sources(link, cleaned_source_ids):
            link_id = link.get("link_id") or f"row {idx}"
            raise ValueError(f"link {link_id}: unresolved before chunking")


def _cleaned_source_ids(cleaned_dir: Path | str) -> set[str]:
    source_ids: set[str] = set()
    for doc_path in Path(cleaned_dir).glob("*.md"):
        stem = doc_path.stem
        source_id = stem.split("__", 1)[0] if "__" in stem else stem
        if source_id:
            source_ids.add(source_id)
    return source_ids


def _link_applies_to_cleaned_sources(link: dict[str, Any], cleaned_source_ids: set[str]) -> bool:
    source_id = str(link.get("source_id") or "").strip()
    if not source_id:
        return True
    if not cleaned_source_ids:
        return True
    return source_id in cleaned_source_ids


def _validate_output_target(out_path: Path | str, *, require_complete_inputs: bool) -> None:
    if require_complete_inputs:
        return
    path = Path(out_path)
    parts = tuple(part.lower() for part in path.parts)
    writes_deploy_kb = any(left == "deploy" and right == "kb" for left, right in zip(parts, parts[1:]))
    if writes_deploy_kb or path.name.lower() == "stage2b_w0.jsonl":
        raise ValueError("dry-run chunking cannot write deploy/kb or stage2b_w0.jsonl")


def _base_chunk(
    *,
    chunk_id: str,
    kb_version: str,
    source_type: str,
    product_area: str,
    title: str,
    content: str,
    source_refs: list[str],
    asset_refs: list[str],
    confidence: str,
    valid_from: str,
) -> dict[str, Any]:
    return {
        "chunk_id": chunk_id,
        "kb_version": kb_version,
        "source_type": source_type,
        "product_area": product_area,
        "acl": "customer_safe",
        "title": title.strip()[:180],
        "question_patterns": _question_patterns(title, product_area),
        "content": content.strip(),
        "source_refs": source_refs,
        "asset_refs": asset_refs,
        "confidence": confidence,
        "valid_from": valid_from,
        "evidence_kind": "knowledge",
        "surface_url": None,
        "retrieval_score_hint": None,
    }


def _front_matter_end(lines: list[str]) -> int:
    if not lines or lines[0].strip() != "---":
        return 0
    for idx, line in enumerate(lines[1:], start=1):
        if line.strip() == "---":
            return idx + 1
    return 0


def _front_matter_values(text: str) -> dict[str, str]:
    lines = text.splitlines()
    end = _front_matter_end(lines)
    if not end:
        return {}
    out: dict[str, str] = {}
    for line in lines[1 : end - 1]:
        if ":" in line:
            key, value = line.split(":", 1)
            out[key.strip()] = value.strip()
    return out


def _body_without_front_matter(text: str) -> str:
    lines = text.splitlines()
    return "\n".join(lines[_front_matter_end(lines) :]).strip()


def _split_sections(body: str, *, fallback_title: str) -> list[dict[str, str]]:
    sections: list[dict[str, str]] = []
    title = fallback_title
    buf: list[str] = []
    for line in body.splitlines():
        match = HEADING_RE.match(line)
        if match or _looks_like_plain_faq_heading(line):
            if buf:
                sections.append({"title": title, "content": "\n".join(buf).strip()})
            title = match.group(2).strip() if match else line.strip()
            buf = []
            continue
        buf.append(line)
    if buf:
        sections.append({"title": title, "content": "\n".join(buf).strip()})
    return sections


def _looks_like_plain_faq_heading(line: str) -> bool:
    value = line.strip()
    if not value or len(value) > 100:
        return False
    lowered = value.lower()
    if lowered.startswith(("http://", "https://")):
        return False
    if re.search(r"\.(?:png|jpg|jpeg|gif|webp|pdf)$", lowered):
        return False
    if value.startswith(("-", "*", ">", "|", "```", "![", "[")):
        return False
    if any(mark in value for mark in ("，", ",", "；", ";", "：", ":")):
        return False
    if value.endswith(("。", "、")):
        return False
    if value.startswith(("请", "可以", "由于", "可能", "支持", "当前", "每个", "简单", "基本", "如果", "若", "需要", "会", "断链")):
        return False
    if " -- " in value or re.match(r"^\d+(?:\.\d+)+\s+", value):
        return True
    title_signals = (
        "相关",
        "问题",
        "说明",
        "模式",
        "限制",
        "操作",
        "使用",
        "登录",
        "连接",
        "启动中",
        "启动失败",
        "初始化",
        "检测不到",
        "无法",
        "报错",
        "吗？",
        "吗?",
    )
    return any(signal in value for signal in title_signals)


def _split_long_text(text: str, *, max_chars: int) -> list[str]:
    paragraphs = [part.strip() for part in re.split(r"\n\s*\n+", text) if part.strip()]
    parts: list[str] = []
    current: list[str] = []
    current_len = 0
    for paragraph in paragraphs or [text.strip()]:
        if _is_procedural_block(paragraph):
            if len(paragraph) > MAX_CHUNK_CONTENT_RUNES:
                raise ValueError(f"procedure block {paragraph[:80]!r} exceeds MAX_CHUNK_CONTENT_RUNES; add ## subheading to split upstream")
            if len(paragraph) > max_chars:
                if current:
                    parts.append("\n\n".join(current))
                    current = []
                    current_len = 0
                LOGGER.info("procedure block exceeds max_chars but within loader cap, kept intact: %s...", paragraph[:40])
                parts.append(paragraph)
                continue
        if current and current_len + len(paragraph) + 2 > max_chars:
            parts.append("\n\n".join(current))
            current = []
            current_len = 0
        if len(paragraph) > max_chars:
            for unit in _split_oversized_paragraph(paragraph):
                if current and current_len + len(unit) + 1 > max_chars:
                    parts.append("\n\n".join(current))
                    current = []
                    current_len = 0
                current.append(unit)
                current_len += len(unit) + 1
            continue
        current.append(paragraph)
        current_len += len(paragraph) + 2
    if current:
        parts.append("\n\n".join(current))
    return parts


def _split_oversized_paragraph(paragraph: str) -> list[str]:
    lines = [line.strip() for line in paragraph.splitlines() if line.strip()]
    if len(lines) > 1:
        return lines
    if len(paragraph) > MAX_CHUNK_CONTENT_RUNES:
        raise ValueError(f"paragraph {paragraph[:80]!r} exceeds MAX_CHUNK_CONTENT_RUNES; add ## subheading to split upstream")
    if not re.search(r"[。！？!?；;]", paragraph):
        raise ValueError(f"paragraph {paragraph[:80]!r} exceeds max_chars; add ## subheading to split upstream")
    sentences = [item.strip() for item in re.split(r"(?<=[。.!?？])\s+", paragraph) if item.strip()]
    return sentences or [paragraph.strip()]


def _is_procedural_block(paragraph: str) -> bool:
    lines = [line.strip() for line in paragraph.splitlines() if line.strip()]
    streak = 0
    max_streak = 0
    matched = 0
    for line in lines:
        if PROCEDURE_LINE_RE.match(line):
            streak += 1
            max_streak = max(max_streak, streak)
            matched += 1
            continue
        streak = 0
    return max_streak >= 2 and matched / max(len(lines), 1) >= 0.4


def _asset_refs(text: str, *, strict_asset_notes: bool) -> list[str]:
    refs: list[str] = []
    for match in ASSET_NOTE_RE.finditer(text):
        payload = _parse_asset_note_payload(match, strict=strict_asset_notes)
        if payload is None or not _should_include_asset_note(payload):
            continue
        asset_id = payload.get("asset_id")
        if asset_id and asset_id not in refs:
            refs.append(str(asset_id))
    return refs


def _clean_chunk_content(text: str, *, strict_asset_notes: bool) -> str:
    text = ASSET_NOTE_RE.sub(lambda match: _render_asset_note_match(match, strict=strict_asset_notes), text)
    text = HTML_COMMENT_RE.sub("", text)
    lines = [line.rstrip() for line in text.splitlines()]
    return "\n".join(line for line in lines if line.strip()).strip()


def _render_asset_note_match(match: re.Match[str], *, strict: bool) -> str:
    payload = _parse_asset_note_payload(match, strict=strict)
    if payload is None or not _should_include_asset_note(payload):
        return ""
    return _render_asset_note_text(payload)


def _parse_asset_note_payload(match: re.Match[str], *, strict: bool) -> dict[str, Any] | None:
    try:
        value = json.loads(match.group(1))
    except json.JSONDecodeError as exc:
        message = f"invalid asset_note JSON near {match.group(0)[:120]!r}"
        if strict:
            raise ValueError(message) from exc
        LOGGER.warning(message)
        return None
    return value if isinstance(value, dict) else None


def _should_include_asset_note(payload: dict[str, Any]) -> bool:
    if payload.get("include_in_rag") is not True:
        return False
    visual_type = payload.get("visual_type")
    if not visual_type:
        LOGGER.info("asset_note %s has no visual_type; rendering by default", payload.get("asset_id") or "<unknown>")
    if visual_type in ASSET_NOTE_EXCLUDED_TYPES:
        return False
    return True


def _render_asset_note_text(payload: dict[str, Any]) -> str:
    description = _nonempty(payload.get("description"))
    if not description:
        return ""
    parts = [f"[图说] {_sentence_text(description)}"]
    highlighted_ui = _nonempty(payload.get("highlighted_ui"))
    if highlighted_ui:
        parts.append(f"重点：{_sentence_text(highlighted_ui)}")
    user_action = _nonempty(payload.get("user_action"))
    if user_action:
        parts.append(f"操作：{_sentence_text(user_action)}")
    next_step = _nonempty(payload.get("next_step"))
    if next_step:
        parts.append(f"下一步：{_sentence_text(next_step)}")
    caveats = _nonempty(payload.get("caveats"))
    if caveats:
        parts.append(f"注意：{_sentence_text(caveats)}")
    rendered = "".join(parts)
    if len(rendered) > ASSET_NOTE_MAX_CHARS:
        return rendered[:280].rstrip() + "..."
    return rendered


def _nonempty(value: Any) -> str:
    return str(value or "").strip()


def _sentence_text(value: str) -> str:
    return value if value.endswith(("。", "！", "？", ".", "!", "?", "；", ";")) else value + "。"


def _infer_source_type(source_ref: str, product_area: str) -> str:
    ref = source_ref.lower()
    if product_area in {"driver_cuda", "windows"} or any(word in ref for word in ("runbook", "windows", "nvidia", "cuda", "rdp")):
        return "runbook"
    return "faq"


def _infer_product_area(text: str) -> str:
    lowered = text.lower()
    checks = [
        ("billing_rule", ("billing", "invoice", "refund", "arrears", "shutdown", "计费", "发票", "退款", "欠费", "关机")),
        ("driver_cuda", ("cuda", "nvidia", "driver", "驱动")),
        ("windows", ("windows", "rdp", "remote desktop", "sound", "声音")),
        ("login", ("ssh", "jupyter", "login", "password reset", "remote", "登录", "远程", "连接")),
        ("modelverse", ("modelverse", "credit", "package", "模力方", "模型", "额度")),
        ("init_failure", ("init", "initialization", "初始化", "开机失败")),
        ("monitor", ("monitor", "cpu", "memory", "vram", "监控", "内存", "显存")),
        ("image", ("image", "镜像")),
        ("resource_purchase", ("purchase", "inventory", "gpu spec", "资源包", "购买", "库存", "规格")),
    ]
    for area, keywords in checks:
        if any(keyword in lowered or keyword in text for keyword in keywords):
            return area
    return "resource_purchase"


def _content_hash(content: str) -> str:
    normalized = re.sub(r"\s+", " ", content).strip()
    return hashlib.sha256(normalized.encode("utf-8")).hexdigest()[:SHA256_PREFIX_LEN]


def _source_doc_id(source_ref: str) -> str:
    return str(source_ref).split("__", 1)[0]


def _section_label_key(source_doc_id: str, section_index: Any, content_hash: str) -> str:
    try:
        section = int(section_index)
    except (TypeError, ValueError):
        return ""
    if not source_doc_id or not content_hash:
        return ""
    return f"{source_doc_id}#{section}:{content_hash}"


def _section_label_key_from_row(row: dict[str, Any]) -> str:
    key = row.get("key")
    if isinstance(key, dict):
        return _section_label_key(
            str(key.get("source_doc_id") or ""),
            key.get("section_index"),
            str(key.get("content_sha256_prefix") or ""),
        )
    return ""


def _question_patterns(title: str, product_area: str) -> list[str]:
    clean_title = re.sub(r"\s+", " ", title).strip()
    if not clean_title:
        clean_title = product_area.replace("_", " ")
    return [clean_title, f"{clean_title} 怎么处理？"]


def _chunk_id(product_area: str, source_ref: str, section_index: int, part_index: int) -> str:
    slug = re.sub(r"[^a-z0-9]+", "-", source_ref.lower()).strip("-")[:32] or "chunk"
    digest = hashlib.sha256(f"{source_ref}|{section_index}|{part_index}".encode("utf-8")).hexdigest()[:8]
    return f"w0-{product_area}-{slug}-{digest}"


def _dedupe_chunk_ids(chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    seen: dict[str, int] = {}
    for chunk in chunks:
        chunk_id = chunk["chunk_id"]
        count = seen.get(chunk_id, 0)
        seen[chunk_id] = count + 1
        if count:
            chunk["chunk_id"] = f"{chunk_id}-{count + 1}"
    return chunks


def _title_from_filename(stem: str) -> str:
    title = stem.replace("__", " / ").replace("_", " ").replace("-", " ").strip()
    title = re.sub(r"\b(?:feishu|lark)\b", "", title, flags=re.IGNORECASE)
    title = re.sub(r"\s+", " ", title).strip(" /-")
    return title or "W0 source"


def _fallback_title(body: str, source_ref: str) -> str:
    for line in body.splitlines():
        value = line.strip().lstrip("#").strip()
        if not value:
            continue
        if value.lower().endswith((".png", ".jpg", ".jpeg", ".gif", ".webp")):
            continue
        if value.startswith(("http://", "https://")):
            continue
        return value[:180]
    return _title_from_filename(source_ref)


def _read_jsonl(path: Path | str | None) -> list[dict[str, Any]]:
    if not path:
        return []
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def _read_json(path: Path | str) -> dict[str, Any]:
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        data = json.load(fh)
    if not isinstance(data, dict):
        raise ValueError(f"{path}: expected JSON object")
    return data


def _approval_record_hash(approval: dict[str, Any]) -> str:
    payload = json.dumps(approval, ensure_ascii=False, sort_keys=True)
    return "sha256:" + hashlib.sha256(payload.encode("utf-8")).hexdigest()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cleaned-dir", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--kb-version", default=DEFAULT_KB_VERSION)
    parser.add_argument("--valid-from", default=DEFAULT_VALID_FROM)
    parser.add_argument("--cases", type=Path)
    parser.add_argument("--approvals", type=Path)
    parser.add_argument("--asset-notes", type=Path)
    parser.add_argument("--links", type=Path)
    parser.add_argument("--section-labels", type=Path)
    parser.add_argument("--chunk-labels", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--allow-incomplete-inputs", action="store_true", help="Allow dry-run chunking before VL/link processing is complete.")
    parser.add_argument("--max-chars", type=int, default=2000)
    args = parser.parse_args(argv)
    chunk_documents(
        args.cleaned_dir,
        args.out,
        kb_version=args.kb_version,
        valid_from=args.valid_from,
        cases_path=args.cases,
        approvals_path=args.approvals,
        asset_notes_path=args.asset_notes,
        link_manifest_path=args.links,
        chunk_labels_path=args.chunk_labels,
        section_labels_path=args.section_labels,
        require_complete_inputs=not args.allow_incomplete_inputs,
        max_chars=args.max_chars,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
