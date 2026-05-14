from __future__ import annotations

import hashlib
import json
from pathlib import Path
import re
from urllib.parse import parse_qs, urlparse


SOURCE_MANIFEST_SCHEMA = "rag_w0.source_manifest.v1"
ASSET_MANIFEST_SCHEMA = "rag_w0.asset_manifest.v1"
LINK_MANIFEST_SCHEMA = "rag_w0.link_manifest.v1"
CHUNK_SCHEMA = "rag_w0.chunk.v1"

ALLOWED_SOURCE_TYPES = {"faq", "runbook"}
ALLOWED_PRODUCT_AREAS = {
    "login",
    "billing_rule",
    "monitor",
    "image",
    "driver_cuda",
    "windows",
    "modelverse",
    "init_failure",
    "resource_purchase",
}
ALLOWED_CONFIDENCE = {"high", "medium", "low"}
ALLOWED_EVIDENCE_KIND = {"knowledge"}

ALLOWED_ASSET_FINAL_STATES = {
    "included_with_vl_note",
    "included_with_ocr_note",
    "excluded_low_value",
    "excluded_qr_or_group",
    "missing_asset",
    "external_asset_snapshot_required",
    "external_asset_snapshot_failed",
    "duplicate_asset",
}
ALLOWED_LINK_FINAL_STATES = {
    "unknown",
    "snapshotted",
    "local_source_resolved",
    "navigation_only",
    "excluded",
    "review_required",
}

IMAGE_EXTENSIONS = {
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".webp",
    ".bmp",
    ".svg",
}
MARKDOWN_EXTENSIONS = {".md", ".mdx"}

IMAGE_RE = re.compile(r"!\[([^\]]*)\]\(\s*([^\)\s]+)(?:\s+\"[^\"]*\")?\s*\)", re.DOTALL)
LINK_RE = re.compile(r"(?<!!)\[([^\]]+)\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$")

SIGNED_QUERY_KEYS = {"token", "signature", "sign", "x-oss-signature", "x-amz-signature"}
TEMP_QUERY_KEYS = {"expires", "expire", "expiration"}
INTERNAL_PATH_RE = re.compile(r"/(?:admin|workorder|internal)(?:/|$)", re.IGNORECASE)
INTERNAL_TEXT_RE = re.compile(
    r"(gitlab|feishu|lark|workorder|/admin|/internal|uhost-[a-z0-9-]+|uimage-[a-z0-9-]+|bsi-[a-z0-9-]+)",
    re.IGNORECASE,
)


def read_json(path: Path | str) -> dict:
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        return json.load(fh)


def write_json(path: Path | str, data: dict) -> None:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def sha256_file(path: Path | str) -> str:
    h = hashlib.sha256()
    with Path(path).open("rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def stable_id(*parts: object, length: int = 16) -> str:
    raw = "\x1f".join(str(part) for part in parts)
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()[:length]


def iter_files(path: Path | str) -> list[Path]:
    root = Path(path)
    if root.is_file():
        return [root]
    return sorted(p for p in root.rglob("*") if p.is_file())


def tree_digest(path: Path | str) -> tuple[int, int, str]:
    files = iter_files(path)
    h = hashlib.sha256()
    total = 0
    root = Path(path)
    for file_path in files:
        rel = file_path.relative_to(root).as_posix() if root.is_dir() else file_path.name
        digest = sha256_file(file_path)
        size = file_path.stat().st_size
        total += size
        h.update(rel.encode("utf-8"))
        h.update(b"\0")
        h.update(digest.encode("ascii"))
        h.update(b"\0")
    return len(files), total, h.hexdigest()


def is_url(value: str) -> bool:
    parsed = urlparse(value)
    return parsed.scheme in {"http", "https"}


def strip_angle_brackets(value: str) -> str:
    value = value.strip()
    if value.startswith("<") and value.endswith(">"):
        return value[1:-1]
    return value


def denied_internal_host(host: str) -> bool:
    host = host.lower()
    return (
        "gitlab" in host
        or host.endswith(".feishu.cn")
        or host == "feishu.cn"
        or host.endswith(".lark.com")
        or host == "lark.com"
        or ".feishu." in host
    )


def has_signed_query(parsed) -> bool:
    keys = {key.lower() for key in parse_qs(parsed.query, keep_blank_values=True)}
    return bool(keys & SIGNED_QUERY_KEYS)


def is_temporary_download(parsed) -> bool:
    host = (parsed.hostname or parsed.netloc).lower()
    host_segments = [part for part in host.split(".") if part]
    query_keys = {key.lower() for key in parse_qs(parsed.query, keep_blank_values=True)}
    return (
        bool(query_keys & TEMP_QUERY_KEYS)
        or any(part in {"tmp", "temporary"} for part in host_segments)
        or "download" in host_segments and bool(query_keys)
    )


def surface_url_rejection_reason(raw_url: str | None) -> str | None:
    if raw_url is None:
        return None
    if not isinstance(raw_url, str) or raw_url.strip() == "":
        return "surface_url_empty"
    parsed = urlparse(raw_url)
    if not parsed.netloc:
        return "invalid_url"
    host = (parsed.hostname or parsed.netloc).lower()
    path = parsed.path or "/"
    if parsed.scheme != "https":
        return "scheme_not_https"
    if denied_internal_host(host):
        return "denied_internal_host"
    if INTERNAL_PATH_RE.search(path):
        return "internal_path"
    if has_signed_query(parsed):
        return "signed_url_query"
    if is_temporary_download(parsed):
        return "temporary_download"
    if host == "console.compshare.cn":
        return None
    if host == "www.compshare.cn" and path.startswith("/docs/"):
        return None
    return "host_not_in_allowlist"


def contains_internal_pattern(text: str) -> bool:
    return bool(INTERNAL_TEXT_RE.search(text or ""))
