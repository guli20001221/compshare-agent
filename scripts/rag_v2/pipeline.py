from __future__ import annotations

import base64
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import asdict, dataclass, field, replace
from datetime import date, datetime, timezone
import hashlib
import json
import mimetypes
from pathlib import Path
import re
import shutil
import tempfile
import time
from typing import Any, Iterable
from urllib.parse import quote, unquote, urlparse
from urllib.request import Request, urlopen
import zipfile


MAX_CONTENT_RUNES = 4000
TARGET_CONTENT_RUNES = 3400
IMAGE_RE = re.compile(r"!\[([^\]]*)\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
FLEX_IMAGE_RE = re.compile(r"!\[([^\]\n]{0,500})\]\(\s*([^\s)]+)(?:\s+\"[^\"\n]*\")?\s*\)")
GENERAL_IMAGE_RE = re.compile(
    r"!\[([^\]\n]{0,500})\]\(\s*(?:<([^>\n]{1,2000})>|([^\n)]{1,2000}))\s*\)"
)
HTML_IMAGE_RE = re.compile(r"<img\b[^>]*>", re.IGNORECASE | re.DOTALL)
HTML_ATTR_RE = re.compile(r"\b(src|alt)\s*=\s*([\"'])(.*?)\2", re.IGNORECASE | re.DOTALL)
DIRECT_IMAGE_LINK_RE = re.compile(
    r"(?<!!)\[([^\]\n]{1,500})\]\((https?://[^\s)]+\.(?:png|jpe?g|gif|webp|bmp|tiff?)(?:\?[^\s)]*)?)\)",
    re.IGNORECASE,
)
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$")
FRONT_MATTER_RE = re.compile(r"\A---\s*\n.*?\n---\s*\n", re.DOTALL)
HTML_NOISE_RE = re.compile(r"<(?:script|style)\b.*?</(?:script|style)>", re.DOTALL | re.IGNORECASE)
HTML_COMMENT_RE = re.compile(r"<!--.*?-->", re.DOTALL)
SPACE_RE = re.compile(r"[ \t]+")
BLANK_RE = re.compile(r"\n{3,}")

# MDX_LAYOUT_COMPONENTS are the compshare-docs presentation components whose TAGS
# carry no information but whose CHILDREN are the documentation — <ApiFieldTable>
# wraps the request/response parameter tables. Only the tags are removed; the body
# between them is kept verbatim.
#
# This is an allowlist of names and deliberately not a general JSX pattern,
# because the corpus is full of angle-bracket placeholders that a pattern would
# eat: `ssh -p <ExternalPort> root@<ExternalHost>`, `<UModelverse_API_KEY>`,
# `<YOUR_API_KEY>`, and `<EOF` in heredocs. Some of those are PascalCase too, so
# even "looks like a component name" is not a safe discriminator.
MDX_LAYOUT_COMPONENTS = ("ApiEndpoint", "ApiFieldTable")
MDX_LAYOUT_TAG_RE = re.compile(
    r"</?(?:" + "|".join(MDX_LAYOUT_COMPONENTS) + r")\b[^>]*/?>"
)

# UNKNOWN_MDX_BLOCK_RE finds a line that is nothing but one capitalized tag.
# MDX requires a block-level component to sit alone on its line for the markdown
# around it to keep parsing, while every placeholder above appears inline, inside
# a code span or command. A match that is not in the allowlist is a component
# added to the docs after this pipeline last looked, and the build reports it
# rather than letting its tag text flow into a chunk unnoticed.
UNKNOWN_MDX_BLOCK_RE = re.compile(r"^\s*(</?([A-Z][A-Za-z0-9]*)\b[^>]*/?>)\s*$")


@dataclass(frozen=True)
class SourceDocument:
    source_id: str
    source_path: str
    source_kind: str
    source_origin: str
    title: str
    text: str
    surface_url: str | None
    root: Path
    absolute_path: Path
    compatibility: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class AssetNote:
    asset_id: str
    source_path: str
    repo_path: str | None
    public_url: str
    description: str
    visible_text: list[str]
    controls: list[str]
    relations: list[str]
    confidence: float
    model: str
    visual_type: str = "content"
    include_in_rag: bool = True


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def normalized_file_digest(path: Path) -> str:
    return sha256_bytes(path.read_bytes().replace(b"\r\n", b"\n"))


def _retryable_asset_failure(item: dict[str, Any]) -> bool:
    reason = str(item.get("reason") or "")
    if reason.startswith("source_remote_image_unavailable:"):
        return any(marker in reason for marker in ("Timeout", "HTTP Error 429", "HTTP Error 5", "URLError", "SSL"))
    if not reason.startswith("vl_failed:"):
        return False
    return True


def _canonical_remote_image_url(value: str) -> str:
    parsed = urlparse(value)
    if (parsed.hostname or "").lower() != "github.com":
        return value
    parts = [part for part in parsed.path.split("/") if part]
    if len(parts) >= 5 and parts[2] in {"blob", "raw"}:
        owner, repo, _mode, revision, *asset_path = parts
        return f"https://raw.githubusercontent.com/{owner}/{repo}/{revision}/{'/'.join(asset_path)}"
    return value


def _is_decorative_asset(alt: str, ref: str) -> bool:
    parsed = urlparse(ref)
    host = (parsed.hostname or "").lower()
    path = parsed.path.lower()
    if host == "camo.githubusercontent.com":
        try:
            decoded_target = bytes.fromhex(path.rsplit("/", 1)[-1]).decode("utf-8")
        except (ValueError, UnicodeDecodeError):
            decoded_target = ""
        if decoded_target and _is_decorative_asset(alt, decoded_target):
            return True
    if host in {
        "img.shields.io", "badge.fury.io", "counter.seku.su", "contrib.rocks",
        "reporoster.com", "trendshift.io", "api.star-history.com",
    }:
        return True
    if host == "colab.research.google.com" and path.endswith("/assets/colab-badge.svg"):
        return True
    if host == "camo.githubusercontent.com" and any(token in alt.lower() for token in ("badge", "license", "version", "stars")):
        return True
    return path.endswith(("/logo.svg", "/next.svg", "/bot.svg"))


def _image_content_type(data: bytes, declared: str, source_url: str) -> str | None:
    if declared.startswith("image/"):
        return declared
    if data.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if data.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if data.startswith((b"GIF87a", b"GIF89a")):
        return "image/gif"
    if len(data) >= 12 and data.startswith(b"RIFF") and data[8:12] == b"WEBP":
        return "image/webp"
    stripped = data.lstrip()[:256].lower()
    if stripped.startswith((b"<svg", b"<?xml")) and b"<svg" in data[:4096].lower():
        return "image/svg+xml"
    guessed = mimetypes.guess_type(urlparse(source_url).path)[0]
    return guessed if guessed and guessed.startswith("image/") else None


def _prepare_vl_image(path: Path, cache_dir: Path) -> Path:
    if path.suffix.lower() not in {".gif", ".webp"}:
        return path
    try:
        from PIL import Image
    except ImportError:
        return path
    with Image.open(path) as source:
        frame_count = int(getattr(source, "n_frames", 1))
        if frame_count <= 1:
            return path
        sample_count = min(4, frame_count)
        frame_indexes = sorted({round(index * (frame_count - 1) / max(1, sample_count - 1)) for index in range(sample_count)})
        frames = []
        for frame_index in frame_indexes:
            source.seek(frame_index)
            frame = source.convert("RGBA")
            frame.thumbnail((1280, 1280))
            background = Image.new("RGB", frame.size, "white")
            background.paste(frame, mask=frame.getchannel("A"))
            frames.append(background)
        width = max(frame.width for frame in frames)
        height = sum(frame.height for frame in frames)
        contact_sheet = Image.new("RGB", (width, height), "white")
        offset = 0
        for frame in frames:
            contact_sheet.paste(frame, (0, offset))
            offset += frame.height
        cache_dir.mkdir(parents=True, exist_ok=True)
        target = cache_dir / f"{sha256_file(path)[:24]}-frames.png"
        if not target.exists():
            contact_sheet.save(target, format="PNG", optimize=True)
        return target


def tree_lock(path: Path) -> dict[str, Any]:
    files = sorted(item for item in path.rglob("*") if item.is_file() and ".git" not in item.parts)
    h = hashlib.sha256()
    total = 0
    for item in files:
        rel = item.relative_to(path).as_posix()
        digest = sha256_file(item)
        size = item.stat().st_size
        total += size
        h.update(rel.encode("utf-8"))
        h.update(b"\0")
        h.update(digest.encode("ascii"))
        h.update(b"\0")
    return {"file_count": len(files), "byte_count": total, "sha256": h.hexdigest()}


def load_env(path: Path | None) -> dict[str, str]:
    values: dict[str, str] = {}
    if path and path.exists():
        for raw in path.read_text(encoding="utf-8-sig").splitlines():
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            values[key.strip()] = value.strip().strip('"').strip("'")
    return values


def safe_extract_zip(zip_path: Path, destination: Path) -> None:
    destination.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(zip_path) as archive:
        for info in archive.infolist():
            name = info.filename.replace("\\", "/")
            target = (destination / name).resolve()
            if destination.resolve() not in target.parents and target != destination.resolve():
                raise ValueError(f"unsafe zip member: {info.filename}")
            if info.is_dir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(info) as src, target.open("wb") as dst:
                shutil.copyfileobj(src, dst)


def clean_public_text(text: str) -> str:
    """Remove layout noise without redacting or rewriting public source text."""
    text = text.replace("\ufeff", "").replace("\r\n", "\n").replace("\r", "\n")
    text = FRONT_MATTER_RE.sub("", text)
    text = HTML_NOISE_RE.sub("", text)
    text = HTML_COMMENT_RE.sub("", text)
    text = MDX_LAYOUT_TAG_RE.sub("", text)
    text = normalize_image_markup(text)
    lines = [SPACE_RE.sub(" ", line).rstrip() for line in text.splitlines()]
    return BLANK_RE.sub("\n\n", "\n".join(lines)).strip() + "\n"


def unknown_mdx_components(text: str) -> set[str]:
    """Report block-level MDX components this pipeline does not know about.

    Called on the RAW document, before clean_public_text removes the allowlisted
    tags, so a newly introduced component surfaces as a build signal instead of
    as tag text inside a chunk.
    """
    found: set[str] = set()
    for line in text.splitlines():
        match = UNKNOWN_MDX_BLOCK_RE.match(line)
        if match and match.group(2) not in MDX_LAYOUT_COMPONENTS:
            found.add(match.group(2))
    return found


def normalize_image_markup(text: str) -> str:
    """Normalize supported image syntaxes before VL discovery."""
    def html_image(match: re.Match[str]) -> str:
        attrs = {name.lower(): value.strip() for name, _quote, value in HTML_ATTR_RE.findall(match.group(0))}
        src = attrs.get("src", "")
        return f"![{attrs.get('alt', '')}]({src})" if src else ""

    def markdown_image(match: re.Match[str]) -> str:
        alt = match.group(1).strip()
        ref = (match.group(2) or match.group(3) or "").strip()
        # Tolerate invalid exports containing literal spaces or non-ASCII
        # characters in image targets, then hand a canonical target to IMAGE_RE.
        if ref.endswith('"') and ' "' in ref:
            ref = ref.rsplit(' "', 1)[0].strip()
        # Keep already-valid references byte-stable so incremental builds can
        # reuse the prior VL lock. Literal spaces are the only characters that
        # prevent IMAGE_RE from consuming the normalized target.
        ref = ref.replace(" ", "%20")
        return f"![{alt}]({ref})"

    text = HTML_IMAGE_RE.sub(html_image, text)
    text = GENERAL_IMAGE_RE.sub(markdown_image, text)
    text = FLEX_IMAGE_RE.sub(lambda match: f"![{match.group(1).strip()}]({match.group(2).strip()})", text)
    return DIRECT_IMAGE_LINK_RE.sub(lambda match: f"![{match.group(1).strip()}]({match.group(2).strip()})", text)


def markdown_title(text: str, fallback: str) -> str:
    for line in text.splitlines():
        match = HEADING_RE.match(line.strip())
        if match:
            return match.group(2).strip()
    return fallback.replace("-", " ").replace("_", " ").strip()


# INTERNAL_DOC_ROOTS are the compshare-docs directories that hold published
# documentation. "content/" is the Next.js App Router layout the site migrated to
# (revision 0cd491da); "pages/" is the Nextra layout it migrated from, kept so an
# older pinned revision still builds.
#
# public/action_md/ is deliberately NOT here. Its eight API markdown files are
# referenced by nothing in the repository at either revision, they duplicate
# content/gpus/instance/*.mdx, and the URL this module derived for them
# (/docs/gpus/action/<CamelCase>) answers 404 on the live site while the
# lowercase content/ pages answer 200 — so every chunk built from them carried a
# dead citation for content that was already in the corpus twice.
INTERNAL_DOC_ROOTS = ("content/", "pages/")

# Documentation extensions. .mdx is load-bearing: at revision 0cd491da the
# migration rewrote every API reference page to MDX, so 118 of 232 documents
# would be invisible to a .md-only scan.
DOC_SUFFIXES = (".md", ".mdx")


def public_docs_url(relative: str) -> str | None:
    rel = relative.replace("\\", "/")
    for root in INTERNAL_DOC_ROOTS:
        if rel.startswith(root):
            rel = rel[len(root) :]
            break
    else:
        return None
    for suffix in DOC_SUFFIXES:
        if rel.lower().endswith(suffix):
            rel = rel[: -len(suffix)]
            break
    return "https://www.compshare.cn/docs/" + rel.strip("/")


def collect_internal_docs(root: Path) -> list[SourceDocument]:
    docs: list[SourceDocument] = []
    candidates = sorted(
        path for suffix in DOC_SUFFIXES for path in root.rglob(f"*{suffix}")
    )
    unknown_components: dict[str, str] = {}
    for path in candidates:
        rel = path.relative_to(root).as_posix()
        if rel.startswith(".git/") or rel in {"README.md", "CONSOLE_DOCS_AUDIT.md"}:
            continue
        if not rel.startswith(INTERNAL_DOC_ROOTS):
            continue
        raw = path.read_text(encoding="utf-8", errors="replace")
        for name in unknown_mdx_components(raw):
            unknown_components.setdefault(name, rel)
        text = clean_public_text(raw)
        if len(text.strip()) < 40:
            continue
        docs.append(SourceDocument(
            source_id="gitlab-compshare-docs",
            source_path=rel,
            source_kind="platform_public_doc",
            source_origin="official",
            title=markdown_title(text, path.stem),
            text=text,
            surface_url=public_docs_url(rel),
            root=root,
            absolute_path=path,
        ))
    if unknown_components:
        listed = ", ".join(f"<{name}> ({rel})" for name, rel in sorted(unknown_components.items()))
        raise ValueError(
            "compshare-docs introduced MDX block components this pipeline does not "
            f"classify: {listed}. Decide per component whether its tags are layout "
            "(add to MDX_LAYOUT_COMPONENTS so only the tags are dropped and the body "
            "is kept) or content, then rebuild. Failing closed here is deliberate: "
            "the alternative is tag text flowing into chunks and being embedded, "
            "retrieved and cited as if it were documentation."
        )
    return docs


def collect_faq_docs(root: Path, source_id: str) -> list[SourceDocument]:
    docs: list[SourceDocument] = []
    for path in sorted(root.rglob("*.md")):
        text = clean_public_text(path.read_text(encoding="utf-8", errors="replace"))
        docs.append(SourceDocument(
            source_id=source_id,
            source_path=path.relative_to(root).as_posix(),
            source_kind="public_faq_export",
            source_origin="official",
            title=markdown_title(text, path.stem),
            text=text,
            surface_url=None,
            root=root,
            absolute_path=path,
        ))
    for path in sorted(root.rglob("*.pdf")):
        text = extract_pdf_text(path)
        if text.strip():
            docs.append(SourceDocument(
                source_id=source_id,
                source_path=path.relative_to(root).as_posix(),
                source_kind="public_pdf",
                source_origin="official",
                title=path.stem,
                text=text,
                surface_url=None,
                root=root,
                absolute_path=path,
            ))
    return docs


def extract_pdf_text(path: Path) -> str:
    try:
        from pypdf import PdfReader
    except ImportError as exc:  # pragma: no cover
        raise RuntimeError("pypdf is required for PDF extraction") from exc
    reader = PdfReader(str(path))
    pages: list[str] = [f"# {path.stem}"]
    rendered_dir = path.parent / ".rag_v2_pdf_pages"
    try:
        import fitz

        pdf = fitz.open(str(path))
        rendered_dir.mkdir(exist_ok=True)
        rendered_pages = []
        for index, pdf_page in enumerate(pdf, start=1):
            target = rendered_dir / f"{path.stem}-page-{index}.png"
            target.write_bytes(pdf_page.get_pixmap(matrix=fitz.Matrix(2, 2), alpha=False).tobytes("png"))
            rendered_pages.append(target)
        pdf.close()
    except Exception as exc:  # pragma: no cover - release validation reports missing renderer
        raise RuntimeError(f"PDF page rendering failed for {path}: {exc}") from exc
    for index, page in enumerate(reader.pages, start=1):
        text = (page.extract_text() or "").strip()
        image_ref = rendered_pages[index - 1].relative_to(path.parent).as_posix()
        pages.extend([
            f"## 第 {index} 页",
            text or "[该页无可提取文字，以页面图像识别结果为准]",
            f"![{path.stem} 第 {index} 页]({image_ref})",
        ])
    return clean_public_text("\n\n".join(pages))


def resolve_local_asset(doc: SourceDocument, decoded_ref: str) -> Path | None:
    """Map an image reference in a document to a file on disk.

    A leading slash means the site root, not the filesystem root. Next.js serves
    everything under public/ at the site root, so /foo.png IS public/foo.png —
    that is the framework's contract, not a guess about this repo's layout.

    compshare-docs used to write these references as literal repo-relative paths
    (`![](public/sysdisk_step1.jpg)`), which the root candidate resolved. The App
    Router migration rewrote all of them into the site-root form
    (`![](/sysdisk_step1.jpg)`), and 14 required images in content/operation/
    stopped resolving — enough to fail the build outright, since a missing image
    on an internal doc is an error, not a warning.

    A site-root reference is also never document-relative, so the parent-relative
    candidate is skipped for it. On Windows `Path("F:/a/b") / "/c.png"` discards
    the base and resolves against the current drive, which could silently match
    an unrelated file at the drive root and feed its VL description into the
    corpus as if it were the documented screenshot.
    """
    for candidate in local_asset_candidates(doc, decoded_ref):
        if candidate.is_file():
            return candidate.resolve()
    return None


def local_asset_candidates(doc: SourceDocument, decoded_ref: str) -> list[Path]:
    """The paths resolve_local_asset will try, in order.

    Split out so the candidate SET is testable on its own. Whether the
    parent-relative candidate is present for a site-root reference cannot be
    asserted through the return value — it only differs when a file happens to
    exist at the drive root, which a test must not create.
    """
    site_root_ref = decoded_ref.startswith("/")
    candidates: list[Path] = []
    if not site_root_ref:
        candidates.append((doc.absolute_path.parent / decoded_ref).resolve())
    candidates.append(doc.root / decoded_ref.lstrip("/"))
    if site_root_ref:
        candidates.append(doc.root / "public" / decoded_ref.lstrip("/"))
    return candidates


EXTERNAL_EXCLUDED_PATH_PARTS = {
    ".github", "issue_template", "pull_request_template", "contributing.md",
    "security.md", "changelog.md", "license.md", "node_db", "typings",
}


def _external_entry_included(package: str, entry: dict[str, Any]) -> bool:
    path = str(entry.get("path") or "").replace("\\", "/")
    low = path.lower()
    if not low.endswith((".md", ".mdx")):
        return False
    if any(part in low for part in EXTERNAL_EXCLUDED_PATH_PARTS):
        return False
    source_type = str(entry.get("source_type") or "")
    if package == "comfyui":
        if source_type in {"workflow_blueprint", "mirror_workflow"}:
            return False
        if source_type == "installed_custom_node":
            return low.endswith(("/readme.md", "/readme.zh_cn.md", "/readme_zh.md"))
        return source_type in {
            "official_tutorial", "official_interface", "community_tutorial",
            "runtime_inventory", "mirror_image_page", "mirror_image_readme",
        }
    if package == "digital-human":
        return source_type in {"runtime_inventory", "installed_app_markdown", "image_source"}
    return str(entry.get("category") or "") not in {"包说明", "版本记录"}


def collect_external_docs(root: Path, package: str) -> tuple[list[SourceDocument], dict[str, Any]]:
    manifest_path = root / "manifest.json"
    if not manifest_path.exists():
        candidates = list(root.rglob("manifest.json"))
        if len(candidates) != 1:
            raise ValueError(f"{package}: expected one manifest.json")
        manifest_path = candidates[0]
        root = manifest_path.parent
    manifest = json.loads(manifest_path.read_text(encoding="utf-8-sig"))
    docs: list[SourceDocument] = []
    excluded = 0
    missing = 0
    for entry in manifest.get("files") or []:
        if not _external_entry_included(package, entry):
            excluded += 1
            continue
        rel = str(entry.get("path") or "").replace("\\", "/")
        path = root / rel
        if not path.exists():
            missing += 1
            continue
        text = clean_public_text(path.read_text(encoding="utf-8", errors="replace"))
        if len(text.strip()) < 40:
            excluded += 1
            continue
        # A source snapshot that already contains a redaction placeholder has
        # irreversibly lost information. V2 excludes it instead of carrying the
        # placeholder forward or attempting to guess the missing value.
        if "REDACTED]" in text:
            excluded += 1
            continue
        source_type = str(entry.get("source_type") or entry.get("category") or "external_doc")
        is_community = source_type == "community_tutorial"
        docs.append(SourceDocument(
            source_id=f"external-{package}",
            source_path=rel,
            source_kind=source_type,
            source_origin="external_community" if is_community else "external_official",
            title=str(entry.get("title") or markdown_title(text, path.stem)),
            text=text,
            surface_url=_safe_external_url(entry.get("source_url")),
            root=root,
            absolute_path=path,
            compatibility=str(entry.get("compatibility") or ""),
            metadata={key: entry.get(key) for key in ("image", "scope", "category") if entry.get(key)},
        ))
    return docs, {"manifest_entries": len(manifest.get("files") or []), "included": len(docs), "excluded": excluded, "missing": missing}


def _safe_external_url(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    parsed = urlparse(value.strip())
    if parsed.scheme != "https" or not parsed.netloc:
        return None
    host = (parsed.hostname or "").lower()
    if "gitlab" in host or host.endswith(".feishu.cn") or host.endswith(".lark.com"):
        return None
    if host not in {"www.compshare.cn", "compshare.cn", "console.compshare.cn"}:
        return None
    if host in {"www.compshare.cn", "compshare.cn"} and not parsed.path.startswith("/docs/"):
        return None
    return value.strip()


class ModelVerseClient:
    def __init__(self, *, base_url: str, api_key: str, cache_dir: Path) -> None:
        if not api_key:
            raise ValueError("MODELVERSE_API_KEY is required")
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.cache_dir = cache_dir
        self.cache_dir.mkdir(parents=True, exist_ok=True)

    def _cache_path(self, *, model: str, prompt: str, image: Path | str | None) -> Path:
        image_key = ""
        if isinstance(image, Path):
            image_key = sha256_file(image)
        elif isinstance(image, str):
            image_key = image
        cache_key = sha256_bytes(json.dumps({"model": model, "prompt": prompt, "image": image_key}, ensure_ascii=False, sort_keys=True).encode("utf-8"))
        return self.cache_dir / f"{cache_key}.json"

    def cached_json(self, *, model: str, prompt: str, image: Path | str | None = None) -> dict[str, Any] | None:
        cache_path = self._cache_path(model=model, prompt=prompt, image=image)
        if cache_path.exists():
            return json.loads(cache_path.read_text(encoding="utf-8"))["payload"]
        return None

    def json_chat(
        self,
        *,
        model: str,
        prompt: str,
        image: Path | str | None = None,
        max_tokens: int = 1200,
        timeout: int = 240,
        retries: int = 3,
    ) -> dict[str, Any]:
        cache_path = self._cache_path(model=model, prompt=prompt, image=image)
        cached = self.cached_json(model=model, prompt=prompt, image=image)
        if cached is not None:
            return cached
        content: Any = prompt
        if image is not None:
            if isinstance(image, Path):
                mime = mimetypes.guess_type(image.name)[0] or "image/png"
                encoded = base64.b64encode(image.read_bytes()).decode("ascii")
                image_url = f"data:{mime};base64,{encoded}"
            else:
                image_url = image
            content = [
                {"type": "text", "text": prompt},
                {"type": "image_url", "image_url": {"url": image_url}},
            ]
        body = json.dumps({
            "model": model,
            "messages": [{"role": "user", "content": content}],
            "temperature": 0,
            "max_tokens": max_tokens,
            "response_format": {"type": "json_object"},
        }, ensure_ascii=False).encode("utf-8")
        request = Request(
            self.base_url + "/chat/completions",
            data=body,
            headers={"Authorization": f"Bearer {self.api_key}", "Content-Type": "application/json"},
            method="POST",
        )
        last_error: Exception | None = None
        for attempt in range(retries + 1):
            try:
                with urlopen(request, timeout=timeout) as response:
                    raw = json.loads(response.read().decode("utf-8"))
                break
            except Exception as exc:  # noqa: BLE001 - retried then surfaced to release gate
                last_error = exc
                if attempt == retries:
                    raise
                time.sleep(2 ** attempt)
        else:  # pragma: no cover
            raise RuntimeError("ModelVerse request failed") from last_error
        content_raw = raw["choices"][0]["message"]["content"]
        payload = json.loads(content_raw)
        cache_path.write_text(json.dumps({"payload": payload}, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        return payload


def describe_assets(
    documents: list[SourceDocument],
    *,
    client: ModelVerseClient,
    model: str,
    fallback_model: str | None,
    assets_dir: Path,
    raw_asset_base_url: str,
    workers: int = 8,
    reuse_existing_notes: bool = False,
) -> tuple[dict[tuple[str, str, str], AssetNote], list[dict[str, Any]]]:
    notes: dict[tuple[str, str, str], AssetNote] = {}
    failures: list[dict[str, Any]] = []
    assets_dir.mkdir(parents=True, exist_ok=True)
    task_groups: dict[str, list[tuple[SourceDocument, str, str]]] = {}
    for doc in documents:
        for alt, ref in IMAGE_RE.findall(doc.text):
            decoded = unquote(ref).replace("\\", "/")
            if decoded.startswith(("https://", "http://")):
                identity = "url:" + decoded
            else:
                candidate = resolve_local_asset(doc, decoded)
                identity = "file:" + sha256_file(candidate) if candidate else f"missing:{doc.source_id}:{doc.source_path}:{decoded}"
            task_groups.setdefault(identity, []).append((doc, alt, ref))

    lock_path = assets_dir.parent / "asset_lock.json"
    lock_fingerprint = sha256_bytes(json.dumps({
        "processor": "caption-only-v3",
        "model": model,
        "fallback_model": fallback_model,
        "references": sorted(
            (doc.source_id, doc.source_path, alt, ref)
            for aliases in task_groups.values()
            for doc, alt, ref in aliases
        ),
    }, ensure_ascii=False, sort_keys=True).encode("utf-8"))
    reusable_notes: dict[tuple[str, str, str], AssetNote] = {}
    reusable_failures: dict[tuple[str, str, str], dict[str, Any]] = {}
    reusable_failures_by_path: dict[tuple[str, str], dict[str, Any]] = {}
    if lock_path.exists():
        try:
            locked = json.loads(lock_path.read_text(encoding="utf-8"))
            locked_failures = list(locked.get("failures") or [])
            exact_lock = locked.get("fingerprint") == lock_fingerprint
            if exact_lock and not any(_retryable_asset_failure(item) for item in locked_failures):
                for item in locked.get("notes") or []:
                    note_fields = {key: value for key, value in item.items() if key not in {"source_id", "ref"}}
                    notes[(str(item["source_id"]), str(item["source_path"]), str(item["ref"]))] = AssetNote(**note_fields)
                return notes, locked_failures
            if exact_lock or reuse_existing_notes:
                for item in locked.get("notes") or []:
                    note_fields = {key: value for key, value in item.items() if key not in {"source_id", "ref"}}
                    note = AssetNote(**note_fields)
                    if note.model in {model, fallback_model, "deterministic-decoration-filter"}:
                        reusable_notes[(str(item["source_id"]), str(item["source_path"]), str(item["ref"]))] = note
                for item in locked_failures:
                    if _retryable_asset_failure(item):
                        continue
                    failure_key = (
                        str(item.get("source_id") or ""),
                        str(item.get("source") or ""),
                        str(item.get("ref") or ""),
                    )
                    reusable_failures[failure_key] = dict(item)
                    reusable_failures_by_path[(failure_key[1], failure_key[2])] = dict(item)
        except (OSError, ValueError, TypeError, KeyError):
            notes = {}

    def process(task: tuple[SourceDocument, str, str]) -> tuple[tuple[str, str, str], AssetNote | None, dict[str, Any] | None]:
        doc, alt, ref = task
        key = (doc.source_id, doc.source_path, ref)
        try:
            decoded = unquote(ref).replace("\\", "/")
            if _is_decorative_asset(alt, decoded):
                return key, AssetNote(
                    asset_id="decorative-" + sha256_bytes(decoded.encode("utf-8"))[:16],
                    source_path=doc.source_path,
                    repo_path=None,
                    public_url=decoded if decoded.startswith("https://") else "",
                    description="decorative asset",
                    visible_text=[],
                    controls=[],
                    relations=[],
                    confidence=1.0,
                    model="deterministic-decoration-filter",
                    visual_type="decorative",
                    include_in_rag=False,
                ), None
            image_input: Path | str
            repo_path: str | None = None
            legacy_image_url: str | None = None
            if decoded.startswith(("https://", "http://")):
                if not decoded.startswith("https://"):
                    severity = "warning" if doc.source_origin.startswith("external_") else "error"
                    return key, None, {"source": doc.source_path, "ref": ref, "reason": "non_https_image", "severity": severity}
                try:
                    remote_cache_dir = client.cache_dir.parent / "remote-assets"
                    remote_cache_dir.mkdir(parents=True, exist_ok=True)
                    url_map_dir = remote_cache_dir / "by-url"
                    url_map_dir.mkdir(exist_ok=True)
                    url_map = url_map_dir / f"{sha256_bytes(decoded.encode('utf-8'))}.json"
                    cached_remote: Path | None = None
                    content_type = "application/octet-stream"
                    if url_map.exists():
                        mapping = json.loads(url_map.read_text(encoding="utf-8"))
                        candidate = remote_cache_dir / str(mapping["filename"])
                        if candidate.is_file():
                            cached_remote = candidate
                            content_type = str(mapping["content_type"])
                    if cached_remote is None:
                        last_download_error: Exception | None = None
                        for download_attempt in range(3):
                            try:
                                request_url = quote(_canonical_remote_image_url(decoded), safe="/:?&=%#@+;,[]!$'()*")
                                request = Request(request_url, headers={"User-Agent": "compshare-rag-v2/1.0"})
                                with urlopen(request, timeout=10) as response:
                                    data = response.read(20 * 1024 * 1024 + 1)
                                    content_type = response.headers.get_content_type()
                                break
                            except Exception as exc:
                                last_download_error = exc
                                if download_attempt == 2:
                                    raise
                                time.sleep(2 ** download_attempt)
                    else:
                        data = cached_remote.read_bytes()
                    if len(data) > 20 * 1024 * 1024:
                        raise ValueError("remote image exceeds 20 MiB")
                    detected_content_type = _image_content_type(data, content_type, decoded)
                    if detected_content_type is None:
                        raise ValueError(f"remote asset is not an image: {content_type}")
                    content_type = detected_content_type
                except Exception as exc:  # source defect, not a preprocessing defect
                    return key, None, {
                        "source": doc.source_path,
                        "ref": ref,
                        "reason": f"source_remote_image_unavailable:{type(exc).__name__}:{exc}",
                        "severity": "warning",
                    }
                digest = sha256_bytes(data)
                suffix = mimetypes.guess_extension(content_type) or Path(urlparse(decoded).path).suffix.lower() or ".img"
                target = remote_cache_dir / f"{digest[:24]}{suffix}"
                if not target.exists():
                    target.write_bytes(data)
                url_map.write_text(
                    json.dumps({"filename": target.name, "content_type": content_type}, sort_keys=True) + "\n",
                    encoding="utf-8",
                )
                public_url = decoded
                asset_id = "remote-" + digest[:16]
                image_input = target
                legacy_image_url = decoded
            else:
                candidate = resolve_local_asset(doc, decoded)
                if candidate is None:
                    severity = "warning" if doc.source_origin.startswith("external_") else "error"
                    return key, None, {"source": doc.source_path, "ref": ref, "reason": "missing_local_image", "severity": severity}
                digest = sha256_file(candidate)
                suffix = candidate.suffix.lower() or ".bin"
                target = assets_dir / f"{digest[:24]}{suffix}"
                if not target.exists():
                    shutil.copy2(candidate, target)
                repo_path = target.as_posix()
                public_url = raw_asset_base_url.rstrip("/") + "/" + target.name
                asset_id = "asset-" + digest[:16]
                image_input = candidate
            prompt = (
                "识别这张公开文档图片。禁止猜测不可见或打码内容。只输出 JSON，严格包含 "
                "description(string)、visible_text(array of strings)、controls(array of strings)、"
                "relations(array of strings)、confidence(number)、visual_type(string)、include_in_rag(boolean)。"
                "保留命令、路径、错误码、模型名、数字、单位和按钮原文。二维码、群聊码、纯装饰图和无信息图设置 include_in_rag=false。"
            )
            if isinstance(image_input, Path):
                image_input = _prepare_vl_image(image_input, client.cache_dir.parent / "vl-ready")
            used_model = model
            try:
                payload = client.cached_json(model=model, prompt=prompt, image=legacy_image_url) if legacy_image_url else None
                if payload is None:
                    payload = client.json_chat(model=model, prompt=prompt, image=image_input, max_tokens=4000)
            except Exception:
                if not fallback_model:
                    raise
                fallback_prompt = prompt + " 输出务必精炼：description 不超过200字，数组各不超过20项、每项不超过100字。"
                payload = client.json_chat(
                    model=fallback_model,
                    prompt=fallback_prompt,
                    image=image_input,
                    max_tokens=2500,
                )
                used_model = fallback_model
            required = {"description", "visible_text", "controls", "relations", "confidence", "visual_type", "include_in_rag"}
            if set(payload) != required:
                raise ValueError("VL response schema mismatch")
            note = AssetNote(
                asset_id=asset_id,
                source_path=doc.source_path,
                repo_path=repo_path,
                public_url=public_url,
                description=str(payload["description"]).strip() or (alt or "文档图片"),
                visible_text=[str(item).strip() for item in payload["visible_text"] if str(item).strip()],
                controls=[str(item).strip() for item in payload["controls"] if str(item).strip()],
                relations=[str(item).strip() for item in payload["relations"] if str(item).strip()],
                confidence=float(payload["confidence"]),
                model=used_model,
                visual_type=str(payload["visual_type"]).strip() or "content",
                include_in_rag=bool(payload["include_in_rag"]),
            )
            return key, note, None
        except Exception as exc:  # noqa: BLE001 - failure is captured as release gate
            severity = "warning" if doc.source_origin.startswith("external_") else "error"
            return key, None, {"source": doc.source_path, "ref": ref, "reason": f"vl_failed:{type(exc).__name__}:{exc}", "severity": severity}

    pending_groups: list[list[tuple[SourceDocument, str, str]]] = []
    for aliases in task_groups.values():
        first_doc, _first_alt, first_ref = aliases[0]
        reusable = reusable_notes.get((first_doc.source_id, first_doc.source_path, first_ref))
        if reusable is not None:
            for alias_doc, _alias_alt, alias_ref in aliases:
                notes[(alias_doc.source_id, alias_doc.source_path, alias_ref)] = replace(reusable, source_path=alias_doc.source_path)
            continue
        reusable_failure = reusable_failures.get((first_doc.source_id, first_doc.source_path, first_ref))
        if reusable_failure is None:
            reusable_failure = reusable_failures_by_path.get((first_doc.source_path, first_ref))
        # Internal/public-platform images are mandatory. Never preserve a prior
        # failure for them: retry it and let the release gate block if it still
        # cannot be described. Missing images in third-party snapshots remain
        # non-blocking source warnings.
        if reusable_failure is not None and all(alias_doc.source_origin.startswith("external_") for alias_doc, _alt, _ref in aliases):
            for alias_doc, _alias_alt, alias_ref in aliases:
                failures.append({
                    **reusable_failure,
                    "source_id": alias_doc.source_id,
                    "source": alias_doc.source_path,
                    "ref": alias_ref,
                    "severity": "warning",
                })
            continue
        pending_groups.append(aliases)

    if reuse_existing_notes:
        print(f"asset_groups={len(task_groups)} reused={len(task_groups) - len(pending_groups)} pending_vl={len(pending_groups)}", flush=True)

    with ThreadPoolExecutor(max_workers=max(1, workers)) as executor:
        pending = {executor.submit(process, aliases[0]): aliases for aliases in pending_groups}
        for future in as_completed(pending):
            aliases = pending[future]
            key, note, failure = future.result()
            if note is not None:
                for alias_doc, _alias_alt, alias_ref in aliases:
                    notes[(alias_doc.source_id, alias_doc.source_path, alias_ref)] = replace(note, source_path=alias_doc.source_path)
            if failure is not None:
                for alias_doc, _alias_alt, alias_ref in aliases:
                    failures.append({
                        **failure,
                        "source_id": alias_doc.source_id,
                        "source": alias_doc.source_path,
                        "ref": alias_ref,
                        "severity": "warning" if alias_doc.source_origin.startswith("external_") else "error",
                    })
    locked_notes = []
    for (source_id, source_path, ref), note in sorted(notes.items()):
        locked_notes.append({"source_id": source_id, "source_path": source_path, "ref": ref, **asdict(note)})
    lock_path.write_text(json.dumps({
        "schema_version": "compshare.rag.asset-lock.v1",
        "fingerprint": lock_fingerprint,
        "notes": locked_notes,
        "failures": failures,
    }, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return notes, failures


def inject_asset_notes(doc: SourceDocument, notes: dict[tuple[str, str, str], AssetNote]) -> tuple[str, list[dict[str, Any]]]:
    # Images are build-time evidence only. The runtime corpus carries the
    # structured VL extraction, never a hosted asset or clickable URL.
    media: list[dict[str, Any]] = []

    def replace(match: re.Match[str]) -> str:
        alt, ref = match.group(1), match.group(2)
        note = notes.get((doc.source_id, doc.source_path, ref))
        if note is None:
            return ""
        if not note.include_in_rag:
            return ""
        parts = [f"[图片说明] {note.description}"]
        if note.visible_text:
            parts.append("[图片文字] " + "；".join(note.visible_text))
        if note.controls:
            parts.append("[界面控件] " + "；".join(note.controls))
        if note.relations:
            parts.append("[界面关系] " + "；".join(note.relations))
        return "\n\n".join(parts)

    return IMAGE_RE.sub(replace, doc.text), media


def split_sections(text: str, fallback_title: str) -> list[tuple[list[str], str]]:
    sections: list[tuple[list[str], str]] = []
    heading_path: list[str] = []
    current: list[str] = []
    current_path: list[str] = [fallback_title]
    in_fence = False
    for line in text.splitlines():
        if line.lstrip().startswith("```"):
            in_fence = not in_fence
        match = None if in_fence else HEADING_RE.match(line)
        if match:
            if any(item.strip() for item in current):
                sections.append((current_path, "\n".join(current).strip()))
            level = len(match.group(1))
            title = match.group(2).strip()
            heading_path = heading_path[: level - 1]
            heading_path.append(title)
            current_path = heading_path.copy()
            current = [line]
        else:
            current.append(line)
    if any(item.strip() for item in current):
        sections.append((current_path, "\n".join(current).strip()))
    return sections


QUESTION_HEADING_RE = re.compile(
    r"(?:[?？]$|^(?:如何|怎么|为什么|为何|是否|能否|什么是|常见问题|问题\s*\d+|忘记|无法|不能|失败|报错|错误))"
)


# Topic mixing in coalesced chunks is REAL and deliberately NOT fixed here.
#
# The rule below merges any two adjacent sections that share only their H1, so a
# chunk keeps the heading of the section that OPENED it while later, unrelated
# topics ride along invisible to title, heading_path and question_patterns —
# matchable only through body text, in a vector they share with another subject.
# Two shipped examples: v2-resource_purchase-15e832a82bdfedbb is titled
# 查看账户活动记录 and also holds ## 学生权限限制 and ## FAQ（学生常见问题）;
# v2-resource_purchase-0b932f89a95bcff9 is titled 操作说明 (under 功能二：邀请成员)
# and also holds ## 功能三：预算分配.
#
# Two replacement rules were measured over all 232 documents, counting sections
# whose body contains a heading at or above their own depth:
#
#	rule                     sections  median runes  <200 runes  mixed
#	same H1 (this one)            364          2149          40     86
#	siblings or direct child      643           808         125    142
#	strict descendant only       1641           264         658     32
#
# Sibling merging makes the target metric WORSE — merging at the same depth is
# what puts same-depth headings in one section. Descendant-only does fix it, and
# fragments the corpus 4.5x to a 264-rune median, which pulls directly against
# raising MAX_CONTENT_RUNES_FOR_EMB to 4000 in this same rebuild: one change gives
# the dense leg more text per chunk while the other takes it away, and afterwards
# neither is attributable.
#
# So this needs its own change with a retrieval eval behind it, not a ride-along
# on a source sync. The numbers above are the starting point.


def coalesce_sections(sections: list[tuple[list[str], str]]) -> list[tuple[list[str], str]]:
    """Join adjacent subordinate sections when they form one retrieval unit."""
    out: list[tuple[list[str], str]] = []
    for heading_path, content in sections:
        if not out:
            out.append((heading_path, content))
            continue
        prev_path, prev_content = out[-1]
        same_root = bool(prev_path and heading_path and prev_path[0] == heading_path[0])
        prev_title = prev_path[-1] if prev_path else ""
        next_title = heading_path[-1] if heading_path else ""
        question_like = bool(QUESTION_HEADING_RE.search(prev_title) or QUESTION_HEADING_RE.search(next_title))
        subordinate = len(prev_path) > 1 or len(heading_path) > 1
        combined = prev_content.rstrip() + "\n\n" + content.lstrip()
        if same_root and subordinate and not question_like and len(combined) <= TARGET_CONTENT_RUNES:
            out[-1] = (prev_path, combined)
        else:
            out.append((heading_path, content))
    return out


def document_type(doc: SourceDocument) -> str:
    """Classify the retrieval unit before choosing chunk boundaries."""
    path = doc.source_path.replace("\\", "/").lower()
    title = doc.title.lower()
    if doc.source_kind == "public_faq_export":
        return "faq_collection"
    if "/action_md/" in f"/{path}" or path.startswith("public/action_md/"):
        return "api_reference"
    if "/api/" in f"/{path}" or re.search(r"\b(?:request|response)\s+(?:parameters|elements)\b", doc.text, re.I):
        return "api_reference"
    if "/operation/" in f"/{path}" or any(token in title for token in ("指南", "教程", "操作", "部署", "安装", "使用")):
        return "operation_guide"
    return "reference"


def plan_document_units(doc: SourceDocument, text: str) -> list[tuple[list[str], str, str]]:
    """Keep semantic documents whole; split only oversized documents at complete units."""
    kind = document_type(doc)
    clean = text.strip()
    if kind in {"api_reference", "operation_guide"} and len(clean) <= MAX_CONTENT_RUNES:
        return [([doc.title], clean, "complete_document")]

    sections = split_sections(clean, doc.title)
    if kind == "faq_collection":
        # Each question and answer is its own retrieval unit. Never merge neighboring questions.
        return [(path, section, "question_answer") for path, section in sections]

    if kind in {"api_reference", "operation_guide"}:
        role = "api_section" if kind == "api_reference" else "complete_step_group"
        units: list[tuple[list[str], str, str]] = []
        current_path: list[str] = [doc.title]
        current: list[str] = []
        current_size = 0
        for path, section in sections:
            extra = len(section) + (2 if current else 0)
            if current and current_size + extra > TARGET_CONTENT_RUNES:
                units.append((current_path, "\n\n".join(current), role))
                current, current_size = [], 0
            if len(section) > MAX_CONTENT_RUNES:
                if current:
                    units.append((current_path, "\n\n".join(current), role))
                    current, current_size = [], 0
                for part in semantic_parts(section, client=None, model="deterministic-boundary"):
                    units.append((path or [doc.title], part, role))
                continue
            if not current:
                current_path = path or [doc.title]
            current.append(section)
            current_size += len(section) + (2 if len(current) > 1 else 0)
        if current:
            units.append((current_path, "\n\n".join(current), role))
        return units

    return [(path, section, "topic_section") for path, section in coalesce_sections(sections)]


def semantic_parts(text: str, *, client: ModelVerseClient | None, model: str) -> list[str]:
    if len(text) <= TARGET_CONTENT_RUNES:
        return [text.strip()]
    blocks = _semantic_blocks(text)
    if len(blocks) <= 1:
        return _hard_split(text, TARGET_CONTENT_RUNES)
    lock_path: Path | None = None
    if client is not None:
        lock_dir = client.cache_dir.parent.parent / "semantic_plans"
        lock_dir.mkdir(parents=True, exist_ok=True)
        lock_path = lock_dir / f"{sha256_bytes((model + '\x1f' + text).encode('utf-8'))}.json"
        if lock_path.exists():
            locked = json.loads(lock_path.read_text(encoding="utf-8"))
            groups = locked.get("groups")
            if groups is None:
                return _pack_blocks(blocks, TARGET_CONTENT_RUNES)
            if _valid_groups(groups, len(blocks)):
                return ["\n\n".join(blocks[i] for i in group).strip() for group in groups]
    if client is not None and len(blocks) <= 40:
        compact = [{"id": i, "text": block[:800]} for i, block in enumerate(blocks)]
        prompt = (
            "为公开知识库生成语义切分计划。不得改写正文。只输出 JSON，严格包含 groups(array of arrays of integer)。"
            "每个块编号必须恰好出现一次，组内编号连续且升序；同一问题、表格、步骤或代码说明不得拆开；每组原文不超过3400字。\n"
            + json.dumps(compact, ensure_ascii=False)
        )
        try:
            payload = client.json_chat(model=model, prompt=prompt, max_tokens=1200, timeout=90, retries=1)
            groups = payload.get("groups")
            if set(payload) == {"groups"} and _valid_groups(groups, len(blocks)):
                planned = ["\n\n".join(blocks[i] for i in group).strip() for group in groups]
                if all(len(part) <= MAX_CONTENT_RUNES for part in planned):
                    if lock_path is not None:
                        lock_path.write_text(json.dumps({"groups": groups}, sort_keys=True) + "\n", encoding="utf-8")
                    return planned
        except Exception:
            pass
    if lock_path is not None:
        lock_path.write_text('{"groups":null}\n', encoding="utf-8")
    return _pack_blocks(blocks, TARGET_CONTENT_RUNES)


def _semantic_blocks(text: str) -> list[str]:
    raw = re.split(r"\n\s*\n", text.strip())
    return [item.strip() for item in raw if item.strip()]


def _valid_groups(groups: Any, count: int) -> bool:
    if not isinstance(groups, list) or not groups:
        return False
    flat: list[int] = []
    for group in groups:
        if not isinstance(group, list) or not group or not all(isinstance(item, int) for item in group):
            return False
        if group != list(range(group[0], group[-1] + 1)):
            return False
        flat.extend(group)
    return flat == list(range(count))


def _pack_blocks(blocks: list[str], limit: int) -> list[str]:
    out: list[str] = []
    current: list[str] = []
    size = 0
    for block in blocks:
        if len(block) > MAX_CONTENT_RUNES:
            if current:
                out.append("\n\n".join(current))
                current, size = [], 0
            out.extend(_hard_split(block, limit))
            continue
        extra = len(block) + (2 if current else 0)
        if current and size + extra > limit:
            out.append("\n\n".join(current))
            current, size = [block], len(block)
        else:
            current.append(block)
            size += extra
    if current:
        out.append("\n\n".join(current))
    return out


def _hard_split(text: str, limit: int) -> list[str]:
    lines = text.splitlines()
    out: list[str] = []
    current: list[str] = []
    size = 0
    for line in lines:
        if len(line) > limit:
            if current:
                out.append("\n".join(current).strip())
                current, size = [], 0
            for start in range(0, len(line), limit):
                out.append(line[start : start + limit])
            continue
        if current and size + len(line) + 1 > limit:
            out.append("\n".join(current).strip())
            current, size = [line], len(line)
        else:
            current.append(line)
            size += len(line) + 1
    if current:
        out.append("\n".join(current).strip())
    return [item for item in out if item]


def product_area(doc: SourceDocument, title: str, content: str) -> str:
    text = f"{doc.source_path} {title} {content[:1000]}".lower()
    rules = [
        ("resource_purchase", ("创建实例", "购买", "gpu实例", "资源", "库存", "resourcecapacity", "availablecompshare")),
        ("billing_rule", ("计费", "账单", "充值", "套餐", "价格", "欠费", "续费")),
        ("login", ("登录", "ssh", "vnc", "jupyter", "密码")),
        ("monitor", ("监控", "利用率", "告警")),
        ("driver_cuda", ("cuda", "驱动", "nvidia", "cudnn")),
        ("windows", ("windows", "远程桌面", "rdp")),
        ("image", ("镜像", "comfyui", "livetalking", "infinitetalk", "rvc", "tts", "svc")),
        ("modelverse", ("modelverse", "模型广场", "api key", "baseurl", "大模型")),
        ("inference_serving", ("vllm", "sglang", "ollama", "推理服务", "gradio", "fastapi")),
        ("gpu_troubleshooting", ("oom", "显存", "报错", "故障", "无法启动", "打不开")),
        ("pytorch_basics", ("pytorch", "torch", "训练", "lora")),
        ("linux_ops", ("linux", "conda", "pip", "tmux", "nohup", "磁盘", "文件")),
    ]
    for area, tokens in rules:
        if any(token in text for token in tokens):
            return area
    return "init_failure" if doc.source_origin.startswith("external_") else "resource_purchase"


def build_chunks(
    documents: list[SourceDocument],
    *,
    kb_version: str,
    valid_from: str,
    asset_notes: dict[tuple[str, str, str], AssetNote],
    semantic_client: ModelVerseClient | None,
    semantic_model: str,
) -> tuple[list[dict[str, Any]], dict[str, int]]:
    chunks: list[dict[str, Any]] = []
    skipped_duplicates = 0
    skipped_headings_only = 0
    seen_content: set[str] = set()
    for doc in documents:
        text, media = inject_asset_notes(doc, asset_notes)
        doc_type = document_type(doc)
        document_id = "doc-" + sha256_bytes(f"{doc.source_id}\x1f{doc.source_path}".encode("utf-8"))[:16]
        units = plan_document_units(doc, text)
        for section_index, (heading_path, section, chunk_role) in enumerate(units, start=1):
            title = heading_path[-1] if heading_path else doc.title
            for part_index, content in enumerate(semantic_parts(section, client=semantic_client, model=semantic_model), start=1):
                content = content.strip()
                if len(content) < 10:
                    continue
                if not has_body_beyond_headings(content):
                    skipped_headings_only += 1
                    continue
                digest = sha256_bytes(content.encode("utf-8"))
                if digest in seen_content:
                    skipped_duplicates += 1
                    continue
                seen_content.add(digest)
                area = product_area(doc, title, content)
                stable = sha256_bytes(f"{doc.source_id}\x1f{doc.source_path}\x1f{section_index}\x1f{part_index}\x1f{digest}".encode("utf-8"))[:16]
                prefix = "ext-v2" if doc.source_origin.startswith("external_") else "v2"
                chunk_title = title if part_index == 1 else f"{title}（{part_index}）"
                question_patterns = _question_patterns(chunk_title, heading_path, area)
                row: dict[str, Any] = {
                    "acl": "customer_safe",
                    "asset_refs": _asset_refs_for_content(media, content),
                    "chunk_id": f"{prefix}-{area}-{stable}",
                    "confidence": "high",
                    "content": content,
                    "chunk_role": chunk_role,
                    "document_id": document_id,
                    "document_title": doc.title,
                    "document_type": doc_type,
                    "evidence_kind": "knowledge",
                    "heading_path": heading_path,
                    "kb_version": kb_version,
                    "product_area": area,
                    "question_patterns": question_patterns,
                    "retrieval_score_hint": None,
                    "source_origin": doc.source_origin,
                    "source_refs": [f"{doc.source_id}:{doc.source_path}"],
                    "source_type": "runbook" if _looks_like_runbook(content) else "faq",
                    "surface_url": doc.surface_url,
                    "title": chunk_title,
                    "valid_from": valid_from,
                    "v2_source_kind": doc.source_kind,
                }
                if doc.compatibility:
                    row["compatibility"] = doc.compatibility
                chunks.append(row)
    chunks.sort(key=lambda item: item["chunk_id"])
    return chunks, {
        "chunk_count": len(chunks),
        "duplicate_content_skipped": skipped_duplicates,
        "headings_only_skipped": skipped_headings_only,
    }


def _question_patterns(title: str, heading_path: list[str], area: str) -> list[str]:
    """Structural metadata that labels a chunk, never invented question text.

    question_patterns is not a free field. compshare-kb joins it into the BM25
    "patterns" field AND into the text that is embedded and reranked
    (retrieval_scoring.go, chunkRepr), so anything placed here is scored as
    though the source document said it.

    Two kinds of entry were removed for that reason:

    Templated questions. "怎么" + title and title + "怎么办" were appended to every
    chunk, producing strings the docs never contain — "怎么AttachCompshareDisk —
    挂载已有云盘" — which dilute the patterns field on every chunk equally while
    matching no real query.

    Hand-written question lists. Three keyword rules injected curated phrasings
    for disk billing, Coding Plan and resource capacity ("一直暂无资源是什么情况",
    "Normal 状态一定有库存吗", …). They encode one author's guess at how users ask
    about three topics out of the whole corpus, they are invisible to anyone
    reading the source document, and they made those three topics rank on
    phrasings their text does not support while every other topic got nothing.
    Query-side understanding belongs in the planner, which sees the actual
    question; the corpus side should describe what the document says.

    What remains is derived from the document itself: its heading, its position
    in the heading tree, and its product area.
    """
    values = [title, " ".join(heading_path), area.replace("_", " ")]
    out: list[str] = []
    for value in values:
        value = value.strip()
        if value and value not in out:
            out.append(value[:200])
    return out[:20]


def has_body_beyond_headings(content: str) -> bool:
    """Reject a chunk whose text is nothing but heading lines.

    A section that got split so that its body went elsewhere leaves behind a
    chunk like "## 控制台常见报错提示" and nothing else. It answers no question,
    but it competes for retrieval like any other chunk: it embeds as a short,
    topic-pure vector, which makes it an excellent cosine match for exactly the
    query whose answer is in the sibling chunk it was severed from — so it wins
    a top-3 slot and spends it saying only that the topic exists. 16 of 544
    internal chunks and 10 of 1200 external ones were in this state.
    """
    for line in content.splitlines():
        line = line.strip()
        if line and not HEADING_RE.match(line):
            return True
    return False


def _asset_refs_for_content(media: list[dict[str, Any]], content: str) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    seen: set[tuple[str, str]] = set()
    for item in media:
        key = (str(item.get("asset_id") or ""), str(item.get("url") or ""))
        if item.get("url") not in content or key in seen:
            continue
        seen.add(key)
        out.append(item)
    return out


def _looks_like_runbook(content: str) -> bool:
    return bool(re.search(r"(?m)^(?:\d+[.、]|步骤\s*\d+|```(?:bash|shell|powershell|python))", content))


def load_legacy_external(path: Path, *, kb_version: str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        row["kb_version"] = kb_version
        row["legacy_unrebuildable"] = True
        row["v2_source_kind"] = "legacy_external_chunk_without_source_snapshot"
        rows.append(row)
    return rows


def merge_external(legacy: list[dict[str, Any]], rebuilt: list[dict[str, Any]]) -> tuple[list[dict[str, Any]], int]:
    out: list[dict[str, Any]] = []
    seen_ids: set[str] = set()
    seen_content: set[str] = set()
    skipped = 0
    for row in [*legacy, *rebuilt]:
        chunk_id = str(row.get("chunk_id") or "")
        content_digest = sha256_bytes(str(row.get("content") or "").encode("utf-8"))
        if chunk_id in seen_ids or content_digest in seen_content:
            skipped += 1
            continue
        seen_ids.add(chunk_id)
        seen_content.add(content_digest)
        out.append(row)
    out.sort(key=lambda item: item["chunk_id"])
    return out, skipped


def validate_chunks(rows: list[dict[str, Any]], *, expected_version: str) -> list[str]:
    errors: list[str] = []
    ids: set[str] = set()
    required = {"chunk_id", "kb_version", "source_type", "source_origin", "product_area", "acl", "confidence", "title", "content"}
    for index, row in enumerate(rows, start=1):
        missing = sorted(key for key in required if not str(row.get(key) or "").strip())
        if missing:
            errors.append(f"row {index}: missing {','.join(missing)}")
        chunk_id = str(row.get("chunk_id") or "")
        if chunk_id in ids:
            errors.append(f"row {index}: duplicate chunk_id {chunk_id}")
        ids.add(chunk_id)
        if row.get("kb_version") != expected_version:
            errors.append(f"row {index}: kb_version mismatch")
        if row.get("acl") != "customer_safe":
            errors.append(f"row {index}: acl must be customer_safe")
        if len(str(row.get("content") or "")) > MAX_CONTENT_RUNES:
            errors.append(f"row {index}: content exceeds {MAX_CONTENT_RUNES}")
        if len(row.get("question_patterns") or []) > 20:
            errors.append(f"row {index}: too many question patterns")
        for asset_index, asset in enumerate(row.get("asset_refs") or []):
            if not isinstance(asset, dict):
                errors.append(f"row {index}: asset_refs[{asset_index}] must be an object")
                continue
            url = str(asset.get("url") or "")
            parsed = urlparse(url)
            if parsed.scheme != "https" or not parsed.netloc:
                errors.append(f"row {index}: asset_refs[{asset_index}] must use a public HTTPS URL")
            if url and url not in str(row.get("content") or ""):
                errors.append(f"row {index}: asset_refs[{asset_index}] URL missing from content")
        if "REDACTED]" in str(row.get("content") or ""):
            errors.append(f"row {index}: legacy redaction marker remains")
    return errors


def write_jsonl(path: Path, rows: Iterable[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8", newline="\n") as handle:
        for row in rows:
            handle.write(json.dumps(row, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n")


def write_release_manifest(
    path: Path,
    *,
    internal_corpus: Path,
    external_corpus: Path,
    source_locks: list[dict[str, Any]],
    report: dict[str, Any],
    models: dict[str, str],
) -> dict[str, Any]:
    manifest = {
        "schema_version": "compshare.rag.release.v2",
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "release_id": f"rag-v2-{date.today().isoformat()}",
        "sources": source_locks,
        "models": models,
        "artifacts": {
            "internal_corpus": {"path": internal_corpus.as_posix(), "sha256": normalized_file_digest(internal_corpus)},
            "external_corpus": {"path": external_corpus.as_posix(), "sha256": normalized_file_digest(external_corpus)},
        },
        "report": report,
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return manifest
