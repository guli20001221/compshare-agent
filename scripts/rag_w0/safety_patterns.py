from __future__ import annotations

import re


RESOURCE_ID_RE = re.compile(r"(?<![A-Za-z0-9])(?:uhost|uimage|bsi|udisk|eip)-[A-Za-z0-9-]+", re.IGNORECASE)
SECRET_RE = re.compile(
    r"(?i)(?:bearer[ \t]+\S+|(?:[\w-]*[_-])?"
    r"(?:password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|\u5bc6\u7801|\u53e3\u4ee4)"
    r"[ \t]*[:=\uff1a][ \t]*\S+)"
)
INTERNAL_URL_RE = re.compile(
    r"https?://[^\s)>\"]*(?:gitlab|feishu|lark|workorder|admin|internal)[^\s)>\"]*",
    re.IGNORECASE,
)
ANY_URL_RE = re.compile(r"https?://\S+", re.IGNORECASE)
WECHAT_HANDLE_RE = re.compile(r"\([A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)+\)")
PERSON_HANDLE_RE = re.compile(r"[\u4e00-\u9fff]{2,4}\([A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)+\)")
AT_MENTION_RE = re.compile(r"@[A-Za-z0-9_.\-\u4e00-\u9fff]+(?:\([^)]+\))?")
SPEAKER_NAME_RE = re.compile(r"(?m)^([\u4e00-\u9fff]{2,4})(?=\s+\d{1,2}-\d{1,2}\s+\d{1,2}:\d{2})")

# TODO(PR-RAG-3): replace this hard-coded staff list with a sidecar file or
# generic heuristic. Names not listed here may remain in internal cases.jsonl
# artifacts, but plan section 2.4 approval gates block them from customer chunks.
KNOWN_STAFF_NAMES = (
    "\u5f20\u6167",
    "\u5f20\u96e8\u6b23",
    "\u5434\u5bb6\u6b22",
    "\u674e\u5947",
    "\u6768\u6c38\u5229",
    "\u5f20\u6bc5",
    "\u5b59\u5bb6\u78ca",
    "\u848b\u5148\u9f99",
    "\u8499\u5b87\u94d6",
    "\u6bb5\u4ec1\u5764",
    "\u8881\u946b",
    "\u5468\u7693",
    "\u5b8b\u822a\u5b87",
    "\u5218\u78ca",
    "\u6208\u4f73\u5b81",
    "\u674e\u6881\u8d22",
    "\u5f3a\u54e5",
)
KNOWN_STAFF_RE = re.compile("|".join(re.escape(name) for name in KNOWN_STAFF_NAMES))
INTERNAL_OPS_RE = re.compile(
    r"(?:/cloud/[^\s)>\"]+|SPT\u5de5\u5177|\u7f57\u76d8|\u975e\u6807[\u4e00-\u9fffA-Za-z0-9_-]*|"
    r"\u8054\u7cfb\s*(?:SRE|\u7814\u53d1|\u8fd0\u8425)(?:\([^)]*\)|[\u4e00-\u9fff/\u3001]{0,20})?)",
    re.IGNORECASE,
)
INTERNAL_PLATFORM_RE = re.compile(r"\b(?:gitlab|feishu|lark)\b", re.IGNORECASE)


def redact_customer_text(text: str, *, redact_all_urls: bool = False) -> str:
    cleaned = RESOURCE_ID_RE.sub("[RESOURCE_ID_REDACTED]", text)
    cleaned = SECRET_RE.sub("[SECRET_REDACTED]", cleaned)
    cleaned = INTERNAL_URL_RE.sub("[LINK_REDACTED]", cleaned)
    if redact_all_urls:
        cleaned = ANY_URL_RE.sub("[LINK_REDACTED]", cleaned)
    cleaned = AT_MENTION_RE.sub("[PERSON_REDACTED]", cleaned)
    cleaned = PERSON_HANDLE_RE.sub("[PERSON_REDACTED]", cleaned)
    cleaned = WECHAT_HANDLE_RE.sub("[PERSON_REDACTED]", cleaned)
    cleaned = SPEAKER_NAME_RE.sub("[PERSON_REDACTED]", cleaned)
    cleaned = KNOWN_STAFF_RE.sub("[PERSON_REDACTED]", cleaned)
    cleaned = INTERNAL_OPS_RE.sub("[PRIVATE_PROCESS_REDACTED]", cleaned)
    cleaned = INTERNAL_PLATFORM_RE.sub("[PRIVATE_SYSTEM_REDACTED]", cleaned)
    return cleaned


def unsafe_cleaned_matches(text: str) -> list[str]:
    checks = [
        ("resource_id", RESOURCE_ID_RE),
        ("secret", SECRET_RE),
        ("internal_url", INTERNAL_URL_RE),
        ("wechat_handle", WECHAT_HANDLE_RE),
        ("at_mention", AT_MENTION_RE),
        ("staff_name", KNOWN_STAFF_RE),
        ("internal_ops", INTERNAL_OPS_RE),
        ("internal_platform", INTERNAL_PLATFORM_RE),
    ]
    return [name for name, pattern in checks if pattern.search(text or "")]
