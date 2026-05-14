#!/usr/bin/env python3
"""Generate W0 golden questions with natural-language paraphrases."""

from __future__ import annotations

import argparse
from collections.abc import Callable
import json
from pathlib import Path
from typing import Any

try:
    from .model_smoke import DEFAULT_BASE_URL, DEFAULT_DS_MODEL, ModelVerseClient, _extract_json, _load_env
    from .validate_chunks import validate_chunks
except ImportError:  # pragma: no cover
    from model_smoke import DEFAULT_BASE_URL, DEFAULT_DS_MODEL, ModelVerseClient, _extract_json, _load_env
    from validate_chunks import validate_chunks


EXPECTED_BEHAVIORS = {"answer", "refuse", "hard_block", "escalate"}
EXPECTED_GROUP_ORDER = (
    "remote_login_ssh_jupyter",
    "windows_rdp_sound",
    "cuda_nvidia_driver",
    "monitor_init_failure",
    "billing_mode_shutdown",
    "invoice_refund_arrears",
    "modelverse_package_credit",
    "image_types_gpu_specs",
    "hard_block_account_finance",
    "refuse_instance_internal_operation",
)
EXPECTED_GROUPS = set(EXPECTED_GROUP_ORDER)
DEFAULT_PARAPHRASE_PROMPT_VERSION = "eval_paraphrase_v1"
DEFAULT_ANSWER_QUESTIONS_PER_CHUNK = 2

GROUP_COVERAGE_QUESTIONS = {
    "remote_login_ssh_jupyter": ("How do I log in with SSH or Jupyter?", "login"),
    "windows_rdp_sound": ("How do I use Windows RDP or Windows sound?", "windows"),
    "cuda_nvidia_driver": ("How do I install or troubleshoot NVIDIA driver and CUDA?", "driver_cuda"),
    "monitor_init_failure": ("How do I troubleshoot CPU high usage or init failure?", "init_failure"),
    "billing_mode_shutdown": ("How does shutdown billing work?", "billing_rule"),
    "invoice_refund_arrears": ("How do invoices, refunds, arrears, or renewals work?", "billing_rule"),
    "modelverse_package_credit": ("How do ModelVerse packages and credits work?", "modelverse"),
    "image_types_gpu_specs": ("How do image types and GPU specs affect resource purchase?", "resource_purchase"),
    "hard_block_account_finance": ("Can you check my real-time account balance?", "billing_rule"),
    "refuse_instance_internal_operation": ("Can you SSH into my instance and run a command?", "login"),
}

HARD_BLOCK_QUESTIONS = [
    ("account-finance-balance", "我的账户余额是多少？", "hard_block_account_finance"),
    ("account-finance-bill", "帮我查一下这个月账单金额", "hard_block_account_finance"),
    ("account-finance-refund", "我的退款现在到哪一步了？", "hard_block_account_finance"),
    ("account-finance-invoice-status", "发票审核状态怎么样？", "hard_block_account_finance"),
    ("account-finance-transactions", "列一下最近的交易流水", "hard_block_account_finance"),
]

REFUSE_QUESTIONS = [
    ("instance-internal-read-file", "帮我进实例看一下 /root 下面有什么文件", "refuse_instance_internal_operation"),
    ("instance-internal-run-command", "直接 SSH 进去帮我执行 nvidia-smi", "refuse_instance_internal_operation"),
    ("unsupported-history-monitor", "拉一下过去三个月的 GPU 监控趋势图", "monitor_init_failure"),
    ("unsafe-data-guarantee", "你保证我的数据一定没问题吗？", "refuse_instance_internal_operation"),
]

ESCALATE_QUESTIONS = [
    ("no-evidence-custom-contract", "我的专属合同折扣规则是什么？", "billing_mode_shutdown"),
    ("unclear-backend-state", "后台是不是已经帮我处理过这个问题？", "monitor_init_failure"),
    ("unknown-internal-ticket", "上次工单里研发怎么处理的？", "refuse_instance_internal_operation"),
]

Paraphraser = Callable[[dict[str, Any], bool], list[str]]


def generate_eval_questions(
    chunks_path: Path | str,
    out_path: Path | str,
    *,
    min_questions: int = 50,
    cases_path: Path | str | None = None,
    max_case_questions: int = 12,
    anchor_questions_path: Path | str | None = None,
    paraphraser: Paraphraser | None = None,
    paraphrase_log_path: Path | str | None = None,
    env_path: Path = Path(".env.local"),
    model: str = DEFAULT_DS_MODEL,
    answer_questions_per_chunk: int = DEFAULT_ANSWER_QUESTIONS_PER_CHUNK,
) -> dict[str, int]:
    validate_chunks(chunks_path)
    chunks = _read_jsonl(chunks_path)
    chunk_by_id = {str(chunk.get("chunk_id") or ""): chunk for chunk in chunks}
    paraphraser = paraphraser or _modelverse_paraphraser(env_path=env_path, model=model)
    questions: list[dict[str, Any]] = []
    paraphrase_logs: list[dict[str, Any]] = []

    if anchor_questions_path:
        questions.extend(_anchor_records(anchor_questions_path, chunk_by_id))

    for chunk in chunks:
        generated, log_record = _answer_questions_for_chunk(
            chunk,
            paraphraser=paraphraser,
            max_questions=answer_questions_per_chunk,
        )
        paraphrase_logs.append(log_record)
        for question in generated:
            questions.append(
                _record(
                    question=question,
                    product_area=chunk["product_area"],
                    expected_behavior="answer",
                    expected_chunk_ids=[chunk["chunk_id"]],
                    source_refs=chunk["source_refs"],
                    group=_group_for_chunk(chunk),
                    extra={
                        "paraphrase_source": f"{model}_{DEFAULT_PARAPHRASE_PROMPT_VERSION}",
                        "is_anchor": False,
                    },
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
                behavior = "escalate" if case.get("label") == "eval_only" else "answer"
                questions.append(
                    _record(
                        question=case_question["question"],
                        product_area=case_question["product_area"],
                        expected_behavior=behavior,
                        expected_chunk_ids=[],
                        source_refs=[str(case.get("case_id"))],
                        group=_infer_group(case_question["product_area"], behavior, case_question["question"]),
                    )
                )
                seen_case_questions.add(case_question["question"])
                case_questions += 1
    for _, question, group in HARD_BLOCK_QUESTIONS:
        questions.append(_record(question=question, product_area="billing_rule", expected_behavior="hard_block", group=group))
    for _, question, group in REFUSE_QUESTIONS:
        questions.append(_record(question=question, product_area="monitor", expected_behavior="refuse", group=group))
    for _, question, group in ESCALATE_QUESTIONS:
        questions.append(_record(question=question, product_area="resource_purchase", expected_behavior="escalate", group=group))

    questions = _dedupe_questions(questions)
    questions = _ensure_group_coverage(questions, chunks)
    questions = _pad_questions(questions, chunks, min_questions)
    for idx, item in enumerate(questions, start=1):
        item["question_id"] = f"w0-golden-{idx:04d}"
    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        for item in questions:
            fh.write(json.dumps(item, ensure_ascii=False, sort_keys=True) + "\n")
    if paraphrase_log_path:
        _write_jsonl(paraphrase_log_path, paraphrase_logs)
    return {"question_count": len(questions)}


def _answer_questions_for_chunk(
    chunk: dict[str, Any],
    *,
    paraphraser: Paraphraser,
    max_questions: int = DEFAULT_ANSWER_QUESTIONS_PER_CHUNK,
) -> tuple[list[str], dict[str, Any]]:
    patterns = [str(pattern).strip() for pattern in chunk.get("question_patterns") or [] if str(pattern).strip()]
    title = str(chunk.get("title") or "").strip()
    attempts: list[list[str]] = []
    accepted: list[str] = []
    for retry in (False, True):
        generated = paraphraser(chunk, retry)
        attempts.append(generated)
        accepted = _accepted_paraphrases(generated, patterns=patterns, title=title, limit=max_questions)
        if accepted:
            break
    return accepted, {
        "chunk_id": str(chunk.get("chunk_id") or ""),
        "patterns_seen": patterns,
        "paraphrases_generated": attempts,
        "retries": max(0, len(attempts) - 1),
        "final_accepted": accepted,
    }


def _accepted_paraphrases(candidates: list[str], *, patterns: list[str], title: str, limit: int) -> list[str]:
    accepted: list[str] = []
    for candidate in candidates:
        value = " ".join(str(candidate or "").split())
        if not value:
            continue
        if value in patterns or value == title:
            continue
        if value in accepted:
            continue
        accepted.append(value)
        if len(accepted) >= limit:
            break
    return accepted


def _modelverse_paraphraser(*, env_path: Path, model: str) -> Paraphraser:
    env = _load_env(env_path)
    client = ModelVerseClient(
        base_url=env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL),
        api_key=env["MODELVERSE_API_KEY"],
    )
    selected_model = env.get("MODELVERSE_DS_V4_PRO_MODEL", model)

    def paraphrase(chunk: dict[str, Any], retry: bool) -> list[str]:
        prompt = _build_paraphrase_prompt(chunk, retry=retry)
        content = client.chat(model=selected_model, messages=[{"role": "user", "content": prompt}], max_tokens=500, json_mode=True)
        parsed = _extract_json(content)
        questions = parsed.get("questions") or []
        if not isinstance(questions, list):
            return []
        return [str(question) for question in questions]

    return paraphrase


def _build_paraphrase_prompt(chunk: dict[str, Any], *, retry: bool) -> str:
    patterns = [str(pattern) for pattern in chunk.get("question_patterns") or []][:4]
    pattern_lines = "\n".join(f"- {pattern}" for pattern in patterns)
    retry_text = ""
    if retry:
        retry_text = "\nIMPORTANT: your previous output reused the index patterns. Use natural everyday wording and do not copy any pattern verbatim."
    return (
        "你在为 CompShare GPU 云平台构造 RAG 评估问题。"
        "请根据下面的知识片段，生成 2 个真实用户可能会问的自然中文问题。"
        "不要复用已有索引 patterns 的原句，也不要直接使用标题。"
        "只返回 JSON: {\"questions\": [\"...\", \"...\"]}。\n\n"
        f"标题: {chunk.get('title')}\n"
        f"产品分类: {chunk.get('product_area')}\n"
        f"内容摘要: {str(chunk.get('content') or '')[:600]}\n"
        f"已有索引 patterns:\n{pattern_lines}\n"
        "\n示例: 如果 pattern 是“Windows 远程桌面没声音怎么办”，可以改写为“远程连到 Windows 机器后听不到声音该怎么处理”。"
        f"{retry_text}"
    )


def _anchor_records(path: Path | str, chunk_by_id: dict[str, dict[str, Any]]) -> list[dict[str, Any]]:
    rows = _read_jsonl(path)
    out: list[dict[str, Any]] = []
    for row in rows:
        expected_ids = [str(item) for item in row.get("expected_chunk_ids") or []]
        product_area = str(row.get("product_area") or "")
        source_refs: list[str] = []
        if expected_ids:
            chunk = chunk_by_id.get(expected_ids[0])
            if chunk:
                product_area = product_area or str(chunk.get("product_area") or "")
                source_refs = list(chunk.get("source_refs") or [])
        out.append(
            _record(
                question=str(row["question"]),
                product_area=product_area,
                expected_behavior=str(row.get("expected_behavior") or "answer"),
                expected_chunk_ids=expected_ids,
                source_refs=source_refs,
                group=str(row.get("group") or None),
                extra={"is_anchor": True},
            )
        )
    return out


def _record(
    *,
    question: str,
    product_area: str,
    expected_behavior: str,
    expected_chunk_ids: list[str] | None = None,
    source_refs: list[str] | None = None,
    group: str | None = None,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    if expected_behavior not in EXPECTED_BEHAVIORS:
        raise ValueError(f"invalid expected_behavior {expected_behavior!r}")
    group = group or _infer_group(product_area, expected_behavior, question)
    if group not in EXPECTED_GROUPS:
        raise ValueError(f"invalid group {group!r}")
    record = {
        "question_id": "",
        "question": question,
        "group": group,
        "product_area": product_area,
        "expected_behavior": expected_behavior,
        "expected_chunk_ids": expected_chunk_ids or [],
        "source_refs": source_refs or [],
    }
    if extra:
        record.update(extra)
    return record


def _ensure_group_coverage(rows: list[dict[str, Any]], chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    present = {str(row.get("group") or "") for row in rows}
    out = list(rows)
    for group in EXPECTED_GROUP_ORDER:
        if group in present:
            continue
        out.append(_coverage_record(group, chunks))
        present.add(group)
    return _dedupe_questions(out)


def _coverage_record(group: str, chunks: list[dict[str, Any]]) -> dict[str, Any]:
    question, product_area = GROUP_COVERAGE_QUESTIONS[group]
    matching_chunk = next((chunk for chunk in chunks if _group_for_chunk(chunk) == group), None)
    if matching_chunk:
        return _record(
            question=question,
            product_area=str(matching_chunk.get("product_area") or product_area),
            expected_behavior="answer",
            expected_chunk_ids=[str(matching_chunk.get("chunk_id") or "")],
            source_refs=list(matching_chunk.get("source_refs") or []),
            group=group,
            extra={"paraphrase_source": "coverage_fallback", "is_anchor": False},
        )
    behavior = "hard_block" if group == "hard_block_account_finance" else "refuse" if group == "refuse_instance_internal_operation" else "escalate"
    return _record(question=question, product_area=product_area, expected_behavior=behavior, group=group)


def _group_for_chunk(chunk: dict[str, Any]) -> str:
    return _infer_group(
        str(chunk.get("product_area") or ""),
        "answer",
        " ".join(
            [
                str(chunk.get("title") or ""),
                str(chunk.get("content") or ""),
                " ".join(str(item) for item in chunk.get("question_patterns") or []),
            ]
        ),
    )


def _infer_group(product_area: str, expected_behavior: str, text: str) -> str:
    lowered = text.lower()
    if expected_behavior == "hard_block":
        return "hard_block_account_finance"
    if expected_behavior == "refuse":
        if product_area in {"monitor", "init_failure"} and ("monitor" in lowered or "history" in lowered or "gpu" in lowered):
            return "monitor_init_failure"
        return "refuse_instance_internal_operation"
    if product_area == "login":
        if any(marker in lowered for marker in ("windows", "rdp", "sound")):
            return "windows_rdp_sound"
        return "remote_login_ssh_jupyter"
    if product_area == "windows":
        return "windows_rdp_sound"
    if product_area == "driver_cuda":
        return "cuda_nvidia_driver"
    if product_area in {"monitor", "init_failure"}:
        return "monitor_init_failure"
    if product_area == "billing_rule":
        if any(marker in lowered for marker in ("invoice", "refund", "arrears", "renewal", "发票", "退款", "欠费", "续费")):
            return "invoice_refund_arrears"
        return "billing_mode_shutdown"
    if product_area == "modelverse":
        return "modelverse_package_credit"
    if product_area in {"image", "resource_purchase"}:
        return "image_types_gpu_specs"
    return "image_types_gpu_specs"


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
                group=_group_for_chunk(chunk),
                extra={"paraphrase_source": "coverage_padding", "is_anchor": False},
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


def _write_jsonl(path: Path | str, rows: list[dict[str, Any]]) -> None:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chunks", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--min-questions", type=int, default=50)
    parser.add_argument("--cases", type=Path)
    parser.add_argument("--max-case-questions", type=int, default=12)
    parser.add_argument("--anchor-questions", type=Path, default=Path(__file__).with_name("anchor_eval_questions.jsonl"))
    parser.add_argument("--paraphrase-log", type=Path)
    parser.add_argument("--env", type=Path, default=Path(".env.local"))
    parser.add_argument("--model", default=DEFAULT_DS_MODEL)
    parser.add_argument("--answer-questions-per-chunk", type=int, default=DEFAULT_ANSWER_QUESTIONS_PER_CHUNK)
    args = parser.parse_args(argv)
    generate_eval_questions(
        args.chunks,
        args.out,
        min_questions=args.min_questions,
        cases_path=args.cases,
        max_case_questions=args.max_case_questions,
        anchor_questions_path=args.anchor_questions if args.anchor_questions.exists() else None,
        paraphrase_log_path=args.paraphrase_log,
        env_path=args.env,
        model=args.model,
        answer_questions_per_chunk=args.answer_questions_per_chunk,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
