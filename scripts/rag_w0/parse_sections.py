#!/usr/bin/env python3
"""Parse cleaned W0 Markdown into stable section sidecars."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path
from typing import Any


HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$")
NO_SPACE_HEADING_RE = re.compile(r"^(#{1,6})([^#\s].*?)\s*$")
INVALID_HEADING_RE = re.compile(r"^#{7,}\s*.*$")
EMPTY_HEADING_RE = re.compile(r"^#{1,6}\s*$")
ASSET_NOTE_RE = re.compile(r"<!--\s*asset_note:\s*(\{.*?\})\s*-->", re.DOTALL)
SHA256_PREFIX_LEN = 16


def parse_cleaned_docs(cleaned_dir: Path | str, out_path: Path | str) -> dict[str, int]:
    rows: list[dict[str, Any]] = []
    for doc_path in sorted(Path(cleaned_dir).glob("*.md")):
        rows.extend(parse_doc_sections(doc_path))
    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")
    return {
        "doc_count": len({row["source_ref"] for row in rows}),
        "section_count": len(rows),
    }


def parse_doc_sections(doc_path: Path | str) -> list[dict[str, Any]]:
    path = Path(doc_path)
    text = path.read_text(encoding="utf-8", errors="replace")
    body = _body_without_front_matter(text)
    source_ref = path.stem
    source_doc_id = _source_doc_id(source_ref)
    doc_hash = _content_hash(body)
    sections = _split_sections(body, fallback_title=_fallback_title(body, source_ref))
    out: list[dict[str, Any]] = []
    for index, section in enumerate(sections):
        content = section["content"].strip()
        out.append(
            {
                "source_ref": source_ref,
                "source_doc_id": source_doc_id,
                "doc_content_sha256_prefix": doc_hash,
                "section_index": index,
                "heading_level": section["heading_level"],
                "heading_text": section["heading_text"],
                "heading_path": section["heading_path"],
                "content_sha256_prefix": _content_hash(content),
                "asset_note_count": len(ASSET_NOTE_RE.findall(content)),
                "risk_flags": sorted(section["risk_flags"]),
                "content": content,
            }
        )
    return out


def _split_sections(body: str, *, fallback_title: str) -> list[dict[str, Any]]:
    sections: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None
    heading_stack: list[tuple[int, str]] = []

    def flush() -> None:
        nonlocal current
        if current is None:
            return
        current["content"] = "\n".join(current["lines"]).strip()
        current.pop("lines", None)
        if current["content"]:
            sections.append(current)
        current = None

    for raw_line in body.splitlines():
        line, heading, risk_flags = _normalize_heading(raw_line)
        if heading is not None:
            flush()
            level, title = heading
            heading_stack = [(lvl, text) for lvl, text in heading_stack if lvl < level]
            heading_stack.append((level, title))
            current = {
                "heading_level": level,
                "heading_text": title,
                "heading_path": [text for _, text in heading_stack],
                "risk_flags": set(risk_flags),
                "lines": [line],
            }
            continue
        if current is None:
            current = {
                "heading_level": 0,
                "heading_text": fallback_title,
                "heading_path": [fallback_title],
                "risk_flags": set(),
                "lines": [],
            }
        current["risk_flags"].update(risk_flags)
        current["lines"].append(raw_line)
    flush()
    return sections


def _normalize_heading(raw_line: str) -> tuple[str, tuple[int, str] | None, list[str]]:
    stripped = raw_line.strip()
    if INVALID_HEADING_RE.match(stripped):
        raise ValueError(f"invalid heading: {raw_line!r}")
    if EMPTY_HEADING_RE.match(stripped):
        return raw_line, None, ["empty_heading_marker_as_content"]
    match = HEADING_RE.match(stripped)
    if match:
        return raw_line, (len(match.group(1)), match.group(2).strip()), []
    no_space = NO_SPACE_HEADING_RE.match(stripped)
    if no_space:
        normalized = f"{no_space.group(1)} {no_space.group(2).strip()}"
        return normalized, (len(no_space.group(1)), no_space.group(2).strip()), ["heading_normalized_no_space"]
    return raw_line, None, []


def _front_matter_end(lines: list[str]) -> int:
    if not lines or lines[0].strip() != "---":
        return 0
    for idx, line in enumerate(lines[1:], start=1):
        if line.strip() == "---":
            return idx + 1
    return 0


def _body_without_front_matter(text: str) -> str:
    lines = text.splitlines()
    return "\n".join(lines[_front_matter_end(lines) :]).strip()


def _fallback_title(body: str, source_ref: str) -> str:
    for line in body.splitlines():
        value = line.strip().lstrip("#").strip()
        if value:
            return value[:180]
    return source_ref


def _source_doc_id(source_ref: str) -> str:
    return str(source_ref).split("__", 1)[0]


def _content_hash(content: str) -> str:
    normalized = re.sub(r"\s+", " ", content).strip()
    return hashlib.sha256(normalized.encode("utf-8")).hexdigest()[:SHA256_PREFIX_LEN]


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def load_sections(path: Path | str) -> dict[str, list[dict[str, Any]]]:
    rows = _read_jsonl(path)
    by_source: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        by_source.setdefault(str(row["source_ref"]), []).append(row)
    for values in by_source.values():
        values.sort(key=lambda item: int(item["section_index"]))
    return by_source


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cleaned-dir", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args(argv)
    parse_cleaned_docs(args.cleaned_dir, args.out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
