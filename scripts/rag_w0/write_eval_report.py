#!/usr/bin/env python3
"""Write the W0 eval report and conditionally promote chunks to deploy/kb."""

from __future__ import annotations

import argparse
from datetime import date
import json
from pathlib import Path
from typing import Any

try:
    from .check_internal_leakage import check_internal_leakage
    from .verify_eval_questions import verify_eval_questions
except ImportError:  # pragma: no cover
    from check_internal_leakage import check_internal_leakage
    from verify_eval_questions import verify_eval_questions


def write_eval_report(
    *,
    chunks_path: Path | str,
    questions_path: Path | str,
    retrieval_eval_path: Path | str,
    answer_eval_path: Path | str,
    report_path: Path | str,
    deploy_path: Path | str | None = None,
    needs_split_path: Path | str | None = None,
    needs_review_path: Path | str | None = None,
    min_top3: float = 0.60,
    min_faithfulness: float = 0.80,
    min_cited: float = 1.0,
) -> dict[str, Any]:
    chunks_path = Path(chunks_path)
    questions_path = Path(questions_path)
    retrieval_eval_path = Path(retrieval_eval_path)
    answer_eval_path = Path(answer_eval_path)
    report_path = Path(report_path)
    chunks = _read_jsonl(chunks_path)
    questions = _read_jsonl(questions_path)
    retrieval_eval = _read_json(retrieval_eval_path)
    answer_eval = _read_json(answer_eval_path)
    needs_split = _read_optional_jsonl(needs_split_path)
    needs_review = _read_optional_jsonl(needs_review_path)
    pillar0 = _pillar0_summary(questions_path, chunks_path)
    leakage = check_internal_leakage(chunks_path)
    anchor = _anchor_summary(questions, retrieval_eval)

    gates = {
        "pillar0_no_tautology": pillar0["passed"],
        "anchor_top3": anchor["passed"],
        "distribution_top3": float(retrieval_eval.get("top_3_hit_rate") or 0) >= min_top3,
        "safety_failures": int(answer_eval.get("safety_failures") or 0) == 0,
        "internal_leakage": int(leakage.get("flagged_count") or 0) == 0,
        "faithfulness": float(answer_eval.get("grounded_rate") or 0) >= min_faithfulness,
        "cited": float(answer_eval.get("cited_rate") or 0) >= min_cited,
        "fabricated": float(answer_eval.get("fabricated_rate") or 0) == 0,
    }
    passed = all(gates.values())

    report = _render_report(
        chunks_path=chunks_path,
        questions_path=questions_path,
        chunks=chunks,
        questions=questions,
        retrieval_eval=retrieval_eval,
        answer_eval=answer_eval,
        needs_split=needs_split,
        needs_review=needs_review,
        pillar0=pillar0,
        anchor=anchor,
        leakage=leakage,
        gates=gates,
        passed=passed,
        min_top3=min_top3,
        min_faithfulness=min_faithfulness,
        min_cited=min_cited,
    )
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(report, encoding="utf-8")

    if deploy_path:
        deploy_path = Path(deploy_path)
        if passed:
            deploy_path.parent.mkdir(parents=True, exist_ok=True)
            deploy_path.write_text(chunks_path.read_text(encoding="utf-8"), encoding="utf-8")
        elif deploy_path.exists():
            deploy_path.unlink()

    return {
        "passed": passed,
        "gates": gates,
        "pillar0": pillar0,
        "anchor": anchor,
        "internal_leakage": leakage,
        "chunk_count": len(chunks),
        "question_count": len(questions),
        "needs_split_count": len(needs_split),
        "needs_review_count": len(needs_review),
    }


def _render_report(
    *,
    chunks_path: Path,
    questions_path: Path,
    chunks: list[dict[str, Any]],
    questions: list[dict[str, Any]],
    retrieval_eval: dict[str, Any],
    answer_eval: dict[str, Any],
    needs_split: list[dict[str, Any]],
    needs_review: list[dict[str, Any]],
    pillar0: dict[str, Any],
    anchor: dict[str, Any],
    leakage: dict[str, Any],
    gates: dict[str, bool],
    passed: bool,
    min_top3: float,
    min_faithfulness: float,
    min_cited: float,
) -> str:
    area_counts: dict[str, int] = {}
    for chunk in chunks:
        area = str(chunk.get("product_area") or "unknown")
        area_counts[area] = area_counts.get(area, 0) + 1
    behavior_counts: dict[str, int] = {}
    for question in questions:
        behavior = str(question.get("expected_behavior") or "unknown")
        behavior_counts[behavior] = behavior_counts.get(behavior, 0) + 1
    failed_gates = [name for name, ok in gates.items() if not ok]

    lines = [
        f"# Stage 2B W0 Eval Report - {date.today().isoformat()}",
        "",
        f"Verdict: **{'PASS' if passed else 'FAIL'}**",
        f"Deploy: **{'WRITTEN' if passed else 'SKIPPED'}**",
        f"Failed gates: {', '.join(failed_gates) if failed_gates else 'None'}",
        "",
        "## Inputs",
        f"- Chunks: `{_display_path(chunks_path)}` ({len(chunks)} chunks)",
        f"- Questions: `{_display_path(questions_path)}` ({len(questions)} questions)",
        "",
        "## Gates",
        f"- Pillar 0 no tautology: {int(pillar0.get('violations') or 0)} violations - {'PASS' if gates['pillar0_no_tautology'] else 'FAIL'}",
        f"- Anchor Top-3 hit rate: {_percent(anchor.get('hit_rate'))} ({int(anchor.get('hit') or 0)}/{int(anchor.get('total') or 0)}) - {'PASS' if gates['anchor_top3'] else 'FAIL'}",
        f"- Distribution Top-3 hit rate: {_percent(retrieval_eval.get('top_3_hit_rate'))} (threshold {_percent(min_top3)}) - {'PASS' if gates['distribution_top3'] else 'FAIL'}",
        f"- Safety failures: {int(answer_eval.get('safety_failures') or 0)} - {'PASS' if gates['safety_failures'] else 'FAIL'}",
        f"- Internal leakage: {int(leakage.get('flagged_count') or 0)} flagged chunks - {'PASS' if gates['internal_leakage'] else 'FAIL'}",
        f"- Answer faithfulness: {_percent(answer_eval.get('grounded_rate'))} (threshold {_percent(min_faithfulness)}) - {'PASS' if gates['faithfulness'] else 'FAIL'}",
        f"- Answer citation rate: {_percent(answer_eval.get('cited_rate'))} over non-refusal answers (threshold {_percent(min_cited)}) - {'PASS' if gates['cited'] else 'FAIL'}",
        f"- Fabricated rate: {_percent(answer_eval.get('fabricated_rate'))} (threshold 0.0%) - {'PASS' if gates['fabricated'] else 'FAIL'}",
        "",
        "## Retrieval",
        f"- Evaluated answer questions: {int(retrieval_eval.get('questions_evaluated') or 0)}",
        f"- Excluded non-answer questions: {int(retrieval_eval.get('questions_excluded_non_answer_behavior') or 0)}",
        f"- Failed retrieval questions: {len(retrieval_eval.get('failed_questions') or [])}",
        f"- Failed anchor question ids: {', '.join(anchor.get('failed_question_ids') or []) or 'None'}",
        "- Per-group hit rate:",
    ]
    for group, rate in sorted((retrieval_eval.get("per_group_hit_rate") or {}).items()):
        value = rate.get("hit_rate") if isinstance(rate, dict) else rate
        suffix = f" ({rate.get('hit')}/{rate.get('total')})" if isinstance(rate, dict) else ""
        lines.append(f"  - `{group}`: {_percent(value)}{suffix}")
    lines.extend(
        [
            "",
            "## Answer Judge",
            f"- Answer questions evaluated: {int(answer_eval.get('answer_questions_evaluated') or 0)}",
            f"- Refusal answers: {int(answer_eval.get('answer_questions_refused') or 0)}",
            f"- Non-refusal citation denominator: {int(answer_eval.get('citation_denominator') or answer_eval.get('answer_questions_non_refused') or 0)}",
            f"- Grounded rate: {_percent(answer_eval.get('grounded_rate'))}",
            f"- Citation rate: {_percent(answer_eval.get('cited_rate'))}",
            f"- Fabricated rate: {_percent(answer_eval.get('fabricated_rate'))}",
            f"- Answer model: `{answer_eval.get('answer_model') or ''}`",
            f"- Judge model: `{answer_eval.get('judge_model') or ''}`",
            "",
            "## Corpus Distribution",
        ]
    )
    for area, count in sorted(area_counts.items()):
        lines.append(f"- `{area}`: {count}")
    lines.extend(["", "## Question Distribution"])
    for behavior, count in sorted(behavior_counts.items()):
        lines.append(f"- `{behavior}`: {count}")
    lines.extend(["", "## Outstanding needs_split Queue"])
    if needs_split:
        lines.append("| source_ref | section_index | section_title | reason |")
        lines.append("|---|---|---|---|")
        for row in needs_split:
            key = row.get("key") if isinstance(row.get("key"), dict) else {}
            lines.append(
                "| "
                + " | ".join(
                    [
                        _cell(row.get("source_ref") or key.get("source_doc_id")),
                        _cell(key.get("section_index")),
                        _cell(row.get("section_title")),
                        _cell(row.get("reasoning") or row.get("empty_label_reason")),
                    ]
                )
                + " |"
            )
    else:
        lines.append("- None")
    lines.extend(["", "## needs_review Queue"])
    lines.append(f"- Candidate chunks carrying low-confidence labels: {len(needs_review)}")
    lines.extend(["", "## Failed Answers"])
    failed_answers = answer_eval.get("failed_answers") or []
    if failed_answers:
        for item in failed_answers:
            lines.append(f"- `{item.get('question_id')}`: {item.get('reason')}")
    else:
        lines.append("- None")
    lines.extend(["", "## Internal Leakage Findings"])
    flagged = leakage.get("flagged") or []
    if flagged:
        for item in flagged:
            lines.append(f"- `{item.get('chunk_id')}`: {', '.join(str(x) for x in item.get('findings') or [])}")
    else:
        lines.append("- None")
    lines.append("")
    return "\n".join(lines)


def _pillar0_summary(questions_path: Path, chunks_path: Path) -> dict[str, Any]:
    try:
        result = verify_eval_questions(questions_path, chunks_path)
        return {"passed": True, "violations": int(result.get("violations") or 0)}
    except ValueError as exc:
        return {"passed": False, "violations": 1, "error": str(exc)}


def _anchor_summary(questions: list[dict[str, Any]], retrieval_eval: dict[str, Any]) -> dict[str, Any]:
    anchor_ids = {str(question.get("question_id") or "") for question in questions if question.get("is_anchor") is True}
    if not anchor_ids:
        return {"passed": False, "hit": 0, "total": 0, "hit_rate": 0.0, "failed_question_ids": []}
    failed = {str(item.get("question_id") or "") for item in retrieval_eval.get("failed_questions") or []}
    failed_anchor_ids = sorted(anchor_ids.intersection(failed))
    hit = len(anchor_ids) - len(failed_anchor_ids)
    return {
        "passed": not failed_anchor_ids and hit == len(anchor_ids),
        "hit": hit,
        "total": len(anchor_ids),
        "hit_rate": hit / len(anchor_ids),
        "failed_question_ids": failed_anchor_ids,
    }


def _read_json(path: Path | str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def _read_jsonl(path: Path | str) -> list[dict[str, Any]]:
    return [json.loads(line) for line in Path(path).read_text(encoding="utf-8").splitlines() if line.strip()]


def _read_optional_jsonl(path: Path | str | None) -> list[dict[str, Any]]:
    if not path:
        return []
    path = Path(path)
    if not path.exists():
        return []
    return _read_jsonl(path)


def _percent(value: Any) -> str:
    try:
        return f"{float(value) * 100:.1f}%"
    except (TypeError, ValueError):
        return "0.0%"


def _display_path(path: Path) -> str:
    parts = path.as_posix().split("/")
    if "compshare-agent-runs" in parts:
        index = parts.index("compshare-agent-runs")
        return "<runs>/" + "/".join(parts[index + 1 :])
    return path.as_posix()


def _cell(value: Any) -> str:
    text = " ".join(str(value or "").split())
    text = text.replace("|", "\\|")
    return text[:120]


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chunks", type=Path, required=True)
    parser.add_argument("--questions", type=Path, required=True)
    parser.add_argument("--retrieval-eval", type=Path, required=True)
    parser.add_argument("--answer-eval", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--deploy", type=Path)
    parser.add_argument("--needs-split", type=Path)
    parser.add_argument("--needs-review", type=Path)
    parser.add_argument("--min-top3", type=float, default=0.60)
    parser.add_argument("--min-faithfulness", type=float, default=0.80)
    parser.add_argument("--min-cited", type=float, default=1.0)
    args = parser.parse_args()
    summary = write_eval_report(
        chunks_path=args.chunks,
        questions_path=args.questions,
        retrieval_eval_path=args.retrieval_eval,
        answer_eval_path=args.answer_eval,
        report_path=args.report,
        deploy_path=args.deploy,
        needs_split_path=args.needs_split,
        needs_review_path=args.needs_review,
        min_top3=args.min_top3,
        min_faithfulness=args.min_faithfulness,
        min_cited=args.min_cited,
    )
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
