#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys
import tempfile

try:
    from .pipeline import (
        SEMANTIC_PLAN_STATS,
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
    # The external corpus gets its OWN date, defaulting to --valid-from so an
    # unattended caller that passes one date keeps today's behaviour exactly.
    #
    # One date for both corpora makes a docs-only rebuild rewrite the external
    # corpus for zero external change: valid_from lands in every row (via
    # kb_version AND the row's own field), so all 1189 external chunks change
    # bytes, the corpus digest changes, and the ~63 MB sidecar is rewritten under
    # a new digest-bearing filename with two Go pins following it. Not one vector
    # is recomputed -- chunk_repr is title/question_patterns/content and knows
    # nothing about the date -- so the entire churn is a rename plus a copy that
    # nothing asked for, and it lands in the reviewer's diff as "外部语料 digest:
    # 变了" on a release where no external source moved.
    #
    # This matters more once the build is scheduled: a corpus rewritten on every
    # tick is a corpus whose frozen partitions can never be asserted byte-frozen,
    # which is the assertion standing between auto-publish and the 1268 chunks
    # that have no upstream reviewer.
    parser.add_argument(
        "--external-valid-from",
        help="valid_from for the external corpus; defaults to --valid-from. "
             "Pass the PREVIOUS release's external date on a docs-only rebuild "
             "so the external corpus keeps its bytes.",
    )
    parser.add_argument("--vl-model", default="Qwen/Qwen3-VL-235B-A22B-Instruct")
    parser.add_argument("--vl-fallback-model", default="Qwen/Qwen3-vl-Plus")
    parser.add_argument("--semantic-model", default="qwen3.7-max")
    parser.add_argument("--asset-base-url", default="https://raw.githubusercontent.com/guli20001221/compshare-agent/main/deploy/kb/v2/assets")
    parser.add_argument("--skip-vl", action="store_true")
    parser.add_argument("--skip-semantic", action="store_true")
    # Serial by default. Measured 2026-08-15 over 20 images x 2 calls against
    # the same endpoint: at 8 workers 9/40 calls came back a non-answer on the
    # first attempt and 6/40 never answered across all four attempts, which
    # silently drops those images from the corpus (include_in_rag=false renders
    # as the empty string). At 1 worker it was 0/40 and 0/40, every call
    # accepted on its first attempt. The model is not the variable -- the same
    # 20 images score identically on Qwen3-VL-235B, Qwen3-vl-Plus, qwen3-vl-flash
    # and gpt-5.6-terra; the fan-out is, exactly as vl_payload_answered's
    # docstring already suspected. Text still varies run to run at either
    # setting and that is inherent to captioning with a model; losing images is
    # not. With identity-keyed reuse a routine update captions only the images
    # whose bytes are new, so serial costs minutes, not the ~2.6h a full
    # cache-less pass would take.
    parser.add_argument("--vl-workers", type=int, default=1)
    # Remote images are revalidated with a conditional GET before the reuse
    # decision, so this pass runs on every build. It fans out where the VL step
    # cannot: what made captioning unstable under concurrency was the model
    # returning a shaped non-answer, not HTTP. 1200 URLs at 8 in flight is
    # minutes, and a 304 costs no body at all.
    parser.add_argument("--remote-workers", type=int, default=8)
    # Keeping cached bytes for an unreachable origin stops a CDN blip from
    # failing a build, but for a platform image it means shipping a caption
    # nothing verified this run. That is a person's call, not a default.
    parser.add_argument("--allow-stale-remote", action="store_true")
    args = parser.parse_args(argv)
    # What this build was actually told to do, recorded by the process that did
    # it. The release gate reads it to see whether a safety flag was in play.
    #
    # It is a convenience for the reader, NOT the gate's evidence: a recorder can
    # only ever attest to itself, so the gate's real assertions about --skip-vl
    # and --allow-stale-remote are made against the ARTIFACTS those flags change
    # (the caption count in asset_report.json, the presence of asset_lock.json,
    # the degradations list). Nothing here carries a secret -- the API key
    # arrives through --env as a path, and never on the command line.
    effective_argv = list(argv) if argv is not None else list(sys.argv[1:])

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
    report: dict[str, object] = {"external_selection": {}, "build_argv": effective_argv}

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
            asset_notes, asset_failures, asset_degradations = {}, [], []
        else:
            assert client is not None
            asset_notes, asset_failures, asset_degradations = describe_assets(
                all_image_docs,
                client=client,
                model=args.vl_model,
                fallback_model=args.vl_fallback_model,
                assets_dir=out / "assets",
                raw_asset_base_url=args.asset_base_url,
                workers=args.vl_workers,
                remote_workers=args.remote_workers,
            )
        report["assets"] = {
            "described": len(asset_notes),
            "published": 0,
            "runtime_mode": "caption_only",
            "failures": asset_failures,
            # Images whose origin could not be revalidated this build, so the
            # bytes came from cache. Not a failure -- the caption is usable --
            # but it is the difference between "the image is still this" and
            # "the image was this the last time anyone could ask", and that
            # difference has to be readable rather than inferred from a log line.
            "degradations": asset_degradations,
        }
        # Source-local images are temporary VL inputs, not release artifacts.
        for generated_asset in (out / "assets").glob("*"):
            if generated_asset.is_file():
                generated_asset.unlink()
        if (out / "assets").exists():
            (out / "assets").rmdir()
        (out / "asset_report.json").write_text(
            json.dumps(report["assets"], ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )

        semantic_client = None if args.skip_semantic else client
        external_valid_from = args.external_valid_from or args.valid_from
        internal_version = f"kb.platform.v2.{args.valid_from}"
        external_version = f"kb.external.v2.{external_valid_from}"
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
            valid_from=external_valid_from,
            asset_notes=asset_notes,
            semantic_client=semantic_client,
            semantic_model=args.semantic_model,
        )
        legacy = load_legacy_external(args.legacy_external, kb_version=external_version)
        external_rows, external_merge_skipped = merge_external(legacy, rebuilt_external)
        report["internal"] = {**internal_stats, "documents": len(internal_docs), "valid_from": args.valid_from}
        report["external"] = {
            **external_stats,
            "documents": len(external_docs),
            "valid_from": external_valid_from,
            "legacy_chunks_retained": len(legacy),
            "merged_chunk_count": len(external_rows),
            "merge_duplicates_skipped": external_merge_skipped,
        }
        # How every long document got its boundaries. Chunk counts look identical
        # whether a semantic plan or a rune counter drew them, so without this a
        # planner outage is invisible in the report and in the corpus.
        report["semantic_split"] = dict(sorted(SEMANTIC_PLAN_STATS.items()))

        internal_errors = validate_chunks(internal_rows, expected_version=internal_version)
        external_errors = validate_chunks(external_rows, expected_version=external_version)
        blocking_asset_failures = [item for item in asset_failures if item.get("severity", "error") == "error"]
        if blocking_asset_failures and not args.skip_vl:
            raise ValueError(f"asset processing failed for {len(blocking_asset_failures)} required references; see build report")
        stale_platform_assets = [item for item in asset_degradations if item.get("severity") == "error"]
        if stale_platform_assets and not args.allow_stale_remote:
            raise ValueError(
                f"{len(stale_platform_assets)} platform image(s) could not be revalidated and were taken "
                "from cache; see asset_report.json degradations. Re-run when the origin is reachable, or "
                "pass --allow-stale-remote to publish captions nothing verified this build."
            )
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
