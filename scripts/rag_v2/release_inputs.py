#!/usr/bin/env python3
"""The few facts an unattended release has to READ off its own artifacts.

Each of these was, at one point, a `sed` or a `python -c` embedded in a CI job.
They live here instead for three reasons: a regex over a 200 KB JSON document
keeps working right up until the day it silently does not; a multi-line
`python -c` cannot be indented inside a YAML block scalar without becoming an
IndentationError; and neither shape can be unit-tested, which for values that
decide what a release is built from is the important one.

Deliberately stdlib-only. `knowledge-tick` runs on the kubectl image with
nothing but `apk add python3`, and pulling pipeline.py in here would drag
Pillow onto a job whose entire purpose is to answer one yes/no question
cheaply.
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

DOCS_SOURCE_ID = "gitlab-compshare-docs"


def pinned_docs_revision(manifest_path: Path) -> str:
    """The compshare-docs commit THIS corpus was built from.

    Read off the release manifest rather than restated in the CI job, because a
    restated revision is a second place for the truth to live and the corpus is
    the one that decides.
    """
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    for source in manifest.get("sources") or []:
        if source.get("id") == DOCS_SOURCE_ID:
            revision = str(source.get("revision") or "").strip()
            if not revision:
                raise SystemExit(f"{manifest_path}: {DOCS_SOURCE_ID} records an empty revision")
            return revision
    raise SystemExit(f"{manifest_path}: no {DOCS_SOURCE_ID} source is recorded")


def corpus_valid_from(corpus_path: Path) -> str:
    """The release date stamped on a corpus, taken from kb_version.

    NOT from valid_from. Every row in a corpus carries the same kb_version --
    load_legacy_external rewrites it even on the retained legacy slice -- while
    valid_from is genuinely mixed: the shipped external corpus holds both
    2026-06-06 (legacy) and 2026-08-14 (rebuilt). A max() over valid_from is
    right today by coincidence and wrong the first time a legacy snapshot is
    re-imported with a later date.
    """
    versions: set[str] = set()
    with corpus_path.open(encoding="utf-8-sig") as handle:
        for line in handle:
            if line.strip():
                versions.add(str(json.loads(line)["kb_version"]))
    if len(versions) != 1:
        raise SystemExit(
            f"{corpus_path}: expected one kb_version, found {len(versions)}: {sorted(versions)}")
    version = versions.pop()
    stamp = version.rsplit(".", 1)[-1]
    # kb.external.v2.2026-08-14 -> 2026-08-14. Anything else is a kb_version
    # shape this function was not written for, and guessing would hand the
    # build a --external-valid-from that silently restamps the whole corpus.
    if len(stamp) != 10 or stamp.count("-") != 2:
        raise SystemExit(f"{corpus_path}: kb_version {version!r} does not end in a YYYY-MM-DD stamp")
    return stamp


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    revision = sub.add_parser("pinned-docs-revision")
    revision.add_argument("manifest", type=Path)

    stamp = sub.add_parser("corpus-valid-from")
    stamp.add_argument("corpus", type=Path)

    args = parser.parse_args(argv)
    if args.command == "pinned-docs-revision":
        print(pinned_docs_revision(args.manifest))
    else:
        print(corpus_valid_from(args.corpus))
    return 0


if __name__ == "__main__":
    sys.exit(main())
