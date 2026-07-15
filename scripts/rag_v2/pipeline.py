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
from urllib.parse import unquote, urlparse
from urllib.request import Request, urlopen
import zipfile


MAX_CONTENT_RUNES = 4000
TARGET_CONTENT_RUNES = 3400
IMAGE_RE = re.compile(r"!\[([^\]]*)\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$")
FRONT_MATTER_RE = re.compile(r"\A---\s*\n.*?\n---\s*\n", re.DOTALL)
HTML_NOISE_RE = re.compile(r"<(?:script|style)\b.*?</(?:script|style)>", re.DOTALL | re.IGNORECASE)
HTML_COMMENT_RE = re.compile(r"<!--.*?-->", re.DOTALL)
SPACE_RE = re.compile(r"[ \t]+")
BLANK_RE = re.compile(r"\n{3,}")


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
    lines = [SPACE_RE.sub(" ", line).rstrip() for line in text.splitlines()]
    return BLANK_RE.sub("\n\n", "\n".join(lines)).strip() + "\n"


def markdown_title(text: str, fallback: str) -> str:
    for line in text.splitlines():
        match = HEADING_RE.match(line.strip())
        if match:
            return match.group(2).strip()
    return fallback.replace("-", " ").replace("_", " ").strip()


def public_docs_url(relative: str) -> str | None:
    rel = relative.replace("\\", "/")
    if rel.startswith("pages/"):
        rel = rel[len("pages/") :]
    elif rel.startswith("public/action_md/"):
        rel = "gpus/action/" + rel[len("public/action_md/") :]
    else:
        return None
    if rel.lower().endswith(".md"):
        rel = rel[:-3]
    return "https://www.compshare.cn/docs/" + rel.strip("/")


def collect_internal_docs(root: Path) -> list[SourceDocument]:
    docs: list[SourceDocument] = []
    for path in sorted(root.rglob("*.md")):
        rel = path.relative_to(root).as_posix()
        if rel.startswith(".git/") or rel in {"README.md", "CONSOLE_DOCS_AUDIT.md"}:
            continue
        if not (rel.startswith("pages/") or rel.startswith("public/action_md/")):
            continue
        text = clean_public_text(path.read_text(encoding="utf-8", errors="replace"))
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
    for candidate in (
        (doc.absolute_path.parent / decoded_ref).resolve(),
        (doc.root / decoded_ref.lstrip("/")),
    ):
        if candidate.is_file():
            return candidate.resolve()
    return None


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
        "model": model,
        "fallback_model": fallback_model,
        "references": sorted(
            (doc.source_id, doc.source_path, alt, ref)
            for aliases in task_groups.values()
            for doc, alt, ref in aliases
        ),
    }, ensure_ascii=False, sort_keys=True).encode("utf-8"))
    if lock_path.exists():
        try:
            locked = json.loads(lock_path.read_text(encoding="utf-8"))
            if locked.get("fingerprint") == lock_fingerprint:
                for item in locked.get("notes") or []:
                    note_fields = {key: value for key, value in item.items() if key not in {"source_id", "ref"}}
                    notes[(str(item["source_id"]), str(item["source_path"]), str(item["ref"]))] = AssetNote(**note_fields)
                return notes, list(locked.get("failures") or [])
        except (OSError, ValueError, TypeError, KeyError):
            notes = {}

    def process(task: tuple[SourceDocument, str, str]) -> tuple[tuple[str, str, str], AssetNote | None, dict[str, Any] | None]:
        doc, alt, ref = task
        key = (doc.source_id, doc.source_path, ref)
        try:
            decoded = unquote(ref).replace("\\", "/")
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
                        for download_attempt in range(1):
                            try:
                                request = Request(decoded, headers={"User-Agent": "compshare-rag-v2/1.0"})
                                with urlopen(request, timeout=10) as response:
                                    data = response.read(20 * 1024 * 1024 + 1)
                                    content_type = response.headers.get_content_type()
                                break
                            except Exception as exc:
                                last_download_error = exc
                                if download_attempt == 0:
                                    raise
                                time.sleep(2 ** download_attempt)
                    else:
                        data = cached_remote.read_bytes()
                    if len(data) > 20 * 1024 * 1024:
                        raise ValueError("remote image exceeds 20 MiB")
                    if not content_type.startswith("image/"):
                        raise ValueError(f"remote asset is not an image: {content_type}")
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

    with ThreadPoolExecutor(max_workers=max(1, workers)) as executor:
        pending = {executor.submit(process, aliases[0]): aliases for aliases in task_groups.values()}
        for future in as_completed(pending):
            aliases = pending[future]
            key, note, failure = future.result()
            if note is not None:
                for alias_doc, _alias_alt, alias_ref in aliases:
                    notes[(alias_doc.source_id, alias_doc.source_path, alias_ref)] = replace(note, source_path=alias_doc.source_path)
            if failure is not None:
                for alias_doc, _alias_alt, alias_ref in aliases:
                    failures.append({**failure, "source": alias_doc.source_path, "ref": alias_ref})
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
    media: list[dict[str, Any]] = []

    def replace(match: re.Match[str]) -> str:
        alt, ref = match.group(1), match.group(2)
        note = notes.get((doc.source_id, doc.source_path, ref))
        if note is None:
            return f"[原文图片未包含在来源快照：{alt}]" if alt and alt.lower() not in {"image", "img"} else ""
        if not note.include_in_rag:
            return ""
        media.append({
            "asset_id": note.asset_id,
            "url": note.public_url,
            "description": note.description,
            "confidence": note.confidence,
            "visual_type": note.visual_type,
        })
        parts = [f"[图片说明] {note.description}"]
        if note.visible_text:
            parts.append("[图片文字] " + "；".join(note.visible_text))
        if note.controls:
            parts.append("[界面控件] " + "；".join(note.controls))
        if note.relations:
            parts.append("[界面关系] " + "；".join(note.relations))
        parts.append(f"[查看原图]({note.public_url})")
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
                digest = sha256_bytes(content.encode("utf-8"))
                if digest in seen_content:
                    skipped_duplicates += 1
                    continue
                seen_content.add(digest)
                area = product_area(doc, title, content)
                stable = sha256_bytes(f"{doc.source_id}\x1f{doc.source_path}\x1f{section_index}\x1f{part_index}\x1f{digest}".encode("utf-8"))[:16]
                prefix = "ext-v2" if doc.source_origin.startswith("external_") else "v2"
                chunk_title = title if part_index == 1 else f"{title}（{part_index}）"
                question_patterns = _question_patterns(chunk_title, heading_path, area, doc.source_path, content)
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
                    "retrieval_text": "\n".join([chunk_title, " > ".join(heading_path), *question_patterns, content]),
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
    return chunks, {"chunk_count": len(chunks), "duplicate_content_skipped": skipped_duplicates}


def _question_patterns(title: str, heading_path: list[str], area: str, source_path: str = "", content: str = "") -> list[str]:
    values = [title, "怎么" + title, title + "怎么办", " ".join(heading_path), area.replace("_", " ")]
    haystack = (title + " " + " ".join(heading_path) + " " + source_path + " " + content[:1200]).lower()
    if area == "billing_rule" and any(token in haystack for token in ("磁盘", "硬盘", "计费概览")):
        values.extend(["磁盘空间怎么收费", "系统盘免费额度", "数据盘收费", "100GB 原始空间免费吗"])
    if "coding plan" in haystack or "code plan" in haystack:
        values.extend(["删除 Coding Plan 包", "Coding Plan 套餐管理", "Coding Plan 支持退款吗", "Coding Plan 不支持退款"])
    if any(token in haystack for token in ("checkcompshareresourcecapacity", "describeavailablecompshareinstancetypes", "资源可用性", "可用机型")):
        values.extend(["一直暂无资源是什么情况", "怎么检查库存", "Normal 状态一定有库存吗", "ResourceEnough", "SoldOut"])
    out: list[str] = []
    for value in values:
        value = value.strip()
        if value and value not in out:
            out.append(value[:200])
    return out[:20]


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
