#!/usr/bin/env python3
"""Build a lightweight per-case diff report for HTTP replay runs.

The script compares a manually reviewed baseline markdown file with a current
HTTP replay JSONL. It does not pretend to replace human judgement; it surfaces
stable regression signals so reviewers can focus on the cases most likely to
have changed.
"""

from __future__ import annotations

import argparse
import json
import re
from collections import Counter, defaultdict
from pathlib import Path


STATUS_ORDER = {
    "失败": 0,
    "部分解决": 1,
    "确认前链路通过": 2,
    "通过": 3,
}


def parse_baseline_statuses(path: Path) -> dict[str, str]:
    text = path.read_text(encoding="utf-8")
    statuses: dict[str, str] = {}
    for status in STATUS_ORDER:
        pattern = rf"### {re.escape(status)}(?:（\d+）)?\s*\n\n(?P<body>.*?)(?=\n### |\Z)"
        match = re.search(pattern, text, flags=re.S)
        if not match:
            continue
        for case_id in re.findall(r"`([MN]\d{3})`", match.group("body")):
            statuses[case_id] = status
    return statuses


def read_jsonl(path: Path) -> list[dict]:
    rows: list[dict] = []
    with path.open("r", encoding="utf-8-sig") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            rows.append(json.loads(line))
    return rows


def all_user_text(row: dict) -> str:
    return "\n".join(str(t.get("user", "")) for t in row.get("turns", []))


def all_reply_text(row: dict) -> str:
    replies = [str(t.get("reply", "")) for t in row.get("turns", [])]
    final = str(row.get("final_reply", ""))
    if final:
        replies.append(final)
    return "\n".join(replies)


def all_actions(row: dict) -> list[str]:
    actions: list[str] = []
    for turn in row.get("turns", []):
        for step in turn.get("steps", []) or []:
            action = step.get("action")
            if action:
                actions.append(str(action))
    return actions


def confirmation_count(row: dict) -> int:
    total = 0
    for turn in row.get("turns", []):
        try:
            total += int(turn.get("confirmation_count", 0) or 0)
        except (TypeError, ValueError):
            pass
    return total


def classify_flags(row: dict) -> list[str]:
    user = all_user_text(row)
    reply = all_reply_text(row)
    actions = all_actions(row)
    flags: list[str] = []
    if re.search(r"(未找到|没有找到|找不到).{0,24}(实例|主机|claude-write-test|内网ping勿删)", reply):
        flags.append("instance_not_found")
    if re.search(r"(知识库|资料|文档).{0,12}(未覆盖|没有|未找到|缺少)", reply):
        flags.append("kb_miss")
    if "5090" in user + reply and re.search(r"5090.{0,12}(尚未发布|未发布|不存在|不支持)", reply):
        flags.append("stale_5090")
    if re.search(r"(库存|有货|没卡|没有卡|无法创建|售罄)", user):
        if re.search(r"(Status=Normal|状态=Normal|正常在售|可售)", reply) and not re.search(r"(容量预检|可创建库存|未能确认|暂无可创建)", reply):
            flags.append("status_as_stock")
    if re.search(r"(磁盘|硬盘|存储|空间).{0,12}(价格|收费|计费|多少钱)", user) and re.search(r"(4090|GPU).{0,12}(价格|元|¥)", reply):
        flags.append("disk_price_answered_as_gpu")
    if re.search(r"(vasp|VASP|微调|RVC|训练)", user) and any(a in actions for a in ("CreateInstanceWorkflow", "CreateCompShareInstance")):
        flags.append("training_or_hpc_created")
    if "ComfyUI" in user and re.search(r"(打不开|连接|连不上|访问不了|报错)", user) and any(a in actions for a in ("CreateInstanceWorkflow", "CreateCompShareInstance")):
        flags.append("troubleshooting_created")
    if re.search(r"(轮次超限|处理轮次超限|round limit)", reply, flags=re.I):
        flags.append("round_limit")
    if confirmation_count(row) > 0:
        flags.append("confirmation")
    return flags


def likely_delta(old_status: str, flags: list[str]) -> str:
    serious = {f for f in flags if f != "confirmation"}
    if old_status in ("通过", "确认前链路通过") and serious:
        return "疑似退化"
    if old_status == "失败" and not serious:
        return "需人工确认是否改善"
    if old_status == "部分解决" and not serious:
        return "可能持平/改善"
    return "可能持平"


def preview(text: str, limit: int = 96) -> str:
    text = re.sub(r"\s+", " ", text).strip()
    if len(text) <= limit:
        return text
    return text[: limit - 1] + "…"


def render_report(baseline: dict[str, str], rows: list[dict]) -> str:
    rows_by_id = {str(r.get("case_id", "")): r for r in rows if r.get("case_id")}
    ids = sorted(set(baseline) | set(rows_by_id))
    flag_counts: Counter[str] = Counter()
    delta_counts: Counter[str] = Counter()
    status_counts: Counter[str] = Counter()
    lines: list[str] = []
    lines.append("# HTTP 回放差异候选报告")
    lines.append("")
    lines.append("该报告用于筛出需要人工复核的 case，不替代人工判分。")
    lines.append("")
    for case_id in ids:
        old_status = baseline.get(case_id, "未分档")
        status_counts[old_status] += 1
        row = rows_by_id.get(case_id)
        if row is None:
            delta = "当前缺失"
            flags = ["missing_current"]
            final = ""
            confirms = 0
        else:
            flags = classify_flags(row)
            delta = likely_delta(old_status, flags)
            final = str(row.get("final_reply", ""))
            confirms = confirmation_count(row)
        delta_counts[delta] += 1
        flag_counts.update(flags)
    lines.append("## 汇总")
    lines.append("")
    lines.append("| 项目 | 数量 |")
    lines.append("| --- | ---: |")
    lines.append(f"| baseline case | {len(ids)} |")
    lines.append(f"| current replay rows | {len(rows_by_id)} |")
    for status in STATUS_ORDER:
        lines.append(f"| baseline {status} | {status_counts[status]} |")
    for delta, count in delta_counts.most_common():
        lines.append(f"| {delta} | {count} |")
    lines.append("")
    lines.append("## 风险标记")
    lines.append("")
    lines.append("| 标记 | 数量 |")
    lines.append("| --- | ---: |")
    for flag, count in flag_counts.most_common():
        lines.append(f"| {flag} | {count} |")
    lines.append("")
    lines.append("## 逐案表")
    lines.append("")
    lines.append("| Case | 旧人工结论 | 当前信号 | 判断 | 确认数 | 当前回复预览 |")
    lines.append("| --- | --- | --- | --- | ---: | --- |")
    for case_id in ids:
        old_status = baseline.get(case_id, "未分档")
        row = rows_by_id.get(case_id)
        if row is None:
            flags = ["missing_current"]
            delta = "当前缺失"
            confirms = 0
            final = ""
        else:
            flags = classify_flags(row)
            delta = likely_delta(old_status, flags)
            confirms = confirmation_count(row)
            final = str(row.get("final_reply", ""))
        flag_text = ", ".join(flags) if flags else "-"
        lines.append(f"| {case_id} | {old_status} | {flag_text} | {delta} | {confirms} | {preview(final)} |")
    lines.append("")
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline-review", required=True, type=Path)
    parser.add_argument("--current-jsonl", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args()

    baseline = parse_baseline_statuses(args.baseline_review)
    rows = read_jsonl(args.current_jsonl)
    report = render_report(baseline, rows)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(report, encoding="utf-8")
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
