#!/usr/bin/env python3
"""Shared W0 retrieval scoring primitives."""

from __future__ import annotations

from collections import Counter
from dataclasses import dataclass
import math
import re
import unicodedata
from typing import Any


BM25_K1 = 1.5
BM25_B = 0.75
DEFAULT_THRESHOLD = 0.5
PATTERNS_WEIGHT = 4.0
TITLE_WEIGHT = 3.0
CONTENT_WEIGHT = 1.0
AREA_BONUS = 2.0


@dataclass(frozen=True)
class BM25Document:
    term_frequency: Counter[str]
    length: int


@dataclass(frozen=True)
class BM25FieldIndex:
    documents: list[BM25Document]
    idf: dict[str, float]
    avg_length: float

    def score(self, chunk_index: int, query_tokens: list[str]) -> float:
        if chunk_index < 0 or chunk_index >= len(self.documents):
            return 0.0
        document = self.documents[chunk_index]
        if document.length == 0:
            return 0.0
        score = 0.0
        for token in query_tokens:
            tf = document.term_frequency.get(token, 0)
            if tf == 0:
                continue
            denominator = tf + BM25_K1 * (1 - BM25_B + BM25_B * document.length / self.avg_length)
            if denominator == 0:
                continue
            score += self.idf.get(token, 0.0) * (tf * (BM25_K1 + 1)) / denominator
        return score


class BM25Index:
    def __init__(self, chunks: list[dict[str, Any]]) -> None:
        self.patterns = _field_index([" ".join(str(item) for item in chunk.get("question_patterns") or []) for chunk in chunks])
        self.titles = _field_index([str(chunk.get("title") or "") for chunk in chunks])
        self.contents = _field_index([str(chunk.get("content") or "") for chunk in chunks])

    def score_chunk(self, *, query_tokens: list[str], product_area: str, chunk_index: int, chunk: dict[str, Any]) -> float:
        if not query_tokens:
            return 0.0
        score = (
            PATTERNS_WEIGHT * self.patterns.score(chunk_index, query_tokens)
            + TITLE_WEIGHT * self.titles.score(chunk_index, query_tokens)
            + CONTENT_WEIGHT * self.contents.score(chunk_index, query_tokens)
        )
        if score <= 0:
            return 0.0
        if product_area and product_area == str(chunk.get("product_area") or "").strip().lower():
            score += AREA_BONUS
        return score


def normalize_text(value: str) -> str:
    normalized = unicodedata.normalize("NFKC", value).lower()
    chars: list[str] = []
    last_was_space = True
    for char in normalized:
        if char.isascii() and char.isalnum():
            chars.append(char)
            last_was_space = False
        elif _is_cjk(char):
            chars.append(char)
            last_was_space = False
        elif char.isspace():
            if not last_was_space:
                chars.append(" ")
                last_was_space = True
        else:
            continue
    return "".join(chars).strip()


def tokenize_text(value: str) -> list[str]:
    normalized = normalize_text(value)
    if not normalized:
        return []
    tokens: list[str] = []
    for segment in normalized.split():
        if _ASCII_SEGMENT_RE.fullmatch(segment):
            tokens.append(segment)
            continue
        chars = list(segment)
        for size in (2, 3):
            if len(chars) < size:
                continue
            for index in range(0, len(chars) - size + 1):
                tokens.append("".join(chars[index : index + size]))
    return tokens


def _field_index(values: list[str]) -> BM25FieldIndex:
    documents: list[BM25Document] = []
    document_frequency: Counter[str] = Counter()
    total_length = 0
    for value in values:
        tokens = tokenize_text(value)
        term_frequency = Counter(tokens)
        documents.append(BM25Document(term_frequency=term_frequency, length=len(tokens)))
        document_frequency.update(term_frequency.keys())
        total_length += len(tokens)
    avg_length = (total_length / len(documents)) if documents and total_length else 1.0
    count = float(len(documents))
    idf = {
        token: math.log((count - df + 0.5) / (df + 0.5) + 1)
        for token, df in document_frequency.items()
    }
    return BM25FieldIndex(documents=documents, idf=idf, avg_length=avg_length)


def _is_cjk(char: str) -> bool:
    codepoint = ord(char)
    return any(start <= codepoint <= end for start, end in _CJK_RANGES)


_ASCII_SEGMENT_RE = re.compile(r"[a-z0-9]+")
_CJK_RANGES = (
    (0x3400, 0x4DBF),
    (0x4E00, 0x9FFF),
    (0xF900, 0xFAFF),
    (0x20000, 0x2A6DF),
    (0x2A700, 0x2EBEF),
    (0x30000, 0x3134F),
)
