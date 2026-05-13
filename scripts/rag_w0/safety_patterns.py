from __future__ import annotations

from pathlib import Path
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

KNOWN_STAFF_NAMES = tuple(
    line.strip()
    for line in Path(__file__).with_name("staff_names.txt").read_text(encoding="utf-8").splitlines()
    if line.strip() and not line.lstrip().startswith("#")
)
KNOWN_STAFF_RE = re.compile("|".join(re.escape(name) for name in KNOWN_STAFF_NAMES) or r"a^")
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
