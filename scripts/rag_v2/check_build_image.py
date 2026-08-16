#!/usr/bin/env python3
"""Fail the BUILD IMAGE, not a release, when the interpreter contract is broken.

Run by Dockerfile.kb-build against the requirements.txt it just installed. Each
assertion here has already been a real failure mode in this pipeline:

  - a 3.13 base silently source-building Pillow, because pillow==10.3.0 ships no
    cp313 wheel. A locally compiled Pillow is not the artefact
    caption_contract_digest() attests to, so it would re-earn every animated
    caption under an unchanged contract.
  - a render dependency resolving to a version requirements.txt does not pin.
  - pypdf or pymupdf missing entirely, which extract_pdf_text turns into a hard
    raise -- halfway through a build that has already spent its VL budget.

It is a standalone script rather than an inline `RUN <<'PY'` heredoc because
heredocs in a Dockerfile are BuildKit syntax and this repo's images are built by
Kaniko; the sibling Dockerfile carries the same warning about BuildKit-only RUN
features. Stdlib plus the three packages under test, nothing else.
"""
from __future__ import annotations

import sys
from pathlib import Path

EXPECTED_PYTHON = (3, 12)


def pinned_versions(requirements: Path) -> dict[str, str]:
    pins: dict[str, str] = {}
    for line in requirements.read_text(encoding="utf-8").splitlines():
        line = line.split("#", 1)[0].strip()
        if "==" in line:
            name, _, version = line.partition("==")
            pins[name.strip().lower()] = version.strip()
    return pins


def installed_versions() -> dict[str, str]:
    import PIL
    import pypdf
    import fitz

    return {
        "pillow": PIL.__version__,
        "pypdf": pypdf.__version__,
        # VersionBind is the PyMuPDF package version (e.g. 1.24.11). Its sibling
        # fitz.version[1] is the BUNDLED MuPDF (1.24.10) and is deliberately not
        # what requirements.txt pins.
        "pymupdf": fitz.VersionBind,
    }


def main(argv: list[str] | None = None) -> int:
    arguments = list(sys.argv[1:] if argv is None else argv)
    requirements = Path(arguments[0]) if arguments else Path("scripts/rag_v2/requirements.txt")

    if sys.version_info[:2] != EXPECTED_PYTHON:
        running = ".".join(str(part) for part in sys.version_info[:3])
        raise SystemExit(
            f"interpreter is {running}, want {EXPECTED_PYTHON[0]}.{EXPECTED_PYTHON[1]}.x. "
            "pillow==10.3.0 publishes no cp313 wheel, and a source build renders a "
            "different contact sheet than the caption contract attests to.")

    pins = pinned_versions(requirements)
    if not pins:
        raise SystemExit(f"{requirements} pins nothing; this check would assert nothing")
    installed = installed_versions()
    for name, want in sorted(pins.items()):
        if name not in installed:
            raise SystemExit(f"{requirements} pins {name}, which this check does not know how to read")
        if installed[name] != want:
            raise SystemExit(f"{name} resolved to {installed[name]}, {requirements} pins {want}")
    print(f"interpreter {sys.version.split()[0]} and pinned render dependencies match: {installed}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
