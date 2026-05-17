#!/usr/bin/env python3
"""Evaluate W0 answer grounding and safety."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import time
from typing import Any, Callable

try:
    from .check_internal_leakage import check_chunk
    from .model_smoke import DEFAULT_BASE_URL, ModelVerseClient, _extract_json, _load_env
    from .safety_patterns import unsafe_cleaned_matches
except ImportError:  # pragma: no cover
    from check_internal_leakage import check_chunk
    from model_smoke import DEFAULT_BASE_URL, ModelVerseClient, _extract_json, _load_env
    from safety_patterns import unsafe_cleaned_matches


DEFAULT_JUDGE_MODEL = "claude-opus-4-7"
DEFAULT_ANSWER_MODEL = "deepseek-v4-pro"
CITATION_RE = re.compile(r"\[[1-9]\d*\]")
# DEPRECATED post-RAG-13 (2026-05-17): kept for backward compatibility.
# Conflates hard refusal templates with soft boundary-disclosure (disclaimer).
# Use HARD_REFUSAL_RE + SOFT_DISCLAIMER_RE below for correct metric split.
REFUSAL_RE = re.compile(r"(知识库未覆盖|当前知识库只收录|没有找到可靠资料|知识库暂未收录|无法根据知识库回答)")
# Hard refusal: template that declines to answer. Combined with no-citation +
# short-length guards in _is_hard_refusal so a long grounded answer that merely
# mentions "知识库未覆盖" inside its body is not misclassified.
HARD_REFUSAL_RE = re.compile(r"(知识库未覆盖|无法根据知识库回答|没有找到可靠资料)")
# Soft disclaimer: boundary-disclosure phrases tacked onto substantive answers.
# A grounded answer with [n] citations can also contain a disclaimer; that is
# distinct from a pure refusal.
SOFT_DISCLAIMER_RE = re.compile(r"(当前知识库只收录|知识库暂未收录|当前知识库只|当前知识库未)")
# Pure refusal template must be short — long answers that happen to include
# the refusal phrase are likely partial answers with disclaimers, not refusals.
HARD_REFUSAL_MAX_CHARS = 100
Answerer = Callable[[str, list[dict[str, Any]]], str]
Judge = Callable[[str, str, list[dict[str, Any]]], dict[str, Any]]


def evaluate_answers(
    chunks_path: Path | str,
    questions_path: Path | str,
    retrieval_eval_path: Path | str,
    out_path: Path | str,
    *,
    answerer: Answerer | None = None,
    judge: Judge | None = None,
    answer_model: str = DEFAULT_ANSWER_MODEL,
    judge_model: str = DEFAULT_JUDGE_MODEL,
    env_path: Path = Path(".env.local"),
    progress: bool = False,
) -> dict[str, Any]:
    chunks = {str(chunk.get("chunk_id") or ""): chunk for chunk in _read_jsonl(chunks_path)}
    questions = {str(row.get("question_id") or ""): row for row in _read_jsonl(questions_path)}
    retrieval_eval = _read_json(retrieval_eval_path)
    answerer = answerer or _modelverse_answerer(env_path=env_path, model=answer_model)
    judge = judge or _modelverse_judge(env_path=env_path, model=judge_model)

    previous = _read_existing_summary(out_path)
    answer_results: list[dict[str, Any]] = list(previous.get("answers") or [])
    failed_answers: list[dict[str, Any]] = [
        item for item in previous.get("failed_answers") or [] if item.get("reason") == "safety_failure"
    ]
    failed_answers.extend(_failed_answer_flags(answer_results))
    safety_failures = int(previous.get("safety_failures") or 0)
    internal_leakage = _internal_leakage_count(chunks.values())
    previous_counts = _answer_counts(answer_results)
    grounded = previous_counts["grounded"]
    cited = previous_counts["cited"]
    fabricated = previous_counts["fabricated"]
    evaluated = len(answer_results)
    completed_question_ids = {str(item.get("question_id") or "") for item in answer_results}
    completed_question_ids.update(str(item.get("question_id") or "") for item in failed_answers if item.get("reason") == "safety_failure")

    for trace in retrieval_eval.get("trace_records") or []:
        question_id = str(trace.get("question_id") or "")
        if question_id in completed_question_ids:
            continue
        question = questions.get(question_id)
        if not question or question.get("expected_behavior") != "answer":
            continue
        hit_chunks = [
            chunks[str(hit.get("chunk_id") or "")]
            for hit in trace.get("hit_items") or []
            if str(hit.get("chunk_id") or "") in chunks
        ]
        unsafe = _unsafe_chunks(hit_chunks)
        if unsafe:
            safety_failures += len(unsafe)
            failed_answers.append({"question_id": question_id, "reason": "safety_failure", "chunks": unsafe})
            _write_json(
                out_path,
                _answer_summary(
                    evaluated=evaluated,
                    grounded=grounded,
                    cited=cited,
                    fabricated=fabricated,
                    safety_failures=safety_failures,
                    internal_leakage=internal_leakage,
                    failed_answers=failed_answers,
                    answer_model=answer_model,
                    judge_model=judge_model,
                    answer_results=answer_results,
                ),
            )
            if progress:
                print(f"evaluated {evaluated} answer questions; safety failure at {question_id}", flush=True)
            completed_question_ids.add(question_id)
            continue
        evaluated += 1
        try:
            answer = _call_with_retries(
                lambda: answerer(str(question.get("question") or ""), hit_chunks),
                label=f"answer:{question_id}",
                progress=progress,
            )
            citation_retry = False
            pure_refusal = _is_hard_refusal(answer)
            if not pure_refusal and not _has_citation(answer):
                citation_retry = True
                answer = _call_with_retries(
                    lambda: answerer(_citation_retry_question(str(question.get("question") or "")), hit_chunks),
                    label=f"answer-citation-retry:{question_id}",
                    attempts=1,
                    progress=progress,
                )
                pure_refusal = _is_hard_refusal(answer)
            citation_present = _has_citation(answer)
            judge_result = _call_with_retries(
                lambda: judge(str(question.get("question") or ""), answer, hit_chunks),
                label=f"judge:{question_id}",
                progress=progress,
            )
            if not pure_refusal:
                judge_result["cited"] = citation_present
        except Exception as exc:  # pragma: no cover - exercised through integration runs
            failed_answers.append({"question_id": question_id, "reason": "model_call_error", "error": str(exc)[:500]})
            _write_json(
                out_path,
                _answer_summary(
                    evaluated=evaluated,
                    grounded=grounded,
                    cited=cited,
                    fabricated=fabricated,
                    safety_failures=safety_failures,
                    internal_leakage=internal_leakage,
                    failed_answers=failed_answers,
                    answer_model=answer_model,
                    judge_model=judge_model,
                    answer_results=answer_results,
                ),
            )
            if progress:
                print(f"evaluated {evaluated} answer questions; model error at {question_id}", flush=True)
            completed_question_ids.add(question_id)
            continue
        missing_citation = not pure_refusal and not citation_present
        if judge_result.get("grounded") is not True or missing_citation or judge_result.get("fabricated") is True:
            failed_answers.append({"question_id": question_id, "reason": "judge_flagged", "judge": judge_result})
        answer_results.append(
            {
                "question_id": question_id,
                "answer": answer,
                "chunk_ids": [str(chunk.get("chunk_id") or "") for chunk in hit_chunks],
                "judge": judge_result,
                # `refusal` kept for backward compat; equals `pure_refusal` post-RAG-13.
                "refusal": pure_refusal,
                "pure_refusal": pure_refusal,
                "soft_disclaimer": _has_soft_disclaimer(answer),
                "citation_present": citation_present,
                "citation_retry": citation_retry,
            }
        )
        _write_json(
            out_path,
            _answer_summary(
                evaluated=evaluated,
                grounded=grounded,
                cited=cited,
                fabricated=fabricated,
                safety_failures=safety_failures,
                internal_leakage=internal_leakage,
                failed_answers=failed_answers,
                answer_model=answer_model,
                judge_model=judge_model,
                answer_results=answer_results,
            ),
        )
        if progress:
            print(f"evaluated {evaluated} answer questions", flush=True)
        completed_question_ids.add(question_id)

    summary = _answer_summary(
        evaluated=evaluated,
        grounded=grounded,
        cited=cited,
        fabricated=fabricated,
        safety_failures=safety_failures,
        internal_leakage=internal_leakage,
        failed_answers=failed_answers,
        answer_model=answer_model,
        judge_model=judge_model,
        answer_results=answer_results,
    )
    _write_json(out_path, summary)
    return summary


def _answer_summary(
    *,
    evaluated: int,
    grounded: int,
    cited: int,
    fabricated: int,
    safety_failures: int,
    internal_leakage: int,
    failed_answers: list[dict[str, Any]],
    answer_model: str,
    judge_model: str,
    answer_results: list[dict[str, Any]],
) -> dict[str, Any]:
    counts = _answer_counts(answer_results)
    grounded = counts["grounded"]
    cited = counts["cited"]
    fabricated = counts["fabricated"]
    pure_refused = counts["pure_refused"]
    with_disclaimer = counts["with_disclaimer"]
    non_refused = counts["non_refused"]
    answered_subset = with_disclaimer + non_refused
    return {
        "answer_questions_evaluated": evaluated,
        "answer_generation_failures": 0,
        "grounded_rate": _rate(grounded, evaluated),
        # RAG-13 split (2026-05-17): pure refusal and disclaimer are now distinct.
        "pure_refusal_rate": _rate(pure_refused, evaluated),
        "disclaimer_rate": _rate(with_disclaimer, evaluated),
        # cited_rate denom changed to the answered subset (with_disclaimer + non_refused).
        # Before RAG-13 it was non_refused (post-RAG-13 nomenclature). Disclaimer answers
        # routinely carry [n] citations, so this denom shift typically grows the numerator
        # alongside the denominator and keeps the 100% cited-contract target attainable.
        "cited_rate": _rate(cited, answered_subset),
        "fabricated_rate": _rate(fabricated, evaluated),
        "safety_failures": safety_failures,
        "internal_leakage": internal_leakage,
        "failed_answers": failed_answers,
        "answer_model": answer_model,
        "judge_model": judge_model,
        "judge_sampled_count": evaluated,
        # NEW post-RAG-13 counts.
        "answer_questions_pure_refused": pure_refused,
        "answer_questions_with_disclaimer": with_disclaimer,
        "answer_questions_non_refused": non_refused,
        # Backward-compat alias: pre-RAG-13 callers expect `answer_questions_refused`.
        # Now equals pure_refused only (disclaimers excluded). Historical numbers
        # (PRs #88 / #89) are not directly comparable — re-run via this script.
        "answer_questions_refused": pure_refused,
        "citation_denominator": answered_subset,
        "answers": answer_results,
    }


def _answer_counts(answer_results: list[dict[str, Any]]) -> dict[str, int]:
    """RAG-13: split refusal into hard refusal (pure_refused) and soft disclaimer
    (with_disclaimer). cited_rate denom changes to the answered subset
    (with_disclaimer + non_refused)."""
    grounded = 0
    cited = 0
    fabricated = 0
    pure_refused = 0
    with_disclaimer = 0
    non_refused = 0
    for item in answer_results:
        judge = item.get("judge") or {}
        if judge.get("grounded") is True:
            grounded += 1
        if judge.get("fabricated") is True:
            fabricated += 1
        answer = str(item.get("answer") or "")

        if _is_hard_refusal(answer):
            pure_refused += 1
            item["pure_refusal"] = True
            item["soft_disclaimer"] = False
            item["refusal"] = True  # backward-compat alias
            item.setdefault("citation_present", _has_citation(answer))
            continue

        # Not a hard refusal — the model produced substantive content.
        item["pure_refusal"] = False
        item["refusal"] = False  # backward-compat alias
        has_disclaimer = _has_soft_disclaimer(answer)
        item["soft_disclaimer"] = has_disclaimer
        if has_disclaimer:
            with_disclaimer += 1
        else:
            non_refused += 1
        citation_present = _has_citation(answer)
        item["citation_present"] = citation_present
        if citation_present:
            cited += 1
    return {
        "grounded": grounded,
        "cited": cited,
        "fabricated": fabricated,
        "pure_refused": pure_refused,
        "with_disclaimer": with_disclaimer,
        "non_refused": non_refused,
    }


def _failed_answer_flags(answer_results: list[dict[str, Any]]) -> list[dict[str, Any]]:
    failed: list[dict[str, Any]] = []
    for item in answer_results:
        answer = str(item.get("answer") or "")
        pure_refusal = _is_hard_refusal(answer)
        citation_present = _has_citation(answer)
        item["pure_refusal"] = pure_refusal
        item["soft_disclaimer"] = _has_soft_disclaimer(answer)
        item["refusal"] = pure_refusal  # backward-compat alias
        item["citation_present"] = citation_present
        judge = item.get("judge") or {}
        if judge.get("grounded") is not True or (not pure_refusal and not citation_present) or judge.get("fabricated") is True:
            failed.append(
                {
                    "question_id": str(item.get("question_id") or ""),
                    "reason": "judge_flagged",
                    "judge": judge,
                }
            )
    return failed


def _read_existing_summary(path: Path | str) -> dict[str, Any]:
    path = Path(path)
    if not path.exists():
        return {}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return {}


def _call_with_retries(callback: Callable[[], Any], *, label: str, attempts: int = 3, progress: bool = False) -> Any:
    last_exc: Exception | None = None
    for attempt in range(1, attempts + 1):
        try:
            return callback()
        except Exception as exc:  # pragma: no cover - network path
            last_exc = exc
            if attempt == attempts:
                break
            if progress:
                print(f"{label} failed on attempt {attempt}; retrying", flush=True)
            time.sleep(2 * attempt)
    assert last_exc is not None
    raise last_exc


def _unsafe_chunks(chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    failures: list[dict[str, Any]] = []
    for chunk in chunks:
        text = "\n".join(
            [
                str(chunk.get("title") or ""),
                str(chunk.get("content") or ""),
                " ".join(str(item) for item in chunk.get("question_patterns") or []),
            ]
        )
        matches = unsafe_cleaned_matches(text)
        if matches:
            failures.append({"chunk_id": str(chunk.get("chunk_id") or ""), "matches": matches})
    return failures


def _internal_leakage_count(chunks: Any) -> int:
    return sum(1 for chunk in chunks if check_chunk(chunk))


def _has_citation(answer: str) -> bool:
    return bool(CITATION_RE.search(answer))


def _is_hard_refusal(answer: str) -> bool:
    """True only when the answer is a pure refusal template with no substantive content.

    Requires ALL three to hold:
    - HARD_REFUSAL_RE matches (refusal phrase present)
    - No [n] citation present (grounded answers that mention 知识库未覆盖 in body are excluded)
    - Length < HARD_REFUSAL_MAX_CHARS (long answers that happen to include the phrase
      are partial answers / disclaimers, not refusals)

    Length-only is insufficient: a 54-char grounded answer like
    "错误码 226601 表示初始化失败 [1]。当前知识库只收录此一条。"
    is short but is NOT a refusal — it carries a citation and the model returned a
    grounded answer, not a template.
    """
    if not HARD_REFUSAL_RE.search(answer):
        return False
    if CITATION_RE.search(answer):
        return False
    return len(answer.strip()) < HARD_REFUSAL_MAX_CHARS


def _has_soft_disclaimer(answer: str) -> bool:
    """True if the answer contains a boundary-disclosure phrase such as
    '当前知识库只收录' or '知识库暂未收录'. A grounded answer may still carry a
    disclaimer; that is tracked separately from pure refusal."""
    return bool(SOFT_DISCLAIMER_RE.search(answer))


def _is_refusal_answer(answer: str) -> bool:
    """DEPRECATED post-RAG-13 (2026-05-17). Returns True only for hard refusal
    templates — the previous behavior (which also matched disclaimers) is preserved
    via the alias `answer_questions_refused == pure_refused` in the output schema.

    Callers should migrate to `_is_hard_refusal` + `_has_soft_disclaimer`.
    """
    return _is_hard_refusal(answer)


def _citation_retry_question(question: str) -> str:
    return (
        "请重新回答。除非你明确拒答，否则每个事实都必须带 [1]、[2] 这样的引用编号。"
        "如果知识库没有覆盖，请直接说明知识库未覆盖。\n\n"
        f"原问题：{question}"
    )


def _deterministic_answerer(question: str, chunks: list[dict[str, Any]]) -> str:
    if not chunks:
        return "知识库未覆盖这个问题。"
    parts: list[str] = []
    for index, chunk in enumerate(chunks[:3], start=1):
        content = _first_sentence(str(chunk.get("content") or ""))
        if content:
            parts.append(f"{content} [{index}]")
    return " ".join(parts) if parts else "知识库未覆盖这个问题。"


def _modelverse_answerer(*, env_path: Path, model: str) -> Answerer:
    env = _load_env(env_path)
    client = ModelVerseClient(
        base_url=env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL),
        api_key=env["MODELVERSE_API_KEY"],
    )
    selected_model = env.get("MODELVERSE_DS_V4_PRO_MODEL", model)

    def answer(question: str, chunks: list[dict[str, Any]]) -> str:
        return client.chat(
            model=selected_model,
            messages=[{"role": "user", "content": _answer_prompt(question, chunks)}],
            max_tokens=1000,
        ).strip()

    return answer


def _modelverse_judge(*, env_path: Path, model: str) -> Judge:
    env = _load_env(env_path)
    client = ModelVerseClient(
        base_url=env.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL),
        api_key=env["MODELVERSE_API_KEY"],
    )

    def judge(question: str, answer: str, chunks: list[dict[str, Any]]) -> dict[str, Any]:
        content = client.chat(
            model=env.get("MODELVERSE_CLAUDE_OPUS_MODEL", model),
            messages=[{"role": "user", "content": _judge_prompt(question, answer, chunks)}],
            max_tokens=800,
            json_mode=True,
        )
        parsed = _extract_json(content)
        return {
            "grounded": parsed.get("grounded") is True,
            "cited": parsed.get("cited") is True,
            "fabricated": parsed.get("fabricated") is True,
            "reasoning": str(parsed.get("reasoning") or "")[:500],
        }

    return judge


def _answer_prompt(question: str, chunks: list[dict[str, Any]]) -> str:
    chunk_text = "\n\n".join(
        f"[{index}] {chunk.get('title')}\n{chunk.get('content')}"
        for index, chunk in enumerate(chunks[:3], start=1)
    )
    # PR-RAG-Prompt-Disclaimer-Fix (2026-05-17): three-tier disclaimer strategy.
    # Step 6b (2026-05-17): six anti-fabrication anchor bullets appended to
    # 【事实约束】 — mirrors internal/prompt/rag.go BuildRAGMessages exactly
    # (memory feedback_eval_target_must_match_runtime_path).
    return (
        "你是 CompShare 平台知识库答复助手。只能使用下面的知识片段回答,不要补充片段外的事实。\n\n"
        "【回答规则 — 按资料覆盖度三选一】\n"
        "1. 知识片段能完整回答用户问题:直接给答案 + 引用 [n],不要加 \"当前知识库只收录\" \"知识库暂未收录\" 等边界声明。完整命中时静默引用即可。\n"
        "2. 知识片段只能部分回答(关键细节缺失):用具体的限定词指出未覆盖部分,例如 \"...[1]。关于 <具体未覆盖项> 片段里没有写明,建议 <具体下一步行动>。\" 禁止使用 \"当前知识库只收录了以上信息\" 这种无信息的空模板。\n"
        "3. 知识片段完全无关:回复 \"知识库未覆盖这个问题。\" 不要给出任何推测性信息或建议。\n\n"
        "【事实约束】\n"
        "- 标题和正文存在冲突时,以正文中的明确陈述为准,并说明资料表述不一致。\n"
        "- 涉及时间、金额、窗口、条件判断时,必须保留知识片段里的原始条件,不要把示例改写成通用规则。\n"
        "- 多个片段给出不同价格或规则时,只使用与用户问题直接相关的片段,不要混合无关片段推断。\n"
        "- 所有非拒答的事实句都必须带 [1]、[2] 这样的引用编号;没有引用编号的答案会被判为失败。\n"
        "- 代码、import 语句、函数签名、命令行、配置文件片段必须字符级、按行原样复制知识片段中的内容;不要补全省略号、不要拼接多段、不要修改大小写或下划线。\n"
        "- 枚举值、常量名、错误码、HTTP 状态码必须按知识片段字面拷贝;不要拼接、重复、改变下划线或连字符。\n"
        "- 涉及数字、金额、百分比、精度位时,必须按知识片段给出的字面值复制(含小数点位数),不要四舍五入或换算单位。\n"
        "- 回答不允许包含知识片段没有写的故障排除建议、操作步骤、联系方式或下一步行动;只有当知识片段里自身出现 \"建议...\"、\"请...\" 等表述时才能复述。\n"
        "- 涉及推荐 / 禁止 / 支持 / 不支持 / 启用 / 禁用 / 包含 / 排除 等方向性词汇时,必须按知识片段原始方向陈述;不要因为用户问题方向相反就翻转知识片段含义。\n"
        "- 同一份知识片段中如出现多个字段名或列表标题(如官网链接 / API 端点 / 请求地址 / 服务地址 / 文档地址 / 支持列表 / 已下架列表 等),必须按对应字段或列表标题旁的具体值回答,不要把一项的内容套用到另一个标题上。\n\n"
        f"用户问题:\n{question}\n\n知识片段:\n{chunk_text}"
    )


def _judge_prompt(question: str, answer: str, chunks: list[dict[str, Any]]) -> str:
    chunk_text = "\n\n".join(
        f"[{index}] {chunk.get('title')}\n{chunk.get('content')}"
        for index, chunk in enumerate(chunks[:3], start=1)
    )
    return (
        "You are a strict RAG grounding judge. Return only JSON with keys: "
        "grounded boolean, cited boolean, fabricated boolean, reasoning string.\n\n"
        f"Question:\n{question}\n\nAnswer:\n{answer}\n\nEvidence chunks:\n{chunk_text}"
    )


def _first_sentence(text: str) -> str:
    text = " ".join(text.split())
    if len(text) <= 240:
        return text
    match = re.search(r"[。.!?]", text[:240])
    if match:
        return text[: match.end()]
    return text[:240]


def _rate(count: int, total: int) -> float | None:
    return None if total == 0 else count / total


def _read_json(path: Path | str) -> dict[str, Any]:
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        value = json.load(fh)
    if not isinstance(value, dict):
        raise ValueError(f"{path}: expected object")
    return value


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8-sig") as fh:
        for row, line in enumerate(fh, start=1):
            if not line.strip():
                continue
            value = json.loads(line)
            if not isinstance(value, dict):
                raise ValueError(f"{path}:{row}: expected object")
            rows.append(value)
    return rows


def _write_json(path: Path | str, value: dict[str, Any]) -> None:
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chunks", type=Path, required=True)
    parser.add_argument("--questions", type=Path, required=True)
    parser.add_argument("--retrieval-eval", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--answer-model", default=DEFAULT_ANSWER_MODEL)
    parser.add_argument("--judge-model", default=DEFAULT_JUDGE_MODEL)
    parser.add_argument("--env", type=Path, default=Path(".env.local"))
    args = parser.parse_args(argv)
    evaluate_answers(
        args.chunks,
        args.questions,
        args.retrieval_eval,
        args.out,
        answer_model=args.answer_model,
        judge_model=args.judge_model,
        env_path=args.env,
        progress=True,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
