#!/usr/bin/env python3
"""Aggregate trace.v0.x planner-vs-runtime signals into a Markdown report."""

from __future__ import annotations

import argparse
from collections import Counter
import json
from pathlib import Path
from typing import Any, Iterable


MONITOR_INTENTS = {"monitor_query", "monitor_history"}
ACCOUNT_HARDBLOCK_INTENT = "billing_account_unsupported"
BOUNDARY_INTENTS = {
    "billing_instance",
    "diagnosis",
    "operation_lifecycle",
    "knowledge_qa",
    # Legacy trace compatibility: new planner prompts no longer emit mixed_*,
    # but existing trace files and dashboards may still contain these labels.
    "mixed_diagnosis_kb",
    "mixed_billing_kb",
}
LEGACY_MIXED_BOUNDARY_INTENTS = {"mixed_diagnosis_kb", "mixed_billing_kb"}
MIXED_BOUNDARY_COMPAT_INTENTS = {
    "billing_instance",
    "operation_lifecycle",
    "knowledge_qa",
    *LEGACY_MIXED_BOUNDARY_INTENTS,
}
MONITOR_ACTION = "GetCompShareInstanceMonitor"


def load_records(path: Path | str) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8") as fh:
        for line_no, line in enumerate(fh, start=1):
            line = line.strip()
            if not line:
                continue
            try:
                records.append(json.loads(line))
            except json.JSONDecodeError as exc:
                raise ValueError(f"{path}:{line_no}: invalid JSONL: {exc}") from exc
    return records


def summarize(records: Iterable[dict[str, Any]]) -> dict[str, Any]:
    rows = list(records)
    planner_enabled = [r for r in rows if _planner(r).get("enabled")]
    schema_valid = [r for r in planner_enabled if _planner(r).get("schema_valid")]
    intent_counts = Counter(_planner(r).get("intent") or "(empty)" for r in planner_enabled)
    monitor_misses = [r for r in planner_enabled if _monitor_freshness_miss(r)]
    hardblock = _account_hardblock_summary(planner_enabled)
    boundary = Counter(
        _planner(r).get("intent")
        for r in planner_enabled
        if _planner(r).get("intent") in BOUNDARY_INTENTS
    )
    legacy_mixed_boundary = Counter(
        _planner(r).get("intent")
        for r in planner_enabled
        if _planner(r).get("intent") in MIXED_BOUNDARY_COMPAT_INTENTS
    )
    return {
        "total_turns": len(rows),
        "planner_enabled_turns": len(planner_enabled),
        "schema_valid_turns": len(schema_valid),
        "schema_valid_rate": _rate(len(schema_valid), len(planner_enabled)),
        "invalid_or_fallback_turns": len(planner_enabled) - len(schema_valid),
        "engine_hard_block_count": sum(1 for r in rows if _hardblock(r).get("hit")),
        "account_hard_block": hardblock,
        "monitor_freshness_miss_count": len(monitor_misses),
        "monitor_freshness_misses": [_record_id(r) for r in monitor_misses],
        "intent_counts": dict(sorted(intent_counts.items())),
        "boundary_counts": dict(sorted(boundary.items())),
        # Backward-compatible alias for older consumers that still read the
        # pre-cleanup mixed boundary metric.
        "mixed_boundary_counts": dict(sorted(legacy_mixed_boundary.items())),
    }


def render_markdown(summary: dict[str, Any], source: Path | str) -> str:
    lines = [
        "# Planner vs Runtime Report",
        "",
        f"Source: `{source}`",
        "",
        "## Summary",
        "",
        "| Metric | Value |",
        "| --- | ---: |",
        f"| Total turns | {summary['total_turns']} |",
        f"| Planner-enabled turns | {summary['planner_enabled_turns']} |",
        f"| Schema-valid rate | {_percent(summary['schema_valid_rate'])} |",
        f"| Invalid/fallback planner turns | {summary['invalid_or_fallback_turns']} |",
        f"| Engine hard-block count | {summary['engine_hard_block_count']} |",
        f"| Monitor freshness misses | {summary['monitor_freshness_miss_count']} |",
        "",
        "## Intent Distribution",
        "",
        "| Intent | Count |",
        "| --- | ---: |",
    ]
    lines.extend(_count_rows(summary["intent_counts"]))
    lines.extend(
        [
            "",
            "## Account Hard-Block Agreement",
            "",
            "| Outcome | Count |",
            "| --- | ---: |",
        ]
    )
    lines.extend(_count_rows(summary["account_hard_block"]))
    lines.extend(
        [
            "",
            "## Monitor Freshness Misses",
            "",
            "| Trace/turn |",
            "| --- |",
        ]
    )
    misses = summary["monitor_freshness_misses"]
    if misses:
        lines.extend(f"| {item} |" for item in misses)
    else:
        lines.append("| none |")
    lines.extend(
        [
            "",
            "## Boundary Outcomes",
            "",
            "| Intent | Count |",
            "| --- | ---: |",
        ]
    )
    lines.extend(_count_rows(summary["boundary_counts"]))
    lines.append("")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path, help="trace JSONL path")
    parser.add_argument("--output", type=Path, help="write Markdown report to this path")
    args = parser.parse_args(argv)

    summary = summarize(load_records(args.input))
    report = render_markdown(summary, args.input)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(report, encoding="utf-8")
    else:
        print(report)
    return 0


def _planner(record: dict[str, Any]) -> dict[str, Any]:
    planner = record.get("planner")
    return planner if isinstance(planner, dict) else {}


def _hardblock(record: dict[str, Any]) -> dict[str, Any]:
    hardblock = record.get("engine_hard_block")
    return hardblock if isinstance(hardblock, dict) else {}


def _monitor_freshness_miss(record: dict[str, Any]) -> bool:
    intent = _planner(record).get("intent")
    return intent in MONITOR_INTENTS and not _has_current_turn_monitor_call(record)


def _has_current_turn_monitor_call(record: dict[str, Any]) -> bool:
    turn_index = record.get("turn_index")
    for call in record.get("tool_calls") or []:
        if call.get("turn_index") == turn_index and call.get("action") == MONITOR_ACTION:
            return True
    return False


def _account_hardblock_summary(records: Iterable[dict[str, Any]]) -> dict[str, int]:
    out = {"matched": 0, "mismatched": 0, "engine_only": 0, "not_applicable": 0}
    for record in records:
        planner = _planner(record)
        planner_wants_block = (
            planner.get("intent") == ACCOUNT_HARDBLOCK_INTENT
            or bool(planner.get("hard_block_hint"))
        )
        engine_hit = bool(_hardblock(record).get("hit"))
        if planner_wants_block and engine_hit:
            out["matched"] += 1
        elif planner_wants_block and not engine_hit:
            out["mismatched"] += 1
        elif not planner_wants_block and engine_hit:
            out["engine_only"] += 1
        else:
            out["not_applicable"] += 1
    return out


def _record_id(record: dict[str, Any]) -> str:
    trace_id = record.get("trace_id") or "(no-trace-id)"
    return f"{trace_id} turn {record.get('turn_index', '?')}"


def _count_rows(counts: dict[str, int]) -> list[str]:
    if not counts:
        return ["| none | 0 |"]
    return [f"| {key} | {value} |" for key, value in counts.items()]


def _rate(numerator: int, denominator: int) -> float:
    if denominator == 0:
        return 0.0
    return numerator / denominator


def _percent(value: float) -> str:
    return f"{value * 100:.2f}%"


if __name__ == "__main__":
    raise SystemExit(main())
