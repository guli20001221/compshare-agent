#!/usr/bin/env python3
"""Decide whether a knowledge candidate may be published without a human.

"Confirmed to be fine" cannot mean *the content is correct* -- no machine judges
that. It can mean *every difference is attributable*, which is checkable, and is
the property that actually matters here: the compshare-docs half of the corpus
was already reviewed when its merge request landed upstream. The other half --
the after-sales FAQ ZIPs and the scraped external snapshots -- has no upstream
reviewer to inherit from, so the answer for those is not to judge them but to
prove they did not move.

Two rules shaped this file, both from defects already found in this tree:

  * The verdict does not live in release_diff.py. release.py calls that module
    as a function and DISCARDS its return value, so its `return 1` is a no-op
    and always was. A gate has to be its own process with its own exit code.

  * A missing input is a FAILURE, never a pass. The most recent empty gate here
    was `degradations`: an asset report written before the key existed read as
    "nothing degraded" through `.get(...) or []`, so the human-review section
    had never once rendered. Every check below states what it read; a check that
    could not read its evidence reports `ok=False` with `evidence_missing`.

Modes. `--mode shadow` (the default) evaluates everything, writes the verdict,
and ALWAYS exits 0. That is not a weaker gate, it is the only way to learn the
base rate the thresholds need before the thresholds can block anything.
`--mode enforce` exits 1 when any blocking check failed.
"""
from __future__ import annotations

import argparse
import json
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable

try:
    from .pipeline import normalized_file_digest, sha256_bytes
except ImportError:  # pragma: no cover - direct script execution
    from pipeline import normalized_file_digest, sha256_bytes  # type: ignore


# The source id every reviewable document carries. A document whose ref does not
# start with this cannot be attributed to a docs commit, by construction.
DOCS_SOURCE_ID = "gitlab-compshare-docs"

# Partitions with no upstream reviewer. They are not judged; they are frozen.
FROZEN_SOURCE_IDS = ("faq-model-package", "faq-comfyui-base", "faq-usage")

# Flags that turn off one of the build's own blocking raises. --skip-vl is the
# worst of the three: it zeroes the asset notes, disables the blocking-failure
# raise by construction, and leaves the caption lock unwritten, so release_diff
# compares that lock to itself and reports nothing changed while a third of the
# internal corpus text disappears.
DISABLING_BUILD_FLAGS = ("--skip-vl", "--skip-semantic", "--allow-stale-remote")

# Asia/Shanghai as a fixed offset rather than a zoneinfo lookup: the comparison
# is on a DATE, China has observed no DST since 1991, and a tzdata dependency
# that resolves differently on the build image than on a laptop would be a new
# way for this check to disagree with itself.
CHINA = timezone(timedelta(hours=8))


@dataclass
class Finding:
    """One assertion, its verdict, and what it read to get there."""

    id: str
    title: str
    ok: bool
    detail: str
    blocking: bool = True
    # Set when the check could not run at all. Kept distinct from a plain
    # failure so a broken invocation never looks like a rejected candidate.
    evidence_missing: bool = False

    def as_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "title": self.title,
            "ok": self.ok,
            "blocking": self.blocking,
            "evidence_missing": self.evidence_missing,
            "detail": self.detail,
        }


@dataclass
class Inputs:
    """Everything the gate reads, resolved once so a miss is reported once."""

    release_dir: Path
    released_internal: Path
    released_external: Path
    released_manifest: Path
    docs_diff: Path | None = None
    today: date = field(default_factory=lambda: datetime.now(CHINA).date())

    @property
    def candidate_internal(self) -> Path:
        return self.release_dir / "stage2b_v2.jsonl"

    @property
    def candidate_external(self) -> Path:
        return self.release_dir / "external_v2.jsonl"

    @property
    def candidate_manifest(self) -> Path:
        return self.release_dir / "release_manifest.json"

    @property
    def asset_report(self) -> Path:
        return self.release_dir / "asset_report.json"

    @property
    def asset_lock(self) -> Path:
        return self.release_dir / "asset_lock.json"

    @property
    def diff_json(self) -> Path:
        return self.release_dir / "release_diff.json"


def _read_json(path: Path) -> Any | None:
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError):
        return None


def _read_rows(path: Path) -> list[dict[str, Any]] | None:
    if not path.exists():
        return None
    rows = []
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        if line.strip():
            rows.append(json.loads(line))
    return rows


def _missing(check_id: str, title: str, what: str) -> Finding:
    return Finding(
        id=check_id,
        title=title,
        ok=False,
        detail=f"could not read {what}; this check did not run",
        evidence_missing=True,
    )


def _document_of(row: dict[str, Any]) -> str:
    refs = row.get("source_refs") or []
    return str(refs[0]) if refs else str(row.get("document_id") or "?")


def _source_of(document: str) -> str:
    return document.partition(":")[0]


def _path_of(document: str) -> str:
    return document.partition(":")[2]


def _revision_of(manifest: Any) -> str:
    for source in (manifest or {}).get("sources") or []:
        if source.get("id") == DOCS_SOURCE_ID:
            return str(source.get("revision") or "")
    return ""


def _zip_digests(manifest: Any) -> dict[str, str]:
    return {
        str(source.get("id")): str(source.get("sha256") or "")
        for source in (manifest or {}).get("sources") or []
        if source.get("kind") == "zip"
    }


# --------------------------------------------------------------------------
# G0 -- the docs actually moved
# --------------------------------------------------------------------------
def check_docs_revision_moved(inputs: Inputs) -> Finding:
    title = "G0 compshare-docs 版本确实前进了"
    candidate = _read_json(inputs.candidate_manifest)
    released = _read_json(inputs.released_manifest)
    if candidate is None or released is None:
        return _missing("G0", title, "release_manifest.json (candidate or released)")
    new, old = _revision_of(candidate), _revision_of(released)
    if not new or not old:
        return Finding("G0", title, False,
                       f"a manifest records no {DOCS_SOURCE_ID} revision (released={old!r}, candidate={new!r})")
    if new == old:
        return Finding("G0", title, False,
                       f"both manifests record {new[:12]}; this release would change nothing "
                       "a docs commit asked for")
    return Finding("G0", title, True, f"{old[:12]} -> {new[:12]}")


# --------------------------------------------------------------------------
# G1 -- no safety flag was in play
# --------------------------------------------------------------------------
def check_build_ran_with_nothing_disabled(inputs: Inputs) -> list[Finding]:
    """Two independent readings, because a recorder can only attest to itself.

    The argv check names the flag for a human. The artifact checks are the
    evidence: they read what those flags CHANGE, so they hold even if the argv
    record is wrong, absent, or written by something else entirely.
    """
    findings: list[Finding] = []
    manifest = _read_json(inputs.candidate_manifest)
    argv = ((manifest or {}).get("report") or {}).get("build_argv")
    if argv is None:
        findings.append(_missing("G1.argv", "G1 构建参数里没有关闭安全检查的开关",
                                 "report.build_argv in release_manifest.json"))
    else:
        present = [flag for flag in DISABLING_BUILD_FLAGS if flag in argv]
        findings.append(Finding(
            "G1.argv", "G1 构建参数里没有关闭安全检查的开关", not present,
            f"build_argv carries {present}" if present else
            f"none of {list(DISABLING_BUILD_FLAGS)} in {len(argv)} recorded arguments"))

    report = _read_json(inputs.asset_report)
    if report is None:
        findings.append(_missing("G1.captions", "G1 图片确实被描述过了", "asset_report.json"))
    else:
        described = int(report.get("described") or 0)
        findings.append(Finding(
            "G1.captions", "G1 图片确实被描述过了", described > 0,
            f"asset_report.json describes {described} images"
            + ("" if described else " -- this is what --skip-vl produces")))
        # Distinguished from absent for the same reason release_diff does: a
        # report predating the key never examined the question, and reading that
        # as "clean" is the exact empty gate this file exists to not repeat.
        if "degradations" not in report:
            findings.append(Finding(
                "G1.stale-remote", "G1 远端图片回源校验有结论", False,
                "asset_report.json has no `degradations` key, so it never answered "
                "whether any image came from cache. Not the same as 'none did'.",
                evidence_missing=True))
        else:
            stale = report.get("degradations") or []
            blocking = [item for item in stale if item.get("severity") == "error"]
            findings.append(Finding(
                "G1.stale-remote", "G1 远端图片回源校验有结论", not blocking,
                f"{len(stale)} degraded, {len(blocking)} of them platform sources"))

    lock = _read_json(inputs.asset_lock)
    if lock is None:
        findings.append(_missing("G1.lock", "G1 caption lock 是本次构建写出来的", "asset_lock.json"))
    else:
        contract = str(lock.get("contract") or "")
        notes = lock.get("notes") or []
        findings.append(Finding(
            "G1.lock", "G1 caption lock 是本次构建写出来的", bool(contract) and bool(notes),
            f"contract={contract[:12] or '(none)'} notes={len(notes)}"))
    return findings


# --------------------------------------------------------------------------
# G2 -- the six vendored inputs are the same six
# --------------------------------------------------------------------------
def check_input_zips_unchanged(inputs: Inputs) -> Finding:
    title = "G2 六个 ZIP 输入的 sha256 未变"
    candidate = _read_json(inputs.candidate_manifest)
    released = _read_json(inputs.released_manifest)
    if candidate is None or released is None:
        return _missing("G2", title, "release_manifest.json (candidate or released)")
    new, old = _zip_digests(candidate), _zip_digests(released)
    if not new or not old:
        return Finding("G2", title, False,
                       f"a manifest declares no zip sources (released={len(old)}, candidate={len(new)})")
    moved = sorted(
        key for key in new.keys() | old.keys()
        if new.get(key) != old.get(key)
    )
    if moved:
        return Finding("G2", title, False, f"changed or missing zip inputs: {moved}")
    return Finding("G2", title, True, f"{len(new)} zip inputs identical")


# --------------------------------------------------------------------------
# G3 -- valid_from is a real date, and not in the future
# --------------------------------------------------------------------------
def check_valid_from_is_in_effect(inputs: Inputs) -> Finding:
    title = "G3 每条 chunk 的 valid_from 都已生效"
    rows: list[dict[str, Any]] = []
    for path in (inputs.candidate_internal, inputs.candidate_external):
        loaded = _read_rows(path)
        if loaded is None:
            return _missing("G3", title, str(path))
        rows.extend(loaded)
    unparsable: list[str] = []
    future: list[str] = []
    for row in rows:
        raw = str(row.get("valid_from") or "")
        try:
            stamped = date.fromisoformat(raw)
        except ValueError:
            unparsable.append(f"{row.get('chunk_id')}={raw!r}")
            continue
        if stamped > inputs.today:
            future.append(f"{row.get('chunk_id')}={raw}")
    if unparsable or future:
        return Finding(
            "G3", title, False,
            f"{len(unparsable)} unparsable, {len(future)} dated after {inputs.today.isoformat()} "
            f"(Asia/Shanghai). A future date publishes a corpus that retrieves nothing. "
            f"First few: {(unparsable + future)[:5]}")
    stamps = sorted({str(row.get("valid_from")) for row in rows})
    return Finding("G3", title, True,
                   f"{len(rows)} chunks, valid_from in {stamps[:4]}"
                   + (" …" if len(stamps) > 4 else "")
                   + f", all ≤ {inputs.today.isoformat()}")


# --------------------------------------------------------------------------
# G4 -- the partitions with no upstream reviewer did not move
# --------------------------------------------------------------------------
def _frozen_projection(rows: Iterable[dict[str, Any]], sources: tuple[str, ...] | None) -> str:
    """Canonical digest of one partition, ignoring the release date stamp.

    kb_version and valid_from are RELEASE metadata: a docs-only rebuild moves
    them on every internal chunk including the FAQ ones, because the internal
    corpus carries a single kb_version. Freezing them too would make this check
    impossible to satisfy and it would be removed, which is worse than scoping
    it honestly. What is frozen here is everything a reader would call content.
    """
    selected = []
    for row in rows:
        if sources is not None and _source_of(_document_of(row)) not in sources:
            continue
        stripped = {key: value for key, value in row.items()
                    if key not in {"kb_version", "valid_from"}}
        selected.append(json.dumps(stripped, ensure_ascii=False, sort_keys=True))
    selected.sort()
    return sha256_bytes("\n".join(selected).encode("utf-8")) + f":{len(selected)}"


def check_frozen_partitions(inputs: Inputs) -> list[Finding]:
    findings: list[Finding] = []

    faq_title = "G4 售后 FAQ 分区内容未变"
    old_internal = _read_rows(inputs.released_internal)
    new_internal = _read_rows(inputs.candidate_internal)
    if old_internal is None or new_internal is None:
        findings.append(_missing("G4.faq", faq_title, "the internal corpora"))
    else:
        old_print = _frozen_projection(old_internal, FROZEN_SOURCE_IDS)
        new_print = _frozen_projection(new_internal, FROZEN_SOURCE_IDS)
        findings.append(Finding(
            "G4.faq", faq_title, old_print == new_print,
            f"released {old_print[:12]}…/{old_print.rsplit(':', 1)[1]} chunks vs "
            f"candidate {new_print[:12]}…/{new_print.rsplit(':', 1)[1]} chunks "
            "(kb_version and valid_from excluded -- they are release stamps, not content)"))

    ext_title = "G4 外部语料逐字节未变"
    if not inputs.released_external.exists() or not inputs.candidate_external.exists():
        findings.append(_missing("G4.external", ext_title, "the external corpora"))
        return findings
    old_digest = normalized_file_digest(inputs.released_external)
    new_digest = normalized_file_digest(inputs.candidate_external)
    if old_digest == new_digest:
        findings.append(Finding("G4.external", ext_title, True, f"{old_digest[:16]} unchanged"))
        return findings
    # Byte-inequality has two very different causes and the reviewer should not
    # have to work them out. Only the release stamp moving is a build-argv
    # mistake (--external-valid-from was not held back) and costs a rebuilt
    # 63 MB sidecar for no source change; content moving is a real edit in a
    # partition that has no upstream reviewer. Both block. They do not read the
    # same, so they must not print the same.
    old_rows, new_rows = _read_rows(inputs.released_external), _read_rows(inputs.candidate_external)
    if old_rows is None or new_rows is None:
        hint = "could not reread the corpora to say which"
    elif _frozen_projection(old_rows, None) == _frozen_projection(new_rows, None):
        hint = ("only the release stamp moved -- rebuild passing --external-valid-from "
                "with the previous external date")
    else:
        hint = "the external CONTENT changed, and nothing upstream reviewed it"
    findings.append(Finding("G4.external", ext_title, False,
                            f"{old_digest[:16]} -> {new_digest[:16]}; {hint}"))
    return findings


# --------------------------------------------------------------------------
# G5 -- every changed internal document is explained by a docs commit
# --------------------------------------------------------------------------
def _changed_internal_documents(diff: dict[str, Any]) -> set[str]:
    internal = None
    for name, corpus in (diff.get("corpora") or {}).items():
        if "stage2b" in name:
            internal = corpus
            break
    if internal is None:
        return set()
    touched: set[str] = set()
    touched.update(internal.get("documents_added") or [])
    touched.update(internal.get("documents_removed") or [])
    for move in internal.get("documents_moved") or []:
        touched.add(str(move.get("from")))
        touched.add(str(move.get("to")))
    for key in ("sections_changed", "sections_metadata_changed", "sections_added", "sections_removed"):
        for section in internal.get(key) or []:
            touched.add(str(section.get("document")))
    return {doc for doc in touched if doc and doc != "?"}


def _metadata_only_documents(diff: dict[str, Any]) -> tuple[set[str], set[str]]:
    """Documents whose only change was in served metadata, and the field names.

    Kept apart from _changed_internal_documents so G5 can SAY which view put a
    document on the list. A rewrite of the question-pattern or product-area rule
    is a code change, not a docs change, and it lands as dozens of unattributed
    documents at once -- a reviewer reading only "no docs commit touched this
    path" would go looking in compshare-docs for something that is not there.
    """
    internal = None
    for name, corpus in (diff.get("corpora") or {}).items():
        if "stage2b" in name:
            internal = corpus
            break
    if internal is None:
        return set(), set()
    body_moved: set[str] = set()
    for key in ("sections_changed", "sections_added", "sections_removed"):
        for section in internal.get(key) or []:
            body_moved.add(str(section.get("document")))
    for item in internal.get("documents_moved") or []:
        body_moved.add(str(item.get("from")))
        body_moved.add(str(item.get("to")))
    body_moved.update(internal.get("documents_added") or [])
    body_moved.update(internal.get("documents_removed") or [])

    documents: set[str] = set()
    fields: set[str] = set()
    for section in internal.get("sections_metadata_changed") or []:
        document = str(section.get("document"))
        if document in body_moved:
            continue
        documents.add(document)
        fields.update(str(name) for name in (section.get("fields") or []))
    return {doc for doc in documents if doc and doc != "?"}, fields


def check_every_change_is_attributed(inputs: Inputs) -> Finding:
    title = "G5 每个变动的平台文档都能对应到一次 docs 提交"
    diff = _read_json(inputs.diff_json)
    if diff is None:
        return _missing("G5", title, "release_diff.json")
    if inputs.docs_diff is None or not inputs.docs_diff.exists():
        return _missing("G5", title, "the docs diff (--docs-diff)")
    changed_paths = {
        line.strip().replace("\\", "/")
        for line in inputs.docs_diff.read_text(encoding="utf-8").splitlines()
        if line.strip()
    }
    touched = _changed_internal_documents(diff)
    unattributed: list[str] = []
    for document in sorted(touched):
        if _source_of(document) != DOCS_SOURCE_ID:
            unattributed.append(f"{document} (not a compshare-docs document)")
        elif _path_of(document) not in changed_paths:
            unattributed.append(f"{document} (no docs commit touched this path)")
    if unattributed:
        detail = (f"{len(unattributed)} of {len(touched)} changed documents are unattributed: "
                  f"{unattributed[:8]}")
        metadata_only, fields = _metadata_only_documents(diff)
        overlap = metadata_only & {item.split(" (", 1)[0] for item in unattributed}
        if overlap:
            detail += (f". {len(overlap)} of them changed ONLY in served metadata "
                       f"({', '.join(sorted(fields))}) with the body byte-identical -- "
                       "if that is most of the list, suspect a change to the "
                       "question-pattern or product-area rule rather than a docs edit.")
        return Finding("G5", title, False, detail)
    metadata_only, fields = _metadata_only_documents(diff)
    detail = (f"{len(touched)} changed documents, all present in a docs diff of "
              f"{len(changed_paths)} paths")
    if metadata_only:
        detail += (f" (of which {len(metadata_only)} changed only in served metadata: "
                   f"{', '.join(sorted(fields))})")
    return Finding("G5", title, True, detail)


# --------------------------------------------------------------------------
# G6 -- captions did not drift on documents nobody touched
# --------------------------------------------------------------------------
def check_caption_drift_is_attributed(inputs: Inputs) -> Finding:
    title = "G6 重写的图片说明都落在有据可查的文档里"
    diff = _read_json(inputs.diff_json)
    lock = _read_json(inputs.asset_lock)
    if diff is None or lock is None:
        return _missing("G6", title, "release_diff.json / asset_lock.json")
    captions = diff.get("captions")
    if captions is None:
        return _missing("G6", title, "the captions section of release_diff.json")
    if captions.get("contract_before") != captions.get("contract_after"):
        return Finding("G6", title, False,
                       "the caption contract changed, so EVERY caption was re-earned. "
                       "That is a deliberate act (a Pillow or preprocess bump), not a docs "
                       "update, and it needs a human.")
    rewritten = captions.get("captions_rewritten_for_identical_bytes") or []
    if not rewritten:
        return Finding("G6", title, True, "no caption was rewritten for unchanged bytes")
    # An asset can be referenced from more than one document; the lock carries a
    # note per reference, so every document that shows this image has to be
    # attributed, not just the first one release_diff happened to keep.
    owners: dict[str, set[str]] = {}
    for note in lock.get("notes") or []:
        owners.setdefault(str(note.get("asset_id")), set()).add(
            f"{note.get('source_id')}:{note.get('source_path')}")
    diff_docs = _changed_internal_documents(diff)
    stray: list[str] = []
    for item in rewritten:
        asset_id = str(item.get("asset_id"))
        for document in sorted(owners.get(asset_id, {f"(unknown owner of {asset_id})"})):
            if document not in diff_docs:
                stray.append(f"{asset_id} in {document}")
    if stray:
        return Finding("G6", title, False,
                       f"{len(rewritten)} captions rewritten for identical bytes; "
                       f"{len(stray)} of those references sit in documents this release did not "
                       f"otherwise touch: {stray[:8]}")
    return Finding("G6", title, True,
                   f"{len(rewritten)} rewritten captions, all inside documents this release changed")


# --------------------------------------------------------------------------
# G7 -- the crash barrier
# --------------------------------------------------------------------------
def check_corpus_volume(inputs: Inputs, *, max_shrink: float | None) -> Finding:
    title = "G7 语料体量没有异常缩水"
    old_rows = _read_rows(inputs.released_internal)
    new_rows = _read_rows(inputs.candidate_internal)
    if old_rows is None or new_rows is None:
        return _missing("G7", title, "the internal corpora")
    before = sum(len(str(row.get("content") or "")) for row in old_rows)
    after = sum(len(str(row.get("content") or "")) for row in new_rows)
    delta = (after - before) / before if before else 0.0
    detail = (f"internal corpus {before} -> {after} runes ({delta:+.2%}), "
              f"{len(old_rows)} -> {len(new_rows)} chunks")
    if max_shrink is None:
        # Deliberately not a guessed number. Shadow mode exists to measure what
        # a normal release looks like; inventing a threshold now would either
        # never fire or fire on the first ordinary rewrite, and either way the
        # number would be defended by nothing.
        return Finding("G7", title, True, detail + "; no threshold set (--max-shrink unset)",
                       blocking=False)
    return Finding("G7", title, delta >= -abs(max_shrink),
                   detail + f"; threshold -{abs(max_shrink):.2%}")


# --------------------------------------------------------------------------
# G8 -- the incremental build was actually incremental
# --------------------------------------------------------------------------
def check_embedding_reuse(inputs: Inputs, *, min_reuse: float | None) -> Finding:
    """Report how many vectors were reused, because nothing else can.

    Vector reuse is keyed on chunk_repr -- title, question patterns and
    truncated content -- and NOT on chunk_id. The first two are absent from the
    chunk_id hash, so a change to the question-pattern or product-area rule
    invalidates every vector while every id stays put. Measured on one real
    release pair: 78 of 526 chunk ids survived and 0 of 526 vectors did.

    That degradation raises nothing. The corpus is correct, the sidecar is
    correct, the digests bind, every other check here passes -- the build simply
    made 1715 model calls instead of three, and no artifact said so. This is the
    only place that number becomes visible, which is why the check exists even
    while it cannot fail.
    """
    title = "G8 向量复用符合预期"
    manifest = _read_json(inputs.candidate_manifest)
    if manifest is None:
        return _missing("G8", title, "release_manifest.json")
    reports = ((manifest.get("report") or {}).get("embeddings")) or {}
    if not reports:
        # Not a pass. A release built before this was recorded, or by a path that
        # skipped the refresh, has no evidence either way -- and "no evidence"
        # must not read as "reused everything".
        return _missing("G8", title, "report.embeddings in release_manifest.json")

    # Everything below exists because a PARTIAL or STALE record is more dangerous
    # than an absent one: it reads as a measurement. Three shapes were reachable
    # before these checks and all three reported healthy reuse --
    #   only `external` recorded, hiding a full re-embed of the internal corpus;
    #   reused=526 embedded=526 chunks=526, which cannot all be true;
    #   chunks=3 carried over from a previous release, against a 526-row corpus.
    # Each is now evidence_missing, because the check could not actually run.
    corpora = {
        "stage2b": inputs.candidate_internal,
        "external": inputs.candidate_external,
    }
    absent = sorted(name for name in corpora if name not in reports)
    if absent:
        return _missing("G8", title,
                        f"an embedding record for {absent} (only {sorted(reports)} recorded)")

    parts = []
    problems = []
    worst = 1.0
    for corpus, path in sorted(corpora.items()):
        item = reports[corpus]
        chunks = int(item.get("chunks") or 0)
        reused = int(item.get("reused") or 0)
        embedded = int(item.get("embedded") or 0)

        if chunks <= 0:
            problems.append(f"{corpus} records {chunks} chunks")
            continue
        if reused + embedded != chunks:
            problems.append(
                f"{corpus} reused+embedded={reused + embedded} but chunks={chunks}")
        actual = _read_rows(path)
        if actual is None:
            problems.append(f"{corpus} corpus file {path.name} is unreadable")
        elif len(actual) != chunks:
            # The stale-report case, and the only one that needs an artifact to
            # detect: the numbers are internally consistent, they just describe a
            # different release.
            problems.append(
                f"{corpus} records {chunks} chunks but {path.name} holds {len(actual)}")
        recorded_digest = str(item.get("corpus_digest") or "")
        if recorded_digest and path.exists():
            actual_digest = normalized_file_digest(path)
            if recorded_digest != actual_digest:
                problems.append(
                    f"{corpus} record is bound to corpus {recorded_digest[:12]}, "
                    f"candidate is {actual_digest[:12]}")

        ratio = reused / chunks
        worst = min(worst, ratio)
        parts.append(f"{corpus} reused {reused}/{chunks} ({ratio:.1%}), embedded {embedded}")

    if problems:
        return _missing("G8", title, "a coherent embedding record: " + "; ".join(problems))
    detail = "; ".join(parts)

    if min_reuse is None:
        # Same reasoning as G7: shadow mode exists to learn what an ordinary
        # release looks like. A threshold invented now would be defended by
        # nothing. What IS worth saying without one is the degenerate case,
        # because zero reuse across a whole corpus is not a matter of degree.
        if worst <= 0.0:
            detail += ("; NOTHING was reused -- if the docs diff is small, suspect a "
                       "changed question-pattern or product-area rule rather than a "
                       "docs edit, and expect the model bill of a full rebuild")
        return Finding("G8", title, True, detail + "; no threshold set (--min-reuse unset)",
                       blocking=False)
    return Finding("G8", title, worst >= min_reuse,
                   detail + f"; threshold {min_reuse:.1%}")


def evaluate(inputs: Inputs, *, max_shrink: float | None = None,
             min_reuse: float | None = None) -> list[Finding]:
    findings = [check_docs_revision_moved(inputs)]
    findings += check_build_ran_with_nothing_disabled(inputs)
    findings.append(check_input_zips_unchanged(inputs))
    findings.append(check_valid_from_is_in_effect(inputs))
    findings += check_frozen_partitions(inputs)
    findings.append(check_every_change_is_attributed(inputs))
    findings.append(check_caption_drift_is_attributed(inputs))
    findings.append(check_corpus_volume(inputs, max_shrink=max_shrink))
    findings.append(check_embedding_reuse(inputs, min_reuse=min_reuse))
    return findings


def verdict(findings: list[Finding]) -> dict[str, Any]:
    blocking_failures = [f for f in findings if f.blocking and not f.ok]
    return {
        "auto_publishable": not blocking_failures,
        "checks_total": len(findings),
        "checks_failed": len([f for f in findings if not f.ok]),
        "blocking_failures": [f.id for f in blocking_failures],
        "evidence_missing": [f.id for f in findings if f.evidence_missing],
        "findings": [f.as_dict() for f in findings],
    }


def render_markdown(result: dict[str, Any], *, mode: str) -> str:
    lines = ["# 知识库候选发布闸门", ""]
    if result["auto_publishable"]:
        lines.append("**结论：每一处差异都能归因，可以自动发布。**")
    else:
        lines.append(f"**结论：{len(result['blocking_failures'])} 项阻断检查未通过，需要人看。**")
    if mode == "shadow":
        lines += ["", "> 当前是 shadow 模式：这份结论只是记录，不会拦下任何东西。"]
    lines += ["", "| | 检查 | 结果 | 依据 |", "|---|---|---|---|"]
    for finding in result["findings"]:
        if finding["evidence_missing"]:
            mark = "证据缺失"
        elif finding["ok"]:
            mark = "通过"
        else:
            mark = "阻断" if finding["blocking"] else "仅记录"
        detail = str(finding["detail"]).replace("|", "\\|")
        lines.append(f"| `{finding['id']}` | {finding['title']} | {mark} | {detail} |")
    lines += ["", "---", "",
              "归因的含义：docs 那半边继承上游 MR 的评审；没有上游评审的那半边不评审，只证明它没动。"]
    return "\n".join(lines) + "\n"


def main(argv: list[str] | None = None) -> int:
    # allow_abbrev=False for the same reason build.py and release.py set it. This
    # parser now carries --mode, --max-shrink and --min-reuse, and an abbreviation
    # landing a threshold on the wrong check, or `--mod enforce` resolving to
    # --mode, is a silent wrong answer from the process whose entire job is to
    # give a correct one.
    parser = argparse.ArgumentParser(
        description="Decide whether a knowledge candidate is safe to publish unattended.",
        allow_abbrev=False)
    parser.add_argument("--release-dir", type=Path, default=Path("deploy/kb/v2"))
    parser.add_argument("--released-internal", type=Path, required=True,
                        help="the internal corpus as it stood BEFORE this build")
    parser.add_argument("--released-external", type=Path, required=True)
    parser.add_argument("--released-manifest", type=Path, required=True)
    parser.add_argument("--docs-diff", type=Path,
                        help="file of paths from `git diff --name-only <pinned>..<head>`")
    parser.add_argument("--mode", choices=("shadow", "enforce"), default="shadow")
    parser.add_argument("--max-shrink", type=float,
                        help="fraction, e.g. 0.05; omit until shadow runs have shown the base rate")
    parser.add_argument("--min-reuse", type=float,
                        help="fraction of vectors that must be reused, e.g. 0.5; omit until "
                             "shadow runs have shown the base rate")
    parser.add_argument("--today", help="YYYY-MM-DD override for G3; defaults to Asia/Shanghai today")
    parser.add_argument("--out-json", type=Path)
    parser.add_argument("--out-md", type=Path)
    args = parser.parse_args(argv)

    inputs = Inputs(
        release_dir=args.release_dir,
        released_internal=args.released_internal,
        released_external=args.released_external,
        released_manifest=args.released_manifest,
        docs_diff=args.docs_diff,
        today=date.fromisoformat(args.today) if args.today else datetime.now(CHINA).date(),
    )
    result = evaluate(inputs, max_shrink=args.max_shrink, min_reuse=args.min_reuse)
    payload = verdict(result)
    payload["mode"] = args.mode

    markdown = render_markdown(payload, mode=args.mode)
    if args.out_md:
        args.out_md.parent.mkdir(parents=True, exist_ok=True)
        args.out_md.write_text(markdown, encoding="utf-8")
    if args.out_json:
        args.out_json.parent.mkdir(parents=True, exist_ok=True)
        args.out_json.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8")
    print(markdown)

    if payload["auto_publishable"]:
        print("gate: every difference is attributable")
    else:
        print(f"gate: blocked by {payload['blocking_failures']}")
    if args.mode == "shadow":
        # The whole point of shadow. Exit 0 even on a rejection, so the verdict
        # is recorded against releases a human is still approving.
        print("gate: shadow mode, not failing the pipeline")
        return 0
    return 0 if payload["auto_publishable"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
