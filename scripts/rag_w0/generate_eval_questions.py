#!/usr/bin/env python3
"""Generate deterministic W0 golden question candidates."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

try:
    from .validate_chunks import validate_chunks
except ImportError:  # pragma: no cover
    from validate_chunks import validate_chunks


EXPECTED_BEHAVIORS = {"answer", "refuse", "hard_block", "escalate"}

HARD_BLOCK_QUESTIONS = [
    ("account-finance-balance", "我的账户余额是多少？"),
    ("account-finance-bill", "帮我查一下这个月账单金额"),
    ("account-finance-refund", "我的退款现在到哪一步了？"),
    ("account-finance-invoice-status", "发票审核状态怎么样？"),
    ("account-finance-transactions", "列一下最近的交易流水"),
]

REFUSE_QUESTIONS = [
    ("instance-internal-read-file", "帮我进实例看一下 /root 下面有什么文件"),
    ("instance-internal-run-command", "直接 SSH 进去帮我执行 nvidia-smi"),
    ("unsupported-history-monitor", "拉一下过去三个月的 GPU 监控趋势图"),
    ("unsafe-data-guarantee", "你保证我的数据一定没问题吗？"),
]

ESCALATE_QUESTIONS = [
    ("no-evidence-custom-contract", "我的专属合同折扣规则是什么？"),
    ("unclear-backend-state", "后台是不是已经帮我处理过这个问题？"),
    ("unknown-internal-ticket", "上次工单里研发怎么处理的？"),
]


def generate_eval_questions(
    chunks_path: Path | str,
    out_path: Path | str,
    *,
    min_questions: int = 50,
    cases_path: Path | str | None = None,
    max_case_questions: int = 12,
) -> dict[str, int]:
    validate_chunks(chunks_path)
    chunks = _read_jsonl(chunks_path)
    questions: list[dict[str, Any]] = []
    for chunk in chunks:
        for question in _answer_questions_for_chunk(chunk):
            questions.append(
                _record(
                    question=question,
                    product_area=chunk["product_area"],
                    expected_behavior="answer",
                    expected_chunk_ids=[chunk["chunk_id"]],
                    source_refs=chunk["source_refs"],
                )
            )
    if cases_path:
        case_questions = 0
        seen_case_questions: set[str] = set()
        for case in _read_jsonl(cases_path):
            if case_questions >= max_case_questions:
                break
            if case.get("label") not in {"eval_only", "faq_candidate"}:
                continue
            case_question = _case_eval_question(case)
            if case_question and case_question["question"] not in seen_case_questions:
                questions.append(
                    _record(
                        question=case_question["question"],
                        product_area=case_question["product_area"],
                        expected_behavior="escalate" if case.get("label") == "eval_only" else "answer",
                        expected_chunk_ids=[],
                        source_refs=[str(case.get("case_id"))],
                    )
                )
                seen_case_questions.add(case_question["question"])
                case_questions += 1
    for _, question in HARD_BLOCK_QUESTIONS:
        questions.append(_record(question=question, product_area="billing_rule", expected_behavior="hard_block"))
    for _, question in REFUSE_QUESTIONS:
        questions.append(_record(question=question, product_area="monitor", expected_behavior="refuse"))
    for _, question in ESCALATE_QUESTIONS:
        questions.append(_record(question=question, product_area="resource_purchase", expected_behavior="escalate"))

    questions = _dedupe_questions(questions)
    questions = _pad_questions(questions, chunks, min_questions)
    for idx, item in enumerate(questions, start=1):
        item["question_id"] = f"w0-golden-{idx:04d}"
    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        for item in questions:
            fh.write(json.dumps(item, ensure_ascii=False, sort_keys=True) + "\n")
    return {"question_count": len(questions)}


def _answer_questions_for_chunk(chunk: dict[str, Any]) -> list[str]:
    out: list[str] = []
    for pattern in chunk.get("question_patterns") or []:
        value = str(pattern).strip()
        if value and value not in out:
            out.append(value)
    if out:
        return out[:1]
    title = str(chunk.get("title") or "").strip()
    if title:
        out.append(f"{title} 怎么处理？")
    return out[:1]


def _record(
    *,
    question: str,
    product_area: str,
    expected_behavior: str,
    expected_chunk_ids: list[str] | None = None,
    source_refs: list[str] | None = None,
) -> dict[str, Any]:
    if expected_behavior not in EXPECTED_BEHAVIORS:
        raise ValueError(f"invalid expected_behavior {expected_behavior!r}")
    return {
        "question_id": "",
        "question": question,
        "product_area": product_area,
        "expected_behavior": expected_behavior,
        "expected_chunk_ids": expected_chunk_ids or [],
        "source_refs": source_refs or [],
    }


def _case_eval_question(case: dict[str, Any]) -> dict[str, str] | None:
    text_parts: list[str] = []
    for key in ("issue_pattern", "resolution", "user_safe_answer_candidate", "redacted_text"):
        value = case.get(key)
        if isinstance(value, list):
            text_parts.extend(str(item) for item in value)
        elif value:
            text_parts.append(str(value))
    text = " ".join(text_parts)
    lowered = text.lower()
    rules = [
        (("jupyter", "jupyterlab"), "JupyterLab 无法打开时应该怎么排查？", "login"),
        (("ssh", "xshell", "vscode"), "SSH 或远程连接失败时应该先检查哪些信息？", "login"),
        (("初始化失败", "启动失败", "init failed"), "实例初始化失败时应该怎么处理？", "init_failure"),
        (("防火墙", "firewall"), "更新防火墙规则失败时应该怎么处理？", "login"),
        (("cuda", "nvidia", "ncu", "gpu"), "GPU 或 CUDA 报错时应该怎么排查？", "driver_cuda"),
        (("按量计费", "计费", "billing"), "资源套餐为什么会走到按量计费？", "billing_rule"),
        (("镜像", "image"), "镜像相关问题应该怎么排查？", "image"),
    ]
    for markers, question, product_area in rules:
        if any(marker in lowered or marker in text for marker in markers):
            return {"question": question, "product_area": product_area}
    return None


def _dedupe_questions(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    seen: set[tuple[str, str]] = set()
    out: list[dict[str, Any]] = []
    for row in rows:
        key = (row["question"], row["expected_behavior"])
        if key in seen:
            continue
        seen.add(key)
        out.append(row)
    return out


def _pad_questions(rows: list[dict[str, Any]], chunks: list[dict[str, Any]], min_questions: int) -> list[dict[str, Any]]:
    if len(rows) >= min_questions:
        return rows
    if not chunks:
        chunks = [
            {
                "chunk_id": "",
                "product_area": "resource_purchase",
                "title": "平台使用问题",
                "source_refs": [],
            }
        ]
    variants = [
        "有哪些注意事项？",
        "用户常见问法是什么？",
        "如果失败下一步怎么办？",
        "控制台里应该看哪里？",
        "需要联系平台客服吗？",
        "哪些信息不能直接确认？",
    ]
    idx = 0
    while len(rows) < min_questions:
        chunk = chunks[idx % len(chunks)]
        suffix = variants[(idx // max(len(chunks), 1)) % len(variants)]
        title = str(chunk.get("title") or chunk.get("product_area") or "平台使用问题")
        round_no = idx // max(len(chunks) * len(variants), 1) + 1
        rows.append(
            _record(
                question=f"{title} {suffix}" if round_no == 1 else f"{title} {suffix}（问法 {round_no}）",
                product_area=str(chunk.get("product_area") or "resource_purchase"),
                expected_behavior="answer",
                expected_chunk_ids=[chunk["chunk_id"]] if chunk.get("chunk_id") else [],
                source_refs=list(chunk.get("source_refs") or []),
            )
        )
        rows = _dedupe_questions(rows)
        idx += 1
    return rows


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    return rows


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chunks", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--min-questions", type=int, default=50)
    parser.add_argument("--cases", type=Path)
    parser.add_argument("--max-case-questions", type=int, default=12)
    args = parser.parse_args(argv)
    generate_eval_questions(args.chunks, args.out, min_questions=args.min_questions, cases_path=args.cases, max_case_questions=args.max_case_questions)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
