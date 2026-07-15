#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
from pathlib import Path
import shutil

from .pipeline import normalized_file_digest


def read_meta(path: Path) -> dict[str, object]:
    first = path.read_text(encoding="utf-8").splitlines()[0]
    payload = json.loads(first)
    if set(payload) != {"_meta"}:
        raise ValueError(f"{path}: missing embedding metadata")
    return payload["_meta"]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Promote a validated RAG V2 release into runtime paths.")
    parser.add_argument("--release-dir", type=Path, required=True)
    parser.add_argument("--deploy-dir", type=Path, required=True)
    parser.add_argument("--embed-model", default="qwen3-embedding-8b")
    args = parser.parse_args(argv)

    release = args.release_dir
    deploy = args.deploy_dir
    manifest_path = release / "release_manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    corpora = [
        ("internal_corpus", release / "stage2b_v2.jsonl", deploy / "stage2b_w0.jsonl"),
        ("external_corpus", release / "external_v2.jsonl", deploy / "external_w0.jsonl"),
    ]
    embedding_artifacts: dict[str, dict[str, object]] = {}
    selected_sidecars: set[Path] = set()
    for artifact_name, candidate, runtime_path in corpora:
        digest = normalized_file_digest(candidate)
        expected = manifest["artifacts"][artifact_name]["sha256"]
        if digest != expected:
            raise ValueError(f"{artifact_name}: digest mismatch {digest} != {expected}")
        sidecar = release / f"embeddings_{digest}_{args.embed_model}.jsonl"
        meta = read_meta(sidecar)
        row_count = sum(1 for line in candidate.read_text(encoding="utf-8").splitlines() if line.strip())
        if meta.get("corpus_digest") != digest or meta.get("embed_model") != args.embed_model:
            raise ValueError(f"{sidecar}: metadata mismatch")
        if meta.get("dim") != 4096 or meta.get("rows") != row_count:
            raise ValueError(f"{sidecar}: dimension/row mismatch")
        deploy.mkdir(parents=True, exist_ok=True)
        shutil.copy2(candidate, runtime_path)
        deployed_sidecar = deploy / sidecar.name
        shutil.copy2(sidecar, deployed_sidecar)
        selected_sidecars.add(deployed_sidecar.resolve())
        embedding_artifacts[artifact_name.replace("corpus", "embeddings")] = {
            "path": deployed_sidecar.as_posix(),
            "sha256": normalized_file_digest(deployed_sidecar),
            "model": args.embed_model,
            "dim": 4096,
            "rows": row_count,
        }

    for old in deploy.glob(f"embeddings_*_{args.embed_model}.jsonl"):
        if old.resolve() not in selected_sidecars:
            old.unlink()
    manifest["artifacts"].update(embedding_artifacts)
    manifest["promotion"] = {
        "internal_runtime_path": (deploy / "stage2b_w0.jsonl").as_posix(),
        "external_runtime_path": (deploy / "external_w0.jsonl").as_posix(),
    }
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    for name, item in manifest["artifacts"].items():
        print(f"{name}={item['sha256']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
