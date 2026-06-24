#!/usr/bin/env python3
"""Audit compshare-docs coverage in the deployed knowledge base."""

from __future__ import annotations

import argparse
import csv
import json
import re
import subprocess
from collections import Counter, defaultdict
from dataclasses import dataclass
from pathlib import Path
from typing import Any


KB_FILES = ("stage2b_w0.jsonl", "external_w0.jsonl", "curated_faq.jsonl")
DOC_SUFFIXES = {".md", ".mdx"}
SOURCE_PREFIX = "gitlab-compshare-docs__"
NOISE_FILENAMES = {"_app.mdx", "README.md"}


@dataclass
class DocAudit:
    path: str
    title: str
    section: str
    candidate_refs: list[str]
    matched_refs: list[str]
    matched_chunk_count: int
    matched_product_areas: list[str]
    exact_line_total: int
    exact_line_hits_matched: int
    exact_line_hits_all: int
    title_hit_all: bool
    best_chunk_id: str
    best_chunk_title: str
    best_chunk_score: float
    status: str
    priority: str

    @property
    def matched_hit_rate(self) -> float:
        if self.exact_line_total == 0:
            return 0.0
        return self.exact_line_hits_matched / self.exact_line_total

    @property
    def all_hit_rate(self) -> float:
        if self.exact_line_total == 0:
            return 0.0
        return self.exact_line_hits_all / self.exact_line_total


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docs-root", type=Path, required=True)
    parser.add_argument("--repo-root", type=Path, default=Path.cwd())
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()

    docs_root = args.docs_root.resolve()
    repo_root = args.repo_root.resolve()
    out_dir = args.out_dir.resolve()
    out_dir.mkdir(parents=True, exist_ok=True)

    docs = load_docs(docs_root)
    chunks = load_chunks(repo_root / "deploy" / "kb")
    by_ref = chunks_by_ref(chunks)
    all_kb_text = normalize_text("\n".join(str(c.get("content") or "") for c in chunks))
    audits = [audit_doc(doc, chunks, by_ref, all_kb_text) for doc in docs]
    stale_refs = find_stale_refs(by_ref, docs)

    summary = build_summary(docs_root, repo_root, docs, chunks, audits, stale_refs)
    write_outputs(out_dir, args.report, summary, audits, stale_refs)
    return 0


def run_git(path: Path, *args: str) -> str:
    try:
        return subprocess.check_output(["git", "-C", str(path), *args], text=True, stderr=subprocess.DEVNULL).strip()
    except subprocess.CalledProcessError:
        return ""


def load_docs(root: Path) -> list[dict[str, Any]]:
    files = run_git(root, "ls-files", "*.md", "*.mdx").splitlines()
    docs: list[dict[str, Any]] = []
    for rel in files:
        path = root / rel
        if path.name in NOISE_FILENAMES:
            continue
        if path.suffix.lower() not in DOC_SUFFIXES:
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        docs.append(
            {
                "path": rel.replace("\\", "/"),
                "text": text,
                "title": first_heading(text) or path.stem,
                "section": section_for_path(rel),
                "candidate_refs": candidate_refs_for_path(rel),
                "salient_lines": salient_lines(text),
            }
        )
    return sorted(docs, key=lambda d: d["path"])


def load_chunks(kb_dir: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for name in KB_FILES:
        path = kb_dir / name
        with path.open("r", encoding="utf-8") as fh:
            for row_no, line in enumerate(fh, start=1):
                if not line.strip():
                    continue
                item = json.loads(line)
                item["_kb_file"] = name
                item["_row"] = row_no
                item["_norm_content"] = normalize_text(str(item.get("content") or ""))
                rows.append(item)
    return rows


def chunks_by_ref(chunks: list[dict[str, Any]]) -> dict[str, list[dict[str, Any]]]:
    out: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for chunk in chunks:
        for ref in chunk.get("source_refs") or []:
            out[normalize_ref(str(ref))].append(chunk)
    return out


def audit_doc(
    doc: dict[str, Any],
    chunks: list[dict[str, Any]],
    by_ref: dict[str, list[dict[str, Any]]],
    all_kb_text: str,
) -> DocAudit:
    matched: list[dict[str, Any]] = []
    matched_ref_names: list[str] = []
    for ref in doc["candidate_refs"]:
        key = normalize_ref(ref)
        if key in by_ref:
            matched_ref_names.append(ref)
            matched.extend(by_ref[key])
    matched = dedupe_chunks(matched)
    matched_text = normalize_text("\n".join(str(c.get("content") or "") for c in matched))
    line_checks = [normalize_text(line) for line in doc["salient_lines"]]
    line_checks = [line for line in line_checks if len(line) >= 12]
    matched_hits = sum(1 for line in line_checks if line and line in matched_text)
    all_hits = sum(1 for line in line_checks if line and line in all_kb_text)
    title_norm = normalize_text(str(doc["title"]))
    title_hit_all = bool(title_norm and title_norm in all_kb_text)
    best = best_chunk_match(doc, chunks)
    product_areas = sorted({str(c.get("product_area") or "") for c in matched if c.get("product_area")})
    status = classify_status(bool(matched), len(line_checks), matched_hits, all_hits, title_hit_all, best[2])
    priority = classify_priority(doc["path"], status)
    return DocAudit(
        path=doc["path"],
        title=str(doc["title"]),
        section=str(doc["section"]),
        candidate_refs=list(doc["candidate_refs"]),
        matched_refs=sorted(set(matched_ref_names)),
        matched_chunk_count=len(matched),
        matched_product_areas=product_areas,
        exact_line_total=len(line_checks),
        exact_line_hits_matched=matched_hits,
        exact_line_hits_all=all_hits,
        title_hit_all=title_hit_all,
        best_chunk_id=best[0],
        best_chunk_title=best[1],
        best_chunk_score=best[2],
        status=status,
        priority=priority,
    )


def classify_status(
    has_ref_match: bool,
    total: int,
    matched_hits: int,
    all_hits: int,
    title_hit_all: bool,
    best_score: float,
) -> str:
    matched_rate = matched_hits / total if total else 0.0
    all_rate = all_hits / total if total else 0.0
    if has_ref_match and (matched_rate >= 0.35 or matched_hits >= 4):
        return "covered_by_ref"
    if has_ref_match and all_rate >= 0.80:
        return "covered_by_ref_alias"
    if has_ref_match:
        return "ref_match_low_content_overlap"
    if all_rate >= 0.80:
        return "likely_covered_by_content"
    if all_rate >= 0.10 or all_hits >= 4 or (title_hit_all and best_score >= 0.25):
        return "partial_content_overlap"
    if best_score >= 0.38:
        return "possible_content_overlap"
    return "not_found"


def classify_priority(path: str, status: str) -> str:
    if status in {"covered_by_ref", "covered_by_ref_alias"}:
        return "ok"
    lower = path.lower()
    if lower.startswith(("pages/agent/", "pages/operation/", "pages/uaccount/", "pages/overview/")):
        return "high"
    if lower.startswith("pages/modelverse/"):
        return "high" if status == "not_found" else "medium"
    if lower.startswith("pages/gpus/"):
        if "/instance/" in lower or "/image/" in lower or "/data/" in lower or "/team/" in lower:
            return "medium"
        return "high"
    if lower.startswith("pages/serviceagreement/"):
        return "medium"
    return "low"


def best_chunk_match(doc: dict[str, Any], chunks: list[dict[str, Any]]) -> tuple[str, str, float]:
    doc_tokens = token_set(str(doc["title"]) + "\n" + "\n".join(doc["salient_lines"][:30]))
    if not doc_tokens:
        return "", "", 0.0
    best_id = ""
    best_title = ""
    best_score = 0.0
    for chunk in chunks:
        chunk_tokens = token_set(str(chunk.get("title") or "") + "\n" + str(chunk.get("content") or ""))
        if not chunk_tokens:
            continue
        overlap = len(doc_tokens & chunk_tokens)
        score = overlap / max(1, min(len(doc_tokens), len(chunk_tokens)))
        if score > best_score:
            best_score = score
            best_id = str(chunk.get("chunk_id") or "")
            best_title = str(chunk.get("title") or "")
    return best_id, best_title, round(best_score, 4)


def find_stale_refs(by_ref: dict[str, list[dict[str, Any]]], docs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    current_candidates = {normalize_ref(ref) for doc in docs for ref in doc["candidate_refs"]}
    stale = []
    for ref, ref_chunks in sorted(by_ref.items()):
        if not ref.startswith(SOURCE_PREFIX):
            continue
        if ref in current_candidates:
            continue
        stale.append(
            {
                "source_ref": ref,
                "chunk_count": len(ref_chunks),
                "product_areas": sorted({str(c.get("product_area") or "") for c in ref_chunks if c.get("product_area")}),
                "sample_title": str(ref_chunks[0].get("title") or "") if ref_chunks else "",
            }
        )
    return stale


def build_summary(
    docs_root: Path,
    repo_root: Path,
    docs: list[dict[str, Any]],
    chunks: list[dict[str, Any]],
    audits: list[DocAudit],
    stale_refs: list[dict[str, Any]],
) -> dict[str, Any]:
    return {
        "docs_root": str(docs_root),
        "docs_head": run_git(docs_root, "rev-parse", "HEAD"),
        "docs_remote": run_git(docs_root, "remote", "get-url", "origin"),
        "agent_repo_root": str(repo_root),
        "agent_head": run_git(repo_root, "rev-parse", "HEAD"),
        "agent_branch": run_git(repo_root, "branch", "--show-current"),
        "doc_count": len(docs),
        "doc_sections": dict(Counter(d["section"] for d in docs).most_common()),
        "kb_chunk_count": len(chunks),
        "kb_files": dict(Counter(c["_kb_file"] for c in chunks).most_common()),
        "kb_product_areas": dict(Counter(str(c.get("product_area") or "") for c in chunks).most_common()),
        "statuses": dict(Counter(a.status for a in audits).most_common()),
        "priorities": dict(Counter(a.priority for a in audits).most_common()),
        "stale_source_ref_count": len(stale_refs),
    }


def write_outputs(out_dir: Path, report_path: Path, summary: dict[str, Any], audits: list[DocAudit], stale_refs: list[dict[str, Any]]) -> None:
    (out_dir / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True), encoding="utf-8")
    (out_dir / "stale_source_refs.json").write_text(json.dumps(stale_refs, ensure_ascii=False, indent=2, sort_keys=True), encoding="utf-8")
    with (out_dir / "doc_coverage.csv").open("w", encoding="utf-8-sig", newline="") as fh:
        writer = csv.DictWriter(
            fh,
            fieldnames=[
                "path",
                "section",
                "title",
                "status",
                "priority",
                "matched_chunk_count",
                "matched_product_areas",
                "matched_hit_rate",
                "all_hit_rate",
                "best_chunk_score",
                "best_chunk_id",
                "best_chunk_title",
                "matched_refs",
            ],
        )
        writer.writeheader()
        for audit in audits:
            writer.writerow(
                {
                    "path": audit.path,
                    "section": audit.section,
                    "title": audit.title,
                    "status": audit.status,
                    "priority": audit.priority,
                    "matched_chunk_count": audit.matched_chunk_count,
                    "matched_product_areas": ";".join(audit.matched_product_areas),
                    "matched_hit_rate": f"{audit.matched_hit_rate:.3f}",
                    "all_hit_rate": f"{audit.all_hit_rate:.3f}",
                    "best_chunk_score": f"{audit.best_chunk_score:.3f}",
                    "best_chunk_id": audit.best_chunk_id,
                    "best_chunk_title": audit.best_chunk_title,
                    "matched_refs": ";".join(audit.matched_refs),
                }
            )
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(render_report(summary, audits, stale_refs, out_dir), encoding="utf-8")


def render_report(summary: dict[str, Any], audits: list[DocAudit], stale_refs: list[dict[str, Any]], out_dir: Path) -> str:
    statuses = Counter(a.status for a in audits)
    gap_statuses = {"not_found", "possible_content_overlap", "partial_content_overlap", "ref_match_low_content_overlap"}
    high_gaps = [a for a in audits if a.priority == "high" and a.status in gap_statuses]
    medium_gaps = [a for a in audits if a.priority == "medium" and a.status in gap_statuses]
    likely = [a for a in audits if a.status == "likely_covered_by_content"]
    low_overlap = [a for a in audits if a.status == "ref_match_low_content_overlap"]
    lines = [
        "# Compshare Docs vs Knowledge Base Audit (2026-06-23)",
        "",
        "## Scope",
        "",
        f"- Agent repo branch: `{summary['agent_branch']}`",
        f"- Agent repo head: `{summary['agent_head']}`",
        f"- Internal docs remote: `{summary['docs_remote']}`",
        f"- Internal docs head: `{summary['docs_head']}`",
        f"- Audited docs: {summary['doc_count']} Markdown/MDX files",
        f"- Audited KB chunks: {summary['kb_chunk_count']} chunks across {', '.join(f'{k}={v}' for k, v in summary['kb_files'].items())}",
        "",
        "## Result",
        "",
        f"- Direct source-reference coverage: {statuses.get('covered_by_ref', 0)} docs.",
        f"- Covered, but through a source-ref alias or rewritten content: {statuses.get('covered_by_ref_alias', 0)} docs.",
        f"- Likely covered by content but source refs did not map cleanly: {statuses.get('likely_covered_by_content', 0)} docs.",
        f"- Partially overlapped with existing KB content: {statuses.get('partial_content_overlap', 0)} docs.",
        f"- Ref matched but current text overlap is low: {statuses.get('ref_match_low_content_overlap', 0)} docs.",
        f"- Not found / weak overlap: {statuses.get('not_found', 0) + statuses.get('possible_content_overlap', 0)} docs.",
        f"- Stale KB source refs not mapped to current doc paths: {summary['stale_source_ref_count']}.",
        "",
        "Interpretation: the deployed KB already includes the main Agent community docs, but the full docs tree is not completely represented. The main actionable gaps are new or expanded customer-facing docs around Codex Agent Plan setup, GPU region/storage guidance, and the SwitchChargeType API.",
        "",
        "## High-Priority Review List",
        "",
    ]
    lines.extend(render_audit_table(high_gaps[:40]))
    if not high_gaps:
        lines.append("No high-priority gaps found.")
    lines.extend(
        [
            "",
            "## Medium-Priority Review List",
            "",
        ]
    )
    lines.extend(render_audit_table(medium_gaps[:40]))
    if not medium_gaps:
        lines.append("No medium-priority gaps found.")
    lines.extend(
        [
            "",
            "## Source-Reference Cleanup Candidates",
            "",
            "These KB refs did not map to a current docs path candidate. Some are expected because older pipeline refs used stems or alternate path slugs, but they are worth cleaning up in the next rebuild.",
            "",
        ]
    )
    lines.extend(render_stale_table(stale_refs[:40]))
    lines.extend(
        [
            "",
            "## Evidence Files",
            "",
            f"- Summary JSON: `{out_dir / 'summary.json'}`",
            f"- Per-document coverage CSV: `{out_dir / 'doc_coverage.csv'}`",
            f"- Stale source refs JSON: `{out_dir / 'stale_source_refs.json'}`",
            "",
            "## Recommendation",
            "",
            "Update the KB in the next governed corpus rebuild for the high-priority gaps and the SwitchChargeType API. Do not hand-edit the deployed JSONL alone: regenerate the corpus, rebuild the qwen3 sidecar, and update digest pins together.",
        ]
    )
    if likely or low_overlap:
        lines.extend(
            [
                "",
                "## Notes",
                "",
                f"- {len(likely)} docs appear in KB content but lack a clean source-ref match; this points to source-ref naming drift, not necessarily missing content.",
                f"- {len(low_overlap)} docs have source-ref matches but low exact-line overlap; this can mean either doc drift or expected rewriting/cleaning. Review before classifying as stale.",
            ]
        )
    return "\n".join(lines).rstrip() + "\n"


def render_audit_table(items: list[DocAudit]) -> list[str]:
    if not items:
        return []
    lines = ["| Path | Status | Best KB Match | Ref Chunks | Text Hit |", "| --- | --- | --- | ---: | ---: |"]
    for item in items:
        lines.append(
            "| "
            + " | ".join(
                [
                    md_cell(item.path),
                    md_cell(item.status),
                    md_cell(f"{item.best_chunk_title} ({item.best_chunk_score:.2f})" if item.best_chunk_title else f"{item.best_chunk_score:.2f}"),
                    str(item.matched_chunk_count),
                    f"{item.matched_hit_rate:.0%}",
                ]
            )
            + " |"
        )
    return lines


def render_stale_table(items: list[dict[str, Any]]) -> list[str]:
    if not items:
        return ["No stale source-ref candidates found."]
    lines = ["| Source Ref | Chunks | Product Areas | Sample Title |", "| --- | ---: | --- | --- |"]
    for item in items:
        lines.append(
            "| "
            + " | ".join(
                [
                    md_cell(item["source_ref"]),
                    str(item["chunk_count"]),
                    md_cell(",".join(item["product_areas"])),
                    md_cell(item["sample_title"]),
                ]
            )
            + " |"
        )
    return lines


def md_cell(value: str) -> str:
    return str(value).replace("|", "\\|").replace("\n", " ")[:220]


def dedupe_chunks(chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    seen = set()
    out = []
    for chunk in chunks:
        chunk_id = str(chunk.get("chunk_id") or "")
        if chunk_id in seen:
            continue
        seen.add(chunk_id)
        out.append(chunk)
    return out


def candidate_refs_for_path(rel: str) -> list[str]:
    rel = rel.replace("\\", "/")
    no_ext = re.sub(r"\.(md|mdx)$", "", rel, flags=re.IGNORECASE)
    without_pages = no_ext.removeprefix("pages/")
    stem = Path(rel).stem
    variants = {
        stem,
        safe_name(stem),
        safe_name(without_pages),
        safe_name(without_pages, sep="-"),
        safe_name(no_ext),
        safe_name(no_ext, sep="-"),
        without_pages.replace("/", "_"),
        without_pages.replace("/", "-"),
    }
    # Older generated refs sometimes singularized agent docs through bare stems.
    if without_pages.startswith("agent/"):
        variants.add(without_pages.split("/")[-1])
        variants.add("agents-" + without_pages.split("/")[-1])
    out = []
    for value in sorted(v for v in variants if v):
        out.append(SOURCE_PREFIX + value)
        out.append(SOURCE_PREFIX + value.lower())
    return sorted(set(out), key=lambda s: (s.lower(), s))


def normalize_ref(ref: str) -> str:
    return ref.replace("\\", "/").strip().lower()


def safe_name(value: str, sep: str = "_") -> str:
    chars = []
    for ch in value:
        if ch.isalnum() or ch in {"-", "_"}:
            chars.append(ch)
        elif ch in {"/", "\\", " "}:
            chars.append(sep)
        else:
            chars.append(sep)
    return re.sub(rf"{re.escape(sep)}+", sep, "".join(chars)).strip(sep)


def section_for_path(rel: str) -> str:
    parts = rel.replace("\\", "/").split("/")
    if len(parts) >= 2 and parts[0] == "pages":
        return parts[1]
    return parts[0] if parts else ""


def first_heading(text: str) -> str:
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("#"):
            return stripped.lstrip("#").strip()
    return ""


def salient_lines(text: str) -> list[str]:
    lines: list[str] = []
    in_code = False
    for raw in text.splitlines():
        line = raw.strip()
        if line.startswith("```"):
            in_code = not in_code
            continue
        if in_code or not line:
            continue
        if line.startswith("![") or line.startswith("<img") or line.startswith("import "):
            continue
        if re.fullmatch(r"[-|:\s]+", line):
            continue
        line = re.sub(r"!\[[^\]]*\]\([^)]+\)", "", line)
        line = re.sub(r"\[([^\]]+)\]\([^)]+\)", r"\1", line)
        line = re.sub(r"\s+", " ", line).strip()
        if len(line) >= 14:
            lines.append(line[:180])
    # Keep this bounded so large API pages do not dominate.
    return lines[:120]


def normalize_text(value: str) -> str:
    value = value.lower()
    value = re.sub(r"!\[[^\]]*\]\([^)]+\)", " ", value)
    value = re.sub(r"\[([^\]]+)\]\([^)]+\)", r"\1", value)
    value = re.sub(r"[`*_#>|~\-\s]+", " ", value)
    value = re.sub(r"\s+", " ", value).strip()
    return value


def token_set(value: str) -> set[str]:
    value = normalize_text(value)
    tokens = set(re.findall(r"[a-z0-9][a-z0-9_\-.]{2,}|[\u4e00-\u9fff]{2,}", value))
    stop = {"http", "https", "compshare", "pages", "true", "false", "string", "integer", "action"}
    return {t for t in tokens if t not in stop}


if __name__ == "__main__":
    raise SystemExit(main())
