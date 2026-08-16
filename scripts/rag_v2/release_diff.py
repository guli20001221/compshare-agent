#!/usr/bin/env python3
"""What changed between the released corpus and a candidate.

This exists because the publish gate is a human, and a human cannot review two
JSONL files totalling 6 MB. Splitting import from publish only buys a review if
the reviewer is handed something reviewable; otherwise the manual step is a
button that always gets pressed.

Chunks are compared by SLOT, not by chunk_id. chunk_id digests the content
(pipeline.py: source_id/source_path/section_index/part_index/content), so an
edited paragraph reads as one removal plus one addition and every real change
looks like churn. The slot -- source ref plus heading path plus occurrence --
survives an edit, so an edit reads as an edit and only a genuinely new or
deleted section reads as one.

Captions are compared by asset_id, which is a content digest. A caption that
CHANGED under an unchanged asset_id is the VL model saying something different
about identical bytes: not a docs change at all, and the one row in this report
that a reviewer should read as noise rather than signal.
"""
from __future__ import annotations

import argparse
import json
from collections import Counter
from pathlib import Path
from typing import Any

try:
    from .pipeline import normalized_file_digest, sha256_bytes
except ImportError:  # pragma: no cover - direct script execution
    from pipeline import normalized_file_digest, sha256_bytes  # type: ignore


def read_corpus(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def document_key(row: dict[str, Any]) -> str:
    refs = row.get("source_refs") or []
    return str(refs[0]) if refs else str(row.get("document_id") or "?")


def slot_keys(rows: list[dict[str, Any]]) -> dict[tuple[str, str, int], dict[str, Any]]:
    """Position of each chunk within its document, independent of its content."""
    seen: Counter = Counter()
    slots: dict[tuple[str, str, int], dict[str, Any]] = {}
    for row in rows:
        doc = document_key(row)
        heading = " / ".join(str(part) for part in (row.get("heading_path") or []))
        occurrence = seen[(doc, heading)]
        seen[(doc, heading)] += 1
        slots[(doc, heading, occurrence)] = row
    return slots


def content_digest(row: dict[str, Any]) -> str:
    return sha256_bytes(str(row.get("content") or "").encode("utf-8"))


def diff_corpus(old_rows: list[dict[str, Any]], new_rows: list[dict[str, Any]]) -> dict[str, Any]:
    old_slots, new_slots = slot_keys(old_rows), slot_keys(new_rows)
    old_docs = {document_key(row) for row in old_rows}
    new_docs = {document_key(row) for row in new_rows}

    changed = []
    for key in sorted(old_slots.keys() & new_slots.keys()):
        before, after = old_slots[key], new_slots[key]
        if content_digest(before) == content_digest(after):
            continue
        changed.append({
            "document": key[0],
            "heading": key[1],
            "occurrence": key[2],
            "runes_before": len(str(before.get("content") or "")),
            "runes_after": len(str(after.get("content") or "")),
            "chunk_id_before": before.get("chunk_id"),
            "chunk_id_after": after.get("chunk_id"),
        })
    changed.sort(key=lambda item: abs(item["runes_after"] - item["runes_before"]), reverse=True)

    # A docs restructure renames every path at once. Reported naively that is
    # hundreds of additions beside hundreds of deletions, which buries the two
    # real edits underneath it -- the pages/ -> content/ App Router migration
    # rendered as 232 added and 235 removed documents. A document whose chunks
    # carry exactly the old document's content digests has MOVED, and saying so
    # is the difference between a reviewable report and a wall.
    # Both tiers are scoped to the SOURCE. The external corpus carries three
    # independent snapshots (comfyui, digital-human, voice-audio); without this
    # a guide.md deleted from one and added to another pairs as a move, and both
    # halves vanish from the report -- the exact rows a reviewer is there for.
    def source_of(doc: str) -> str:
        return doc.partition(":")[0]

    def fingerprint(doc: str, rows: list[dict[str, Any]]) -> tuple[str, ...]:
        return tuple(sorted(content_digest(row) for row in rows if document_key(row) == doc))

    gone, fresh = old_docs - new_docs, new_docs - old_docs
    old_prints: dict[tuple[str, tuple[str, ...]], list[str]] = {}
    for doc in sorted(gone):
        old_prints.setdefault((source_of(doc), fingerprint(doc, old_rows)), []).append(doc)
    moved = []
    for doc in sorted(fresh):
        candidates = old_prints.get((source_of(doc), fingerprint(doc, new_rows)))
        if candidates:
            moved.append({"from": candidates.pop(0), "to": doc, "content_changed": False})
    moved_from = {item["from"] for item in moved}
    moved_to = {item["to"] for item in moved}

    # Second tier: the same migration usually rewrites the file as well
    # (pages/x.md -> content/x.mdx), so the digests differ and tier one misses
    # it. Pair on the path with its top-level directory and extension dropped,
    # and ONLY when that key is unique on both sides -- an ambiguous match is
    # reported as a real addition and a real deletion, because guessing a rename
    # would hide a document that genuinely appeared.
    def path_stem(doc: str) -> tuple[str, str]:
        source, _, path = doc.partition(":")
        head, _, tail = path.partition("/")
        return source, (tail or head).rsplit(".", 1)[0].lower()

    def unique_stems(docs: set[str]) -> dict[tuple[str, str], str]:
        counts: Counter = Counter(path_stem(doc) for doc in docs)
        return {path_stem(doc): doc for doc in docs if counts[path_stem(doc)] == 1}

    old_stems = unique_stems(gone - moved_from)
    new_stems = unique_stems(fresh - moved_to)
    for stem in sorted(old_stems.keys() & new_stems.keys()):
        moved.append({"from": old_stems[stem], "to": new_stems[stem], "content_changed": True})
    moved_from = {item["from"] for item in moved}
    moved_to = {item["to"] for item in moved}

    added = [{"document": key[0], "heading": key[1], "runes": len(str(new_slots[key].get("content") or ""))}
             for key in sorted(new_slots.keys() - old_slots.keys()) if key[0] not in moved_to]
    removed = [{"document": key[0], "heading": key[1], "runes": len(str(old_slots[key].get("content") or ""))}
               for key in sorted(old_slots.keys() - new_slots.keys()) if key[0] not in moved_from]

    def area_counts(rows: list[dict[str, Any]]) -> Counter:
        return Counter(str(row.get("product_area") or "?") for row in rows)

    before_areas, after_areas = area_counts(old_rows), area_counts(new_rows)
    areas = []
    for area in sorted(before_areas.keys() | after_areas.keys()):
        if before_areas[area] != after_areas[area]:
            areas.append({"product_area": area, "before": before_areas[area], "after": after_areas[area]})

    return {
        "chunks_before": len(old_rows),
        "chunks_after": len(new_rows),
        "documents_before": len(old_docs),
        "documents_after": len(new_docs),
        "documents_added": sorted(fresh - moved_to),
        "documents_removed": sorted(gone - moved_from),
        "documents_moved": moved,
        "sections_changed": changed,
        "sections_added": added,
        "sections_removed": removed,
        "product_areas_changed": areas,
    }


def diff_captions(old_lock: dict[str, Any], new_lock: dict[str, Any]) -> dict[str, Any]:
    def by_asset(lock: dict[str, Any]) -> dict[str, str]:
        out: dict[str, str] = {}
        for note in lock.get("notes") or []:
            out.setdefault(str(note["asset_id"]), str(note.get("description") or ""))
        return out

    before, after = by_asset(old_lock), by_asset(new_lock)
    rewritten = [
        {"asset_id": asset_id, "before": before[asset_id], "after": after[asset_id]}
        for asset_id in sorted(before.keys() & after.keys())
        if before[asset_id] != after[asset_id]
    ]
    return {
        "contract_before": old_lock.get("contract"),
        "contract_after": new_lock.get("contract"),
        "captions_before": len(before),
        "captions_after": len(after),
        "images_added": sorted(after.keys() - before.keys()),
        "images_removed": sorted(before.keys() - after.keys()),
        "captions_rewritten_for_identical_bytes": rewritten,
    }


def _table(rows: list[list[str]], header: list[str]) -> list[str]:
    lines = ["| " + " | ".join(header) + " |", "|" + "|".join(["---"] * len(header)) + "|"]
    lines += ["| " + " | ".join(cell.replace("|", "\\|") for cell in row) + " |" for row in rows]
    return lines


def render_markdown(report: dict[str, Any], *, limit: int = 25) -> str:
    lines = ["# 知识库候选版本 diff", ""]
    head = report["headline"]
    lines += _table(
        [[name, str(value["before"]), str(value["after"]), value["verdict"]]
         for name, value in head.items()],
        ["", "已发布", "候选", ""],
    )
    lines.append("")

    for corpus_name, corpus in report["corpora"].items():
        lines += [f"## {corpus_name}", ""]
        lines.append(
            f"chunk {corpus['chunks_before']} → {corpus['chunks_after']}，"
            f"文档 {corpus['documents_before']} → {corpus['documents_after']}"
        )
        lines.append("")
        if corpus.get("documents_moved"):
            verbatim = [item for item in corpus["documents_moved"] if not item.get("content_changed")]
            edited = [item for item in corpus["documents_moved"] if item.get("content_changed")]
            lines += [
                f"**移动的文档 ({len(corpus['documents_moved'])})** —— "
                f"其中 {len(verbatim)} 个内容逐字未变（不必看），{len(edited)} 个同时改了内容：",
                "",
            ]
            lines += _table(
                [[f"`{item['from']}`", f"`{item['to']}`", "改了内容" if item.get("content_changed") else "仅移动"]
                 for item in (edited + verbatim)[:limit]],
                ["原路径", "新路径", ""],
            )
            if len(corpus["documents_moved"]) > limit:
                lines.append(f"\n…另有 {len(corpus['documents_moved']) - limit} 个")
            lines.append("")
        for label, key in (("新增文档", "documents_added"), ("删除文档", "documents_removed")):
            if corpus[key]:
                lines += [f"**{label} ({len(corpus[key])})**", ""]
                lines += [f"- `{item}`" for item in corpus[key][:limit]]
                if len(corpus[key]) > limit:
                    lines.append(f"- …另有 {len(corpus[key]) - limit} 个")
                lines.append("")
        if corpus["sections_changed"]:
            lines += [f"**内容改动的小节 ({len(corpus['sections_changed'])})**，按改动幅度排序", ""]
            lines += _table(
                [[f"`{item['document']}`", item["heading"] or "(顶层)",
                  f"{item['runes_before']} → {item['runes_after']}",
                  f"{item['runes_after'] - item['runes_before']:+d}"]
                 for item in corpus["sections_changed"][:limit]],
                ["文档", "小节", "字数", "增减"],
            )
            if len(corpus["sections_changed"]) > limit:
                lines.append(f"\n…另有 {len(corpus['sections_changed']) - limit} 处")
            lines.append("")
        for label, key in (("新增小节", "sections_added"), ("删除小节", "sections_removed")):
            if corpus[key]:
                lines += [f"**{label} ({len(corpus[key])})**", ""]
                lines += _table(
                    [[f"`{item['document']}`", item["heading"] or "(顶层)", str(item["runes"])]
                     for item in corpus[key][:limit]],
                    ["文档", "小节", "字数"],
                )
                if len(corpus[key]) > limit:
                    lines.append(f"\n…另有 {len(corpus[key]) - limit} 处")
                lines.append("")
        if corpus["product_areas_changed"]:
            lines += ["**product_area 分布变化**", ""]
            lines += _table(
                [[item["product_area"], str(item["before"]), str(item["after"])]
                 for item in corpus["product_areas_changed"]],
                ["product_area", "已发布", "候选"],
            )
            lines.append("")

    captions = report.get("captions")
    if captions:
        lines += ["## 图片说明", ""]
        if captions["contract_before"] != captions["contract_after"]:
            lines += [
                "> ⚠️ caption 契约变了 —— 本次所有图片说明都是重新生成的，",
                "> 下面的「字节未变但说明变了」不代表模型不稳定。",
                "",
            ]
        lines.append(
            f"说明 {captions['captions_before']} → {captions['captions_after']}，"
            f"新图 {len(captions['images_added'])}，撤下 {len(captions['images_removed'])}"
        )
        lines.append("")
        rewritten = captions["captions_rewritten_for_identical_bytes"]
        if rewritten:
            lines += [
                f"**字节未变但说明变了 ({len(rewritten)})** —— 同一张图，模型这次说了别的。",
                "用 LLM 描述图片本来就不可复现，这一节是噪声地板，不是文档改动：",
                "",
            ]
            lines += _table(
                [[f"`{item['asset_id']}`", item["before"][:60], item["after"][:60]]
                 for item in rewritten[:limit]],
                ["asset", "原说明", "新说明"],
            )
            if len(rewritten) > limit:
                lines.append(f"\n…另有 {len(rewritten) - limit} 张")
            lines.append("")

    assets = report.get("assets") or {}
    stale_unknown = "assets" in report and not assets.get("degradations_reported")
    if assets.get("degradations") or assets.get("blocking_failures") or stale_unknown:
        lines += ["## 需要人看的构建降级", ""]
        if stale_unknown:
            lines.append(
                "- **回源校验情况未知**：asset_report.json 里没有 `degradations` 字段，"
                "说明它是更早的构建产物，没有回答过这个问题。不要读成「没有降级」。"
            )
        elif assets.get("degradations"):
            lines.append(
                f"- **{len(assets['degradations'])} 张图未能回源校验**，用的是缓存字节；"
                "其中平台来源的会阻断发布（除非显式 `--allow-stale-remote`）"
            )
        if assets.get("blocking_failures"):
            lines.append(f"- **{assets['blocking_failures']} 张必需图片处理失败**")
        lines.append("")

    lines += ["---", "", "对照口径：已发布 = 当前 git HEAD 的语料；候选 = 本次构建产物。"]
    return "\n".join(lines) + "\n"


def build_report(
    *,
    old_internal: Path, new_internal: Path,
    old_external: Path, new_external: Path,
    old_lock: Path | None = None, new_lock: Path | None = None,
    asset_report: Path | None = None,
) -> dict[str, Any]:
    def digest(path: Path) -> str:
        return normalized_file_digest(path) if path.exists() else "(缺失)"

    internal = diff_corpus(read_corpus(old_internal), read_corpus(new_internal))
    external = diff_corpus(read_corpus(old_external), read_corpus(new_external))

    def verdict(before: Any, after: Any) -> str:
        return "未变" if before == after else "变了"

    headline = {
        "平台语料 chunk": {"before": internal["chunks_before"], "after": internal["chunks_after"],
                           "verdict": verdict(internal["chunks_before"], internal["chunks_after"])},
        "外部语料 chunk": {"before": external["chunks_before"], "after": external["chunks_after"],
                           "verdict": verdict(external["chunks_before"], external["chunks_after"])},
        "平台语料 digest": {"before": digest(old_internal)[:16], "after": digest(new_internal)[:16],
                            "verdict": verdict(digest(old_internal), digest(new_internal))},
        "外部语料 digest": {"before": digest(old_external)[:16], "after": digest(new_external)[:16],
                            "verdict": verdict(digest(old_external), digest(new_external))},
    }

    report: dict[str, Any] = {
        "headline": headline,
        "corpora": {"平台语料 (stage2b)": internal, "外部语料 (external)": external},
    }

    if old_lock and new_lock and old_lock.exists() and new_lock.exists():
        report["captions"] = diff_captions(
            json.loads(old_lock.read_text(encoding="utf-8")),
            json.loads(new_lock.read_text(encoding="utf-8")),
        )
    if asset_report and asset_report.exists():
        data = json.loads(asset_report.read_text(encoding="utf-8"))
        # `degradations` is distinguished from absent, not coerced to empty. A
        # report written before build.py started emitting the key has no opinion
        # about stale remote images, and `.get(...) or []` would render that as
        # "nothing degraded" -- a clean bill of health from a file that never
        # examined the question. The shipped deploy/kb/v2/asset_report.json is
        # exactly that file: its keys are described/failures/published/
        # runtime_mode and nothing else.
        report["assets"] = {
            "degradations": data.get("degradations"),
            "degradations_reported": "degradations" in data,
            "blocking_failures": sum(
                1 for item in (data.get("failures") or []) if item.get("severity") == "error"
            ),
        }
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Render a reviewable diff between the released corpus and a candidate.")
    parser.add_argument("--old-internal", type=Path, required=True)
    parser.add_argument("--new-internal", type=Path, required=True)
    parser.add_argument("--old-external", type=Path, required=True)
    parser.add_argument("--new-external", type=Path, required=True)
    parser.add_argument("--old-lock", type=Path)
    parser.add_argument("--new-lock", type=Path)
    parser.add_argument("--asset-report", type=Path)
    parser.add_argument("--out-md", type=Path, required=True)
    parser.add_argument("--out-json", type=Path)
    args = parser.parse_args(argv)

    report = build_report(
        old_internal=args.old_internal, new_internal=args.new_internal,
        old_external=args.old_external, new_external=args.new_external,
        old_lock=args.old_lock, new_lock=args.new_lock, asset_report=args.asset_report,
    )
    args.out_md.parent.mkdir(parents=True, exist_ok=True)
    args.out_md.write_text(render_markdown(report), encoding="utf-8")
    if args.out_json:
        args.out_json.write_text(
            json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    internal = report["corpora"]["平台语料 (stage2b)"]
    external = report["corpora"]["外部语料 (external)"]
    print(
        f"chunks {internal['chunks_before']}->{internal['chunks_after']} / "
        f"{external['chunks_before']}->{external['chunks_after']}  "
        f"sections_changed={len(internal['sections_changed']) + len(external['sections_changed'])}  "
        f"docs_added={len(internal['documents_added']) + len(external['documents_added'])}  "
        f"docs_removed={len(internal['documents_removed']) + len(external['documents_removed'])}  "
        f"report={args.out_md}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
