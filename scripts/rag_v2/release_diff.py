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


# The fields that REACH kb_chunks and are not the content itself. A chunk whose
# title, question patterns or product area were rewritten while its body stayed
# byte-identical changes what retrieval matches on and what a citation says, and
# until this existed it produced no entry in any view of the diff: content_digest
# was equal, the slot key was equal, so `sections_changed` stayed empty and G5
# certified the document as attributable without ever seeing it.
#
# Deliberately excluded:
#   kb_version / valid_from -- release stamps. The internal corpus carries a
#     single kb_version, so including them would report all 526 chunks as changed
#     on every rebuild and make G5 demand a docs commit for every document.
#   chunk_id -- it hashes the content and the slot position, and both already
#     have their own view (sections_changed, and sections_added/removed via the
#     slot key). It would only ever restate them.
#   asset_refs / heading_path / document_* / evidence_kind / source_refs /
#     v2_source_kind / chunk_role / retrieval_score_hint -- the importer drops
#     these; they never reach the database, so they cannot change an answer.
SERVED_METADATA_FIELDS = (
    "acl",
    "confidence",
    "product_area",
    "question_patterns",
    "source_origin",
    "source_type",
    "surface_url",
    "title",
    "valid_to",
)


def metadata_digest(row: dict[str, Any]) -> str:
    """Digest of the served, non-content fields. Kept SEPARATE from
    content_digest on purpose: fingerprint() pairs moved documents by their
    content digests, so widening that function would repartition the move
    detection as a side effect of tightening the gate."""
    projection = {key: row.get(key) for key in SERVED_METADATA_FIELDS}
    return sha256_bytes(json.dumps(projection, ensure_ascii=False, sort_keys=True).encode("utf-8"))


def diff_corpus(old_rows: list[dict[str, Any]], new_rows: list[dict[str, Any]]) -> dict[str, Any]:
    old_slots, new_slots = slot_keys(old_rows), slot_keys(new_rows)
    old_docs = {document_key(row) for row in old_rows}
    new_docs = {document_key(row) for row in new_rows}

    changed = []
    metadata_changed = []
    for key in sorted(old_slots.keys() & new_slots.keys()):
        before, after = old_slots[key], new_slots[key]
        content_moved = content_digest(before) != content_digest(after)
        if content_moved:
            changed.append({
                "document": key[0],
                "heading": key[1],
                "occurrence": key[2],
                "runes_before": len(str(before.get("content") or "")),
                "runes_after": len(str(after.get("content") or "")),
                "chunk_id_before": before.get("chunk_id"),
                "chunk_id_after": after.get("chunk_id"),
            })
        if metadata_digest(before) != metadata_digest(after):
            metadata_changed.append({
                "document": key[0],
                "heading": key[1],
                "occurrence": key[2],
                "fields": [name for name in SERVED_METADATA_FIELDS
                           if before.get(name) != after.get(name)],
                # A reviewer needs to tell "the body was rewritten and the title
                # followed" from "only the title moved" -- the second is the
                # shape that used to be invisible.
                "content_also_changed": content_moved,
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

    def metadata_by_slot(doc: str, rows: list[dict[str, Any]]) -> dict[tuple[str, int], dict[str, Any]]:
        """One document's chunks keyed by their position WITHIN it.

        Keyed on heading path and occurrence rather than chunk_id, because a
        moved document has none of its old ids: chunk_id hashes source_path, so
        every id changes the moment the file does. This is the same key
        slot_keys uses, minus the document component that just changed.
        """
        seen: Counter = Counter()
        out: dict[tuple[str, int], dict[str, Any]] = {}
        for row in rows:
            if document_key(row) != doc:
                continue
            heading = " / ".join(str(part) for part in (row.get("heading_path") or []))
            occurrence = seen[heading]
            seen[heading] += 1
            out[(heading, occurrence)] = row
        return out

    def moved_metadata(source: str, target: str) -> tuple[list[str], bool]:
        """Served metadata that changed across a move, plus whether it could be compared.

        Move pairing is done on CONTENT digests, so a document that moved and had
        its title, question patterns or ACL rewritten under unchanged text pairs
        as a clean move and lands in the report as 仅移动 -- telling the reviewer
        there is nothing here to read, in the one release shape where a whole
        directory of documents changes path at once.

        The comparison is by heading slot, so it only works while the headings
        still line up. Rewrite the heading hierarchy in the same commit that moves
        the file and the two slot sets share no key at all: every field comparison
        runs over an EMPTY intersection and reports no change, which is the same
        answer as a genuinely untouched document. Content digests are computed
        from the body alone, so that document still pairs as a clean tier-one
        move -- nothing else in the report contradicts the 仅移动 label either.

        Returning the alignment separately is what keeps "compared and found
        nothing" apart from "could not compare". The caller must never claim
        仅移动 on the second.
        """
        before = metadata_by_slot(source, old_rows)
        after = metadata_by_slot(target, new_rows)
        aligned = before.keys() == after.keys()
        fields: set[str] = set()
        for key in before.keys() & after.keys():
            for name in SERVED_METADATA_FIELDS:
                if before[key].get(name) != after[key].get(name):
                    fields.add(name)
        return sorted(fields), aligned

    gone, fresh = old_docs - new_docs, new_docs - old_docs
    old_prints: dict[tuple[str, tuple[str, ...]], list[str]] = {}
    for doc in sorted(gone):
        old_prints.setdefault((source_of(doc), fingerprint(doc, old_rows)), []).append(doc)
    moved = []
    for doc in sorted(fresh):
        candidates = old_prints.get((source_of(doc), fingerprint(doc, new_rows)))
        if candidates:
            source = candidates.pop(0)
            fields, aligned = moved_metadata(source, doc)
            moved.append({"from": source, "to": doc, "content_changed": False,
                          "metadata_fields": fields, "slots_aligned": aligned})
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
        fields, aligned = moved_metadata(old_stems[stem], new_stems[stem])
        moved.append({"from": old_stems[stem], "to": new_stems[stem], "content_changed": True,
                      "metadata_fields": fields, "slots_aligned": aligned})
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
        "sections_metadata_changed": metadata_changed,
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
            # "不必看" is a strong claim to make about a document, and it has to
            # survive BOTH tests. Move pairing is done on content digests, so a
            # document that moved and had its title, question patterns or ACL
            # rewritten under unchanged text used to pair as a clean move and be
            # labelled 仅移动 -- actively telling the reviewer to skip it, in the
            # release shape (a whole directory changing path) where that label is
            # applied to hundreds of documents at once.
            # The comparison behind those fields is by heading slot, so a commit
            # that moves a file AND rewrites its heading hierarchy leaves the two
            # slot sets with no key in common: the field scan runs over an empty
            # intersection, finds nothing, and is indistinguishable from a
            # document nobody touched. Fail closed on that -- an unaligned pair
            # is 需审阅, never 不必看. `is True` rather than a truthy default so a
            # producer that forgets the key gets the loud answer.
            def _move_label(item):
                fields = item.get("metadata_fields") or []
                if item.get("slots_aligned") is not True:
                    if item.get("content_changed"):
                        return "改了内容，且章节层级已变——检索字段无法逐段对齐，需人工审阅"
                    return "正文逐字未变，但章节层级已变——检索字段无法逐段对齐，需人工审阅"
                if item.get("content_changed") and fields:
                    return "改了内容和检索字段：" + ", ".join(f"`{name}`" for name in fields)
                if item.get("content_changed"):
                    return "改了内容"
                if fields:
                    return "正文未变，改了检索字段：" + ", ".join(f"`{name}`" for name in fields)
                return "仅移动"

            def _is_quiet(item):
                return (item.get("slots_aligned") is True
                        and not item.get("content_changed")
                        and not (item.get("metadata_fields") or []))

            verbatim = [item for item in corpus["documents_moved"] if _is_quiet(item)]
            edited = [item for item in corpus["documents_moved"] if not _is_quiet(item)]
            lines += [
                f"**移动的文档 ({len(corpus['documents_moved'])})** —— "
                f"其中 {len(verbatim)} 个内容和检索字段都逐字未变（不必看），"
                f"{len(edited)} 个还改了别的：",
                "",
            ]
            lines += _table(
                [[f"`{item['from']}`", f"`{item['to']}`", _move_label(item)]
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
        # Only the rows whose BODY did not move. The ones where it did are
        # already in the table above, and repeating them there would bury the
        # shape this section exists to surface: a chunk whose retrieval-facing
        # metadata was rewritten under unchanged text.
        metadata_only = [item for item in corpus.get("sections_metadata_changed") or []
                         if not item.get("content_also_changed")]
        if metadata_only:
            lines += [f"**正文未变、检索字段改动的小节 ({len(metadata_only)})**", ""]
            lines += _table(
                [[f"`{item['document']}`", item["heading"] or "(顶层)",
                  ", ".join(f"`{name}`" for name in item["fields"])]
                 for item in metadata_only[:limit]],
                ["文档", "小节", "改动字段"],
            )
            if len(metadata_only) > limit:
                lines.append(f"\n…另有 {len(metadata_only) - limit} 处")
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
