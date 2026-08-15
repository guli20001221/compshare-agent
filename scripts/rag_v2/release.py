#!/usr/bin/env python3
"""One command from a docs snapshot to a reviewable, importable release.

The chain was seven manual steps with two silent failure modes. Step six was
hand-editing four SHA256 literals in a Go file, where a wrong paste fails a Go
test with a digest mismatch that names neither the artifact nor the reason; and
nothing anywhere produced a diff, so the manual publish gate was a button with
nothing to read behind it.

What this does NOT do is make the database incremental. A release is an
immutable candidate plus an atomic pointer swap, and that is the correct shape:
incrementality belongs to the expensive build steps (captions, semantic plans,
embeddings), all of which are content-addressed and skip unchanged work on their
own. This only sequences them, pins the result, and writes down what changed.

Publication stays out of here on purpose. It is a separate manual GitLab job so
that a person reads release_diff.md between the two.
"""
from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

try:
    from .pipeline import normalized_file_digest
    from . import release_diff
except ImportError:  # pragma: no cover - direct script execution
    from pipeline import normalized_file_digest  # type: ignore
    import release_diff  # type: ignore


# Which Go constant pins which promoted artifact. EmbeddingDigestExpected is
# deliberately absent: it pins a dead text-embedding-3-large sidecar that no
# longer exists and is documented as frozen at its old value.
PINNED_ARTIFACTS = {
    "CorpusDigestExpected": "stage2b_w0.jsonl",
    "ExternalCorpusDigestExpected": "external_w0.jsonl",
    "EmbeddingDigestExpectedQwen3": "embeddings_{CorpusDigestExpected}_qwen3-embedding-8b.jsonl",
    "ExternalEmbeddingDigestExpectedQwen3": "embeddings_{ExternalCorpusDigestExpected}_qwen3-embedding-8b.jsonl",
}


def compute_pins(deploy_dir: Path) -> dict[str, str]:
    """Digests the Go constants must carry for the promoted artifacts."""
    pins: dict[str, str] = {}
    for name in ("CorpusDigestExpected", "ExternalCorpusDigestExpected"):
        path = deploy_dir / PINNED_ARTIFACTS[name]
        if not path.exists():
            raise FileNotFoundError(f"{path} is missing; promote before pinning")
        pins[name] = normalized_file_digest(path)
    for name in ("EmbeddingDigestExpectedQwen3", "ExternalEmbeddingDigestExpectedQwen3"):
        path = deploy_dir / PINNED_ARTIFACTS[name].format(**pins)
        if not path.exists():
            raise FileNotFoundError(f"{path} is missing; refresh embeddings before pinning")
        pins[name] = normalized_file_digest(path)
    return pins


def rewrite_pins(source: str, pins: dict[str, str]) -> tuple[str, dict[str, tuple[str, str]]]:
    """Replace each `const <name> = "<hex>"` value, leaving every comment intact.

    Returns the new source and the constants that actually moved. A name the
    file does not declare is an error rather than a silent no-op: that is how a
    renamed constant would otherwise leave a stale digest in place.
    """
    changed: dict[str, tuple[str, str]] = {}
    for name, digest in pins.items():
        pattern = re.compile(rf'(const\s+{re.escape(name)}\s*=\s*")([0-9a-f]{{64}})(")')
        match = pattern.search(source)
        if match is None:
            raise ValueError(f"{name} is not declared as a 64-hex const in corpus_digest.go")
        if match.group(2) != digest:
            changed[name] = (match.group(2), digest)
        source = pattern.sub(lambda m: m.group(1) + digest + m.group(3), source, count=1)
    return source, changed


def run(command: list[str], *, cwd: Path) -> None:
    print(f"\n$ {' '.join(command)}", flush=True)
    result = subprocess.run(command, cwd=cwd)
    if result.returncode != 0:
        raise SystemExit(f"stage failed: {' '.join(command)}")


def snapshot_released(repo: Path, destination: Path) -> dict[str, Path]:
    """Copy the currently RELEASED artifacts aside before the build overwrites them.

    The baseline has to come from the working tree rather than from git: the
    promoted files under deploy/kb ARE what production imported, while a git
    revision only tells you what was committed. If the tree is dirty the diff
    should say so, which it does by comparing against these bytes.
    """
    destination.mkdir(parents=True, exist_ok=True)
    kept: dict[str, Path] = {}
    for label, relative in (
        ("internal", "deploy/kb/stage2b_w0.jsonl"),
        ("external", "deploy/kb/external_w0.jsonl"),
        ("lock", "deploy/kb/v2/asset_lock.json"),
    ):
        source = repo / relative
        if source.exists():
            target = destination / Path(relative).name
            shutil.copy2(source, target)
            kept[label] = target
    return kept


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Build, embed, promote, pin and diff a knowledge release candidate.")
    parser.add_argument("--repo", type=Path, default=Path.cwd())
    parser.add_argument("--release-dir", type=Path, default=Path("deploy/kb/v2"))
    parser.add_argument("--deploy-dir", type=Path, default=Path("deploy/kb"))
    parser.add_argument("--env", type=Path, required=True,
                        help="env file carrying MODELVERSE_API_KEY")
    parser.add_argument("--embed-model", default="qwen3-embedding-8b")
    parser.add_argument("--skip-build", action="store_true",
                        help="reuse the corpora already in --release-dir")
    # The release this corpus is BUILT ON, recorded here so the import job can
    # pass it as the candidate's parent. It cannot be derived at import time:
    # reading the active release then records whatever is live when the job
    # runs, and the two differ exactly when it matters -- build on R0, let R1
    # publish while the candidate is in review, and an import-time read files
    # the candidate as R1's child, so publishing it silently overwrites content
    # it never saw. Get the value from the serving process:
    #   kubectl -n prj-ucompshare-prod exec compshare-kb-0 -- \
    #     wget -qO- http://127.0.0.1:8088/healthz
    parser.add_argument("--parent-release-id",
                        help="active release id this corpus is built on (see /healthz)")
    parser.add_argument("--build-arg", action="append", default=[],
                        help="passed through to scripts.rag_v2.build; repeat per argument")
    args, _unknown = parser.parse_known_args(argv)

    repo = args.repo.resolve()
    release_dir = (repo / args.release_dir).resolve()
    deploy_dir = (repo / args.deploy_dir).resolve()
    python = sys.executable

    with tempfile.TemporaryDirectory(prefix="kb-release-baseline-") as raw:
        baseline = snapshot_released(repo, Path(raw))
        print(f"baseline snapshot: {sorted(baseline)}")

        # Written before the build, from the artifacts the build is about to
        # overwrite: this file is the candidate's claim about what it was based
        # on, and the import job passes it through as --parent-release-id.
        base_record = {
            "parent_release_id": args.parent_release_id,
            "baseline_digests": {
                label: normalized_file_digest(path)
                for label, path in sorted(baseline.items()) if label != "lock"
            },
        }
        (release_dir).mkdir(parents=True, exist_ok=True)
        (release_dir / "release_base.json").write_text(
            json.dumps(base_record, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8")
        if args.parent_release_id:
            print(f"built on release {args.parent_release_id}")
        else:
            print(
                "WARNING: no --parent-release-id. The import job will refuse this candidate\n"
                "         unless it is the first publication into an empty knowledge base.",
                flush=True,
            )

        if not args.skip_build:
            run([python, "-m", "scripts.rag_v2.build", *args.build_arg,
                 "--out-dir", str(release_dir), "--env", str(args.env)], cwd=repo)

        # Embeddings refresh reuses a vector whenever the chunk's embedding
        # input is unchanged, so a routine update embeds only what moved. It is
        # per corpus; there is no multi-source mode.
        for corpus, released in (("stage2b", "stage2b_w0.jsonl"), ("external", "external_w0.jsonl")):
            old_corpus = deploy_dir / released
            candidate = release_dir / (f"{corpus}_v2.jsonl" if corpus == "stage2b" else "external_v2.jsonl")
            old_sidecar = deploy_dir / (
                f"embeddings_{normalized_file_digest(old_corpus)}_{args.embed_model}.jsonl")
            if not old_sidecar.exists():
                raise SystemExit(f"no released sidecar for {old_corpus}; expected {old_sidecar}")
            run([python, "-m", "scripts.rag_v2.refresh_embeddings",
                 "--old-corpus", str(old_corpus), "--new-corpus", str(candidate),
                 "--old-sidecar", str(old_sidecar), "--out-dir", str(release_dir),
                 "--env", str(args.env), "--embed-model", args.embed_model], cwd=repo)

        # The diff is produced BEFORE promotion, while the released bytes and
        # the candidate bytes both still exist under their own names.
        report_path = release_dir / "release_diff.md"
        release_diff.main([
            "--old-internal", str(baseline.get("internal", deploy_dir / "stage2b_w0.jsonl")),
            "--new-internal", str(release_dir / "stage2b_v2.jsonl"),
            "--old-external", str(baseline.get("external", deploy_dir / "external_w0.jsonl")),
            "--new-external", str(release_dir / "external_v2.jsonl"),
            *(["--old-lock", str(baseline["lock"])] if "lock" in baseline else []),
            "--new-lock", str(release_dir / "asset_lock.json"),
            "--asset-report", str(release_dir / "asset_report.json"),
            "--out-md", str(report_path),
            "--out-json", str(release_dir / "release_diff.json"),
        ])

        run([python, "-m", "scripts.rag_v2.promote",
             "--release-dir", str(release_dir), "--deploy-dir", str(deploy_dir),
             "--embed-model", args.embed_model], cwd=repo)

    pins = compute_pins(deploy_dir)
    digest_file = repo / "internal/knowledge/corpus_digest.go"
    source = digest_file.read_text(encoding="utf-8")
    rewritten, changed = rewrite_pins(source, pins)
    if changed:
        digest_file.write_text(rewritten, encoding="utf-8")
        print("\nrepinned internal/knowledge/corpus_digest.go:")
        for name, (before, after) in sorted(changed.items()):
            print(f"  {name}: {before[:16]} -> {after[:16]}")
    else:
        print("\ncorpus_digest.go pins already match the promoted artifacts")

    # The bundle check the released corpus actually has: the Go loader reads the
    # promoted files and refuses a digest mismatch. It only means anything after
    # the repin above, which is why it is the last stage rather than the first.
    run(["go", "test", "./internal/knowledge", "-count=1"], cwd=repo)

    print(f"\ncandidate ready. Review {report_path}, commit, then run")
    print("import-knowledge-release and (after reading the diff) publish-knowledge-release.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
