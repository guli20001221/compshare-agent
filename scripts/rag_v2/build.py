#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
from pathlib import Path
import tempfile

try:
    from .pipeline import (
        ModelVerseClient,
        build_chunks,
        collect_external_docs,
        collect_faq_docs,
        collect_internal_docs,
        describe_assets,
        load_env,
        load_legacy_external,
        merge_external,
        normalized_file_digest,
        safe_extract_zip,
        sha256_file,
        tree_lock,
        validate_chunks,
        write_jsonl,
        write_release_manifest,
    )
except ImportError:  # pragma: no cover
    from pipeline import *  # type: ignore  # noqa: F403


FAQ_IDS = ("faq-model-package", "faq-comfyui-base", "faq-usage")
EXTERNAL_PACKAGES = ("comfyui", "digital-human", "voice-audio")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Build RAG V2 internal and external corpora from pinned public sources.")
    parser.add_argument("--internal-docs", type=Path, required=True)
    parser.add_argument("--internal-revision", required=True)
    parser.add_argument("--faq-zip", type=Path, action="append", required=True)
    parser.add_argument("--external-zip", type=Path, action="append", required=True)
    parser.add_argument("--legacy-external", type=Path, required=True)
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--env", type=Path)
    parser.add_argument("--valid-from", required=True)
    parser.add_argument("--vl-model", default="Qwen/Qwen3-VL-235B-A22B-Instruct")
    parser.add_argument("--vl-fallback-model", default="Qwen/Qwen3-vl-Plus")
    parser.add_argument("--semantic-model", default="qwen3.7-max")
    parser.add_argument("--asset-base-url", default="https://raw.githubusercontent.com/guli20001221/compshare-agent/main/deploy/kb/v2/assets")
    parser.add_argument("--skip-vl", action="store_true")
    parser.add_argument("--skip-semantic", action="store_true")
    parser.add_argument("--vl-workers", type=int, default=8)
    args = parser.parse_args(argv)

    if len(args.faq_zip) != len(FAQ_IDS):
        parser.error(f"exactly {len(FAQ_IDS)} --faq-zip arguments are required in documented order")
    if len(args.external_zip) != len(EXTERNAL_PACKAGES):
        parser.error(f"exactly {len(EXTERNAL_PACKAGES)} --external-zip arguments are required in documented order")

    out = args.out_dir
    out.mkdir(parents=True, exist_ok=True)
    cache_dir = out / ".cache"
    env = load_env(args.env)
    client = None
    if not (args.skip_vl and args.skip_semantic):
        client = ModelVerseClient(
            base_url=env.get("MODELVERSE_BASE_URL", "https://api.modelverse.cn/v1"),
            api_key=env.get("MODELVERSE_API_KEY", ""),
            cache_dir=cache_dir / "modelverse",
        )

    source_locks = [{
        "id": "gitlab-compshare-docs",
        "kind": "git",
        "revision": args.internal_revision,
        **tree_lock(args.internal_docs),
    }, {
        "id": "legacy-external-unrebuildable",
        "kind": "chunk_snapshot",
        "filename": args.legacy_external.name,
        "sha256": sha256_file(args.legacy_external),
    }]
    report: dict[str, object] = {"external_selection": {}}

    with tempfile.TemporaryDirectory(prefix="compshare-rag-v2-") as tmp_raw:
        tmp = Path(tmp_raw)
        faq_docs = []
        for source_id, zip_path in zip(FAQ_IDS, args.faq_zip):
            destination = tmp / source_id
            safe_extract_zip(zip_path, destination)
            faq_docs.extend(collect_faq_docs(destination, source_id))
            source_locks.append({"id": source_id, "kind": "zip", "filename": zip_path.name, "sha256": sha256_file(zip_path)})

        external_docs = []
        external_selection: dict[str, object] = {}
        for package, zip_path in zip(EXTERNAL_PACKAGES, args.external_zip):
            destination = tmp / f"external-{package}"
            safe_extract_zip(zip_path, destination)
            docs, selection = collect_external_docs(destination, package)
            external_docs.extend(docs)
            external_selection[package] = selection
            source_locks.append({"id": f"external-{package}", "kind": "zip", "filename": zip_path.name, "sha256": sha256_file(zip_path)})
        report["external_selection"] = external_selection

        internal_docs = collect_internal_docs(args.internal_docs) + faq_docs
        all_image_docs = [*internal_docs, *external_docs]
        if args.skip_vl:
            asset_notes, asset_failures = {}, []
        else:
            assert client is not None
            asset_notes, asset_failures = describe_assets(
                all_image_docs,
                client=client,
                model=args.vl_model,
                fallback_model=args.vl_fallback_model,
                assets_dir=out / "assets",
                raw_asset_base_url=args.asset_base_url,
                workers=args.vl_workers,
            )
        report["assets"] = {"described": len(asset_notes), "failures": asset_failures}
        referenced_local_assets = {
            Path(note.repo_path).name for note in asset_notes.values() if note.repo_path
        }
        for generated_asset in (out / "assets").glob("*"):
            if generated_asset.is_file() and generated_asset.name not in referenced_local_assets:
                generated_asset.unlink()
        (out / "asset_report.json").write_text(
            json.dumps(report["assets"], ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )

        semantic_client = None if args.skip_semantic else client
        internal_version = f"kb.platform.v2.{args.valid_from}"
        external_version = f"kb.external.v2.{args.valid_from}"
        internal_rows, internal_stats = build_chunks(
            internal_docs,
            kb_version=internal_version,
            valid_from=args.valid_from,
            asset_notes=asset_notes,
            semantic_client=semantic_client,
            semantic_model=args.semantic_model,
        )
        rebuilt_external, external_stats = build_chunks(
            external_docs,
            kb_version=external_version,
            valid_from=args.valid_from,
            asset_notes=asset_notes,
            semantic_client=semantic_client,
            semantic_model=args.semantic_model,
        )
        legacy = load_legacy_external(args.legacy_external, kb_version=external_version)
        external_rows, external_merge_skipped = merge_external(legacy, rebuilt_external)
        report["internal"] = {**internal_stats, "documents": len(internal_docs)}
        report["external"] = {
            **external_stats,
            "documents": len(external_docs),
            "legacy_chunks_retained": len(legacy),
            "merged_chunk_count": len(external_rows),
            "merge_duplicates_skipped": external_merge_skipped,
        }

        internal_errors = validate_chunks(internal_rows, expected_version=internal_version)
        external_errors = validate_chunks(external_rows, expected_version=external_version)
        blocking_asset_failures = [item for item in asset_failures if item.get("severity", "error") == "error"]
        if blocking_asset_failures and not args.skip_vl:
            raise ValueError(f"asset processing failed for {len(blocking_asset_failures)} required references; see build report")
        if internal_errors or external_errors:
            raise ValueError("chunk validation failed:\n" + "\n".join([*internal_errors, *external_errors][:100]))

        internal_path = out / "stage2b_v2.jsonl"
        external_path = out / "external_v2.jsonl"
        write_jsonl(internal_path, internal_rows)
        write_jsonl(external_path, external_rows)
        write_release_manifest(
            out / "release_manifest.json",
            internal_corpus=internal_path,
            external_corpus=external_path,
            source_locks=source_locks,
            report=report,
            models={
                "vl": args.vl_model,
                "vl_fallback": args.vl_fallback_model,
                "semantic_split": args.semantic_model,
                "embedding": "qwen3-embedding-8b",
                "reranker": "qwen3-reranker-8b",
                "judge": "doubao-seed-2-1-pro-260628",
                "judge_fallback": "doubao-seed-2-1-turbo-260628",
            },
        )

    print(f"internal_chunks={len(internal_rows)} digest={normalized_file_digest(internal_path)}")
    print(f"external_chunks={len(external_rows)} digest={normalized_file_digest(external_path)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
