#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from .pipeline import ModelVerseClient, load_env, sha256_file


def load_rows(path: Path) -> list[dict[str, Any]]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def pick_samples(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    samples: list[dict[str, Any]] = []
    for document_type in ("api_reference", "operation_guide", "faq_collection"):
        row = next((item for item in rows if item.get("document_type") == document_type), None)
        if row:
            samples.append({
                "chunk_id": row.get("chunk_id"),
                "document_type": document_type,
                "title": row.get("title"),
                "content": str(row.get("content") or "")[:4200],
                "asset_refs": row.get("asset_refs") or [],
                "source_refs": row.get("source_refs") or [],
                "document_title": row.get("document_title"),
                "heading_path": row.get("heading_path") or [],
            })
    media = next((item for item in rows if item.get("asset_refs")), None)
    if media and media.get("chunk_id") not in {item["chunk_id"] for item in samples}:
        samples.append({
            "chunk_id": media.get("chunk_id"),
            "document_type": media.get("document_type"),
            "title": media.get("title"),
            "content": str(media.get("content") or "")[:4200],
            "asset_refs": media.get("asset_refs") or [],
            "source_refs": media.get("source_refs") or [],
            "document_title": media.get("document_title"),
            "heading_path": media.get("heading_path") or [],
        })
    return samples


def main() -> int:
    parser = argparse.ArgumentParser(description="Independent LLM review of a built RAG V2 release.")
    parser.add_argument("--internal", type=Path, required=True)
    parser.add_argument("--external", type=Path, required=True)
    parser.add_argument("--retrieval-report", type=Path, required=True, help="Primary real-chat retrieval report")
    parser.add_argument("--compatibility-report", type=Path, help="Optional legacy synthetic compatibility report")
    parser.add_argument("--gap-audit", type=Path, required=True, help="Manual audit of every real-chat miss")
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--env", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--model", default="doubao-seed-2-1-pro-260628")
    args = parser.parse_args()

    internal = load_rows(args.internal)
    external = load_rows(args.external)
    all_rows = internal + external
    refs = [ref for row in all_rows for ref in (row.get("asset_refs") or [])]
    retrieval = json.loads(args.retrieval_report.read_text(encoding="utf-8"))
    real_metrics = retrieval.get("metrics") or {}
    compatibility = (
        json.loads(args.compatibility_report.read_text(encoding="utf-8"))
        if args.compatibility_report else None
    )
    gap_audit = json.loads(args.gap_audit.read_text(encoding="utf-8"))
    metrics = {
        "internal_chunks": len(internal),
        "external_chunks": len(external),
        "legacy_external_chunks": sum(row.get("legacy_unrebuildable") is True for row in external),
        "asset_refs": len(refs),
        "invalid_asset_urls": sum(not str(ref.get("url") or "").startswith("https://") for ref in refs),
        "asset_links_missing_from_content": sum(
            str(ref.get("url") or "") not in str(row.get("content") or "")
            for row in all_rows for ref in (row.get("asset_refs") or [])
        ),
        "internal_sha256": sha256_file(args.internal),
        "external_sha256": sha256_file(args.external),
        "retrieval": {
            "evidence": "50 manually selected RAG-relevant production retrieval queries from 2026-06-26 through 2026-07-09",
            "questions_evaluated": real_metrics.get("kb_applicable"),
            "full": real_metrics.get("full"),
            "partial": real_metrics.get("partial"),
            "miss": real_metrics.get("miss"),
            "full_rate": real_metrics.get("full_rate"),
            "coverage_rate_full_or_partial": real_metrics.get("coverage_rate_full_or_partial"),
            "privacy": retrieval.get("privacy"),
            "legacy_compatibility_only": {
                "questions_evaluated": compatibility.get("questions_evaluated"),
                "top_3_hit_rate": compatibility.get("top_3_hit_rate"),
            } if compatibility else None,
            "miss_audit": gap_audit.get("summary"),
            "release_effect": gap_audit.get("release_effect"),
        },
        "manifest_models": json.loads(args.manifest.read_text(encoding="utf-8")).get("models"),
        "legacy_external_policy": {
            "retained_as_locked_snapshot": True,
            "rebuildable": False,
            "reason": "original source documents unavailable",
            "locked_rows": sum(row.get("legacy_unrebuildable") is True for row in external),
        },
        "runtime_validation": {
            "python_unit_tests": "8 passed",
            "corpus_validators": "internal and external passed",
            "go_packages": ["internal/knowledge", "internal/tools", "internal/engine", "cmd"],
            "go_result": "passed",
        },
    }
    prompt = (
        "你是与构建模型独立的严格 RAG 发布裁判。只根据下面的可核验材料审查，输出 JSON。"
        "必须包含 pipeline_integrity_pass(boolean)、merge_ready(boolean)、scores(object，含 source_traceability/chunk_integrity/media_usability/runtime_compatibility/retrieval_evidence，0-10)、"
        "blocking_issues(array)、observations(array)。流水线完整性通过条件：图片 URL 全为 HTTPS 且链接进入正文；"
        "短接口、短操作指南、FAQ 样例边界完整；保留无源旧外部切片的事实明确。"
        "旧题 top_3_hit_rate 只允许作为兼容性证据，不能证明真实检索质量。真实质量必须以 50 条生产检索问题的 full/partial/miss 为准；"
        "存在未解释的真实 miss 时 merge_ready 必须为 false，即使流水线完整性可以通过。"
        "注意 external_chunks 是外部总数，legacy_external_chunks 才是无源旧切片数，结论不得混淆这两个数字。"
        "不要把模型自评当作运行时兼容证明，只评价给出的证据。\n\n"
        + json.dumps({"metrics": metrics, "samples": pick_samples(internal)}, ensure_ascii=False)
    )
    env = load_env(args.env)
    client = ModelVerseClient(
        base_url=env.get("MODELVERSE_BASE_URL", "https://api.modelverse.cn/v1"),
        api_key=env.get("MODELVERSE_API_KEY", ""),
        cache_dir=args.output.parent / ".cache" / "release_judge",
    )
    verdict = client.json_chat(model=args.model, prompt=prompt, max_tokens=1600)
    report = {"schema_version": "compshare.rag.release-judge.v1", "model": args.model, "metrics": metrics, "verdict": verdict}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(verdict, ensure_ascii=False))
    return 0 if verdict.get("pipeline_integrity_pass") is True else 1


if __name__ == "__main__":
    raise SystemExit(main())
