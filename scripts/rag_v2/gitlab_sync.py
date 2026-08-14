#!/usr/bin/env python3
"""Build a V2 candidate corpus from a reviewed GitLab documentation delta.

This module intentionally does *not* promote files into deploy/kb. The runtime
pins corpus and embedding digests in Go, so a scheduled source update must go
through a candidate artifact, sidecar refresh, CI validation, and the normal
release/promotion change. Keeping that boundary makes a GitLab force-push or a
bad document edit recoverable instead of mutating a live RAG corpus in place.
"""
from __future__ import annotations

import argparse
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
import json
from pathlib import Path
import subprocess
import sys
from typing import Any, Iterable
from urllib.parse import unquote

try:
    from .pipeline import (
        IMAGE_RE,
        ModelVerseClient,
        _is_decorative_asset,
        build_chunks,
        clean_public_text,
        collect_internal_docs,
        load_env,
        normalize_image_markup,
        normalized_file_digest,
        validate_chunks,
        write_jsonl,
    )
except ImportError:  # pragma: no cover - direct script invocation
    from pipeline import (  # type: ignore
        IMAGE_RE,
        ModelVerseClient,
        _is_decorative_asset,
        build_chunks,
        clean_public_text,
        collect_internal_docs,
        load_env,
        normalize_image_markup,
        normalized_file_digest,
        validate_chunks,
        write_jsonl,
    )


SOURCE_ID = "gitlab-compshare-docs"
ELIGIBLE_PREFIXES = ("pages/", "public/action_md/")


@dataclass(frozen=True)
class SourceChange:
    action: str
    old_path: str | None = None
    new_path: str | None = None


@dataclass(frozen=True)
class CandidateResult:
    status: str
    report: dict[str, Any]
    candidate_path: Path | None = None


def normalize_repo_path(value: str) -> str:
    path = value.replace("\\", "/").strip().lstrip("./")
    if not path or path.startswith("/") or any(part in {"", ".", ".."} for part in path.split("/")):
        raise ValueError(f"unsafe repository path {value!r}")
    return path


def is_eligible_doc_path(value: str | None) -> bool:
    if not value:
        return False
    path = normalize_repo_path(value)
    return path.lower().endswith(".md") and path.startswith(ELIGIBLE_PREFIXES)


def is_relevant_source_path(value: str | None) -> bool:
    return bool(value and normalize_repo_path(value).startswith(ELIGIBLE_PREFIXES))


def parse_name_status_z(payload: bytes) -> list[SourceChange]:
    """Parse ``git diff --name-status -z --find-renames`` deterministically."""
    tokens = [item.decode("utf-8", errors="strict") for item in payload.split(b"\0") if item]
    changes: list[SourceChange] = []
    cursor = 0
    while cursor < len(tokens):
        status = tokens[cursor]
        cursor += 1
        action = status[:1]
        if action in {"R", "C"}:
            if cursor + 1 >= len(tokens):
                raise ValueError(f"malformed git name-status payload after {status!r}")
            old_path = normalize_repo_path(tokens[cursor])
            new_path = normalize_repo_path(tokens[cursor + 1])
            cursor += 2
            if action == "C":
                # A copy is semantically an add to the RAG source set. We
                # preserve the source action in the report for reviewability.
                changes.append(SourceChange(action="A", new_path=new_path))
            else:
                changes.append(SourceChange(action="R", old_path=old_path, new_path=new_path))
            continue
        if action not in {"A", "M", "D"}:
            raise ValueError(f"unsupported git change status {status!r}")
        if cursor >= len(tokens):
            raise ValueError(f"malformed git name-status payload after {status!r}")
        path = normalize_repo_path(tokens[cursor])
        cursor += 1
        if action == "D":
            changes.append(SourceChange(action=action, old_path=path))
        else:
            changes.append(SourceChange(action=action, new_path=path, old_path=path if action == "M" else None))
    return changes


def git_changes(repo: Path, base_revision: str, head_revision: str) -> list[SourceChange]:
    result = subprocess.run(
        ["git", "-C", str(repo), "diff", "--name-status", "-z", "--find-renames", base_revision, head_revision],
        capture_output=True,
        check=False,
    )
    if result.returncode != 0:
        stderr = result.stderr.decode("utf-8", errors="replace").strip()
        raise RuntimeError(f"git diff failed: {stderr or result.returncode}")
    return parse_name_status_z(result.stdout)


def git_revision(repo: Path, revision: str = "HEAD") -> str:
    result = subprocess.run(
        ["git", "-C", str(repo), "rev-parse", "--verify", revision],
        capture_output=True,
        check=False,
    )
    if result.returncode != 0:
        stderr = result.stderr.decode("utf-8", errors="replace").strip()
        raise RuntimeError(f"git rev-parse {revision!r} failed: {stderr or result.returncode}")
    return result.stdout.decode("utf-8", errors="strict").strip()


def source_ref(path: str, *, source_id: str = SOURCE_ID) -> str:
    return f"{source_id}:{normalize_repo_path(path)}"


def changed_source_paths(changes: Iterable[SourceChange]) -> tuple[set[str], set[str]]:
    """Return (paths to rebuild, source paths whose old rows must be removed)."""
    rebuild: set[str] = set()
    replace: set[str] = set()
    for change in changes:
        if change.action == "A" and is_eligible_doc_path(change.new_path):
            rebuild.add(normalize_repo_path(change.new_path or ""))
        elif change.action == "M" and is_eligible_doc_path(change.new_path):
            path = normalize_repo_path(change.new_path or "")
            rebuild.add(path)
            replace.add(path)
        elif change.action == "D" and is_eligible_doc_path(change.old_path):
            replace.add(normalize_repo_path(change.old_path or ""))
        elif change.action == "R":
            if is_eligible_doc_path(change.old_path):
                replace.add(normalize_repo_path(change.old_path or ""))
            if is_eligible_doc_path(change.new_path):
                path = normalize_repo_path(change.new_path or "")
                rebuild.add(path)
                # A source tree can contain a pre-existing row at the target
                # only after an unusual history rewrite. Remove it before
                # inserting the rebuilt document to keep the merge idempotent.
                replace.add(path)
    return rebuild, replace


def changed_asset_issues(repo: Path, changes: Iterable[SourceChange]) -> list[dict[str, str]]:
    """Fail closed for deltas the text-only incremental path cannot verify."""
    issues: list[dict[str, str]] = []
    seen: set[tuple[str, str]] = set()
    for change in changes:
        paths = [path for path in (change.old_path, change.new_path) if path]
        for raw_path in paths:
            path = normalize_repo_path(raw_path)
            if not is_relevant_source_path(path):
                continue
            if not is_eligible_doc_path(path):
                key = (path, "non_markdown_source_change")
                if key not in seen:
                    seen.add(key)
                    issues.append({"path": path, "reason": key[1]})
                continue
            # A deleted Markdown file carries no current body. Its old chunks
            # can be removed safely; there is no image evidence to regenerate.
            if change.action == "D" or (change.action == "R" and path == change.old_path):
                continue
            local = repo.joinpath(*path.split("/"))
            if not local.is_file():
                key = (path, "changed_markdown_missing_at_head")
                if key not in seen:
                    seen.add(key)
                    issues.append({"path": path, "reason": key[1]})
                continue
            normalized = normalize_image_markup(local.read_text(encoding="utf-8", errors="replace"))
            for alt, ref in IMAGE_RE.findall(normalized):
                decoded = unquote(ref).replace("\\", "/")
                if _is_decorative_asset(alt, decoded):
                    continue
                key = (path, "non_decorative_image_requires_vl_release")
                if key not in seen:
                    seen.add(key)
                    issues.append({"path": path, "reason": key[1], "ref": decoded})
    return issues


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def merge_incremental_rows(
    base_rows: list[dict[str, Any]],
    changed_rows: list[dict[str, Any]],
    *,
    replace_paths: set[str],
    kb_version: str,
    source_id: str = SOURCE_ID,
) -> tuple[list[dict[str, Any]], dict[str, int]]:
    replace_refs = {source_ref(path, source_id=source_id) for path in replace_paths}
    retained: list[dict[str, Any]] = []
    removed = 0
    for row in base_rows:
        refs = {str(value) for value in (row.get("source_refs") or [])}
        if refs & replace_refs:
            removed += 1
            continue
        retained.append(dict(row))

    merged: list[dict[str, Any]] = []
    seen_ids: set[str] = set()
    seen_content: set[str] = set()
    duplicate_content_skipped = 0
    for row in [*retained, *changed_rows]:
        candidate = dict(row)
        candidate["kb_version"] = kb_version
        chunk_id = str(candidate.get("chunk_id") or "")
        if not chunk_id:
            raise ValueError("candidate row has empty chunk_id")
        if chunk_id in seen_ids:
            raise ValueError(f"duplicate chunk_id after incremental merge: {chunk_id}")
        content = str(candidate.get("content") or "")
        # V2's full build deduplicates identical runtime evidence. Keep the
        # same invariant across a delta without dropping the older frozen FAQ
        # slice merely because a GitLab page happens to quote it.
        content_key = content.encode("utf-8")
        if content_key in seen_content:
            duplicate_content_skipped += 1
            continue
        seen_ids.add(chunk_id)
        seen_content.add(content_key)
        merged.append(candidate)
    merged.sort(key=lambda row: str(row["chunk_id"]))
    return merged, {
        "retained_rows": len(retained),
        "removed_rows": removed,
        "inserted_rows": len(changed_rows),
        "duplicate_content_skipped": duplicate_content_skipped,
        "merged_rows": len(merged),
    }


def build_incremental_candidate(
    *,
    docs_repo: Path,
    base_corpus: Path,
    changes: list[SourceChange],
    head_revision: str,
    kb_version: str,
    valid_from: str,
    out_dir: Path,
    semantic_client: ModelVerseClient | None,
    semantic_model: str,
) -> CandidateResult:
    review_issues = changed_asset_issues(docs_repo, changes)
    rebuild_paths, replace_paths = changed_source_paths(changes)
    report: dict[str, Any] = {
        "schema_version": "compshare.rag.gitlab-sync.v1",
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "head_revision": head_revision,
        "kb_version": kb_version,
        "valid_from": valid_from,
        "changes": [asdict(change) for change in changes],
        "rebuild_paths": sorted(rebuild_paths),
        "replace_paths": sorted(replace_paths),
        "review_issues": review_issues,
    }
    if review_issues:
        report["status"] = "review_required"
        return CandidateResult(status="review_required", report=report)

    all_docs = {
        document.source_path: document
        for document in collect_internal_docs(docs_repo, source_revision=head_revision)
    }
    missing_docs = sorted(path for path in rebuild_paths if path not in all_docs)
    if missing_docs:
        report["status"] = "review_required"
        report["review_issues"] = [
            *review_issues,
            *[{"path": path, "reason": "changed_markdown_not_collectable"} for path in missing_docs],
        ]
        return CandidateResult(status="review_required", report=report)

    if not rebuild_paths and not replace_paths:
        report["status"] = "no_relevant_changes"
        return CandidateResult(status="no_relevant_changes", report=report)

    changed_rows, changed_stats = build_chunks(
        [all_docs[path] for path in sorted(rebuild_paths)],
        kb_version=kb_version,
        valid_from=valid_from,
        asset_notes={},
        semantic_client=semantic_client,
        semantic_model=semantic_model,
    )
    base_rows = read_jsonl(base_corpus)
    merged_rows, merge_stats = merge_incremental_rows(
        base_rows,
        changed_rows,
        replace_paths=replace_paths,
        kb_version=kb_version,
    )
    errors = validate_chunks(merged_rows, expected_version=kb_version)
    if errors:
        raise ValueError("incremental candidate validation failed:\n" + "\n".join(errors[:100]))

    out_dir.mkdir(parents=True, exist_ok=True)
    candidate_path = out_dir / "stage2b_v2.jsonl"
    write_jsonl(candidate_path, merged_rows)
    report.update({
        "status": "candidate_ready",
        "changed": changed_stats,
        "merge": merge_stats,
        "artifacts": {
            "internal_corpus": {
                "path": candidate_path.name,
                "sha256": normalized_file_digest(candidate_path),
                "rows": len(merged_rows),
            },
        },
        "next_gate": "refresh qwen3 sidecar from the previous release, then run release validation and update Go digest pins in the approved release change",
    })
    manifest_path = out_dir / "gitlab_sync_manifest.json"
    manifest_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return CandidateResult(status="candidate_ready", report=report, candidate_path=candidate_path)


def read_state(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    payload = json.loads(path.read_text(encoding="utf-8"))
    if payload.get("schema_version") != "compshare.rag.gitlab-sync-state.v1":
        raise ValueError(f"unsupported state schema in {path}")
    return payload


def write_state_proposal(path: Path, *, head_revision: str, base_corpus: Path, result: CandidateResult) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = {
        "schema_version": "compshare.rag.gitlab-sync-state.v1",
        "proposed_at": datetime.now(timezone.utc).isoformat(),
        "head_revision": head_revision,
        "base_corpus": str(base_corpus),
        "corpus_digest": result.report.get("artifacts", {}).get("internal_corpus", {}).get("sha256") or normalized_file_digest(base_corpus),
    }
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def write_report(path: Path, report: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Build a V2 candidate corpus for a GitLab docs delta; never promotes production files.")
    parser.add_argument("--docs-repo", type=Path, required=True, help="Checked-out compshare-docs worktree at the target commit.")
    parser.add_argument("--base-corpus", type=Path, required=True, help="Current platform V2 corpus, including frozen FAQ rows.")
    parser.add_argument("--base-revision", help="Previously approved GitLab commit; defaults to --state-file head_revision.")
    parser.add_argument("--head-revision", help="Target GitLab commit; defaults to the checkout's HEAD.")
    parser.add_argument("--state-file", type=Path, help="Read-only last-approved state; release CI advances it only after promotion.")
    parser.add_argument("--out-dir", type=Path, required=True, help="Isolated candidate artifact directory, never deploy/kb.")
    parser.add_argument("--kb-version", required=True, help="New immutable corpus version, e.g. kb.platform.v2.2026-08-02.<sha>.")
    parser.add_argument("--valid-from", required=True)
    parser.add_argument("--env", type=Path, help="ModelVerse credential file; required unless --skip-semantic.")
    parser.add_argument("--semantic-model", default="qwen3.7-max")
    parser.add_argument("--skip-semantic", action="store_true", help="Test/diagnostic only; do not use for an approved release.")
    args = parser.parse_args(argv)

    if not args.docs_repo.is_dir():
        parser.error(f"--docs-repo is not a directory: {args.docs_repo}")
    if not args.base_corpus.is_file():
        parser.error(f"--base-corpus is not a file: {args.base_corpus}")
    if args.out_dir.resolve() == args.base_corpus.parent.resolve():
        parser.error("--out-dir must be an isolated candidate directory, not the approved corpus directory")
    if not args.skip_semantic and args.env is None:
        parser.error("--env is required unless --skip-semantic is explicitly used")

    state = read_state(args.state_file) if args.state_file else {}
    base_revision = args.base_revision or str(state.get("head_revision") or "")
    if not base_revision:
        parser.error("--base-revision is required on the first run (or provide a populated --state-file)")
    checkout_head = git_revision(args.docs_repo)
    head_revision = git_revision(args.docs_repo, args.head_revision or checkout_head)
    if head_revision != checkout_head:
        parser.error("--docs-repo must be checked out exactly at --head-revision; fetch/checkout happens outside this builder")

    changes = git_changes(args.docs_repo, base_revision, head_revision)
    semantic_client = None
    if not args.skip_semantic:
        env = load_env(args.env)
        semantic_client = ModelVerseClient(
            base_url=env.get("MODELVERSE_BASE_URL", "https://api.modelverse.cn/v1"),
            api_key=env.get("MODELVERSE_API_KEY", ""),
            cache_dir=args.out_dir / ".cache" / "modelverse",
        )
    result = build_incremental_candidate(
        docs_repo=args.docs_repo,
        base_corpus=args.base_corpus,
        changes=changes,
        head_revision=head_revision,
        kb_version=args.kb_version,
        valid_from=args.valid_from,
        out_dir=args.out_dir,
        semantic_client=semantic_client,
        semantic_model=args.semantic_model,
    )
    write_report(args.out_dir / "gitlab_sync_report.json", result.report)
    if result.status == "review_required":
        print("status=review_required; candidate was not produced", file=sys.stderr)
        return 2
    # Never advance --state-file here. A candidate may still fail embedding,
    # judge, digest-pin, or deployment validation; advancing the base revision
    # before the matching corpus is approved would silently skip source edits
    # on the next scheduled run. Release CI promotes this proposal atomically
    # with the validated corpus and sidecar instead.
    write_state_proposal(args.out_dir / "gitlab_sync_state_proposal.json", head_revision=head_revision, base_corpus=args.base_corpus, result=result)
    print(f"status={result.status}")
    if result.candidate_path:
        print(f"candidate={result.candidate_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
