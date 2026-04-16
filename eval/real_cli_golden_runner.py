from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any


THIS_DIR = Path(__file__).resolve().parent
DEFAULT_CASES = THIS_DIR / "real_cli_golden_cases.json"
BOOT_MARKER = b"You> "
CONFIRM_MARKER = "确认执行？(y/N)".encode("utf-8")

ASSERTION_KEYS = [
    "expect_tool_calls", "expect_first_tool", "reject_tool_calls",
    "expect_no_tool_call", "expect_display", "reply_contains",
    "reply_contains_any", "reply_not_contains",
]


def validate_case_schema(case: dict[str, Any]) -> None:
    """Validate that case has input XOR steps."""
    has_input = "input" in case
    has_steps = "steps" in case
    if has_input == has_steps:  # both or neither
        raise ValueError(
            f"Case {case.get('id', '?')}: 'input' and 'steps' are mutually exclusive; "
            f"provide exactly one."
        )


def normalize_to_steps(case: dict[str, Any]) -> list[dict[str, Any]]:
    """Convert single-turn case to steps format, or return steps directly."""
    validate_case_schema(case)
    if "steps" in case:
        return case["steps"]
    # Single-turn -> wrap in one step
    step: dict[str, Any] = {"input": case["input"]}
    if "confirm" in case:
        step["confirm"] = case["confirm"]
    for key in ASSERTION_KEYS:
        if key in case:
            step[key] = case[key]
    return [step]


def load_cases(path: str | Path) -> list[dict[str, Any]]:
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def parse_session_output(text: str) -> dict[str, Any]:
    tool_calls: list[str] = []
    display_lines: list[str] = []
    errors: list[str] = []

    for raw_line in text.splitlines():
        line = raw_line.strip()
        if "🔧 调用 " in line:
            fragment = line.split("🔧 调用 ", 1)[1]
            action = fragment.split(" ...", 1)[0].strip()
            if action:
                tool_calls.append(action)
        if line.startswith("🔑 "):
            display_lines.append(line[2:].strip())
        if line.startswith("错误:"):
            errors.append(line)

    assistant_reply = ""
    if "Assistant>" in text:
        tail = text.split("Assistant>", 1)[1]
        assistant_reply = tail.split("\nYou>", 1)[0].strip()

    return {
        "tool_calls": tool_calls,
        "display_lines": display_lines,
        "assistant_reply": assistant_reply,
        "errors": errors,
        "has_confirm": "确认执行？(y/N)" in text,
        "raw_output": text,
    }


def evaluate_case(case: dict[str, Any], parsed: dict[str, Any]) -> dict[str, Any]:
    failures: list[str] = []
    tool_calls = parsed["tool_calls"]
    reply = parsed["assistant_reply"]

    for err in parsed["errors"]:
        failures.append(err)

    expected_tools = case.get("expect_tool_calls", [])
    if expected_tools:
        if not any(tool in tool_calls for tool in expected_tools):
            failures.append(f"expected one of {expected_tools}, got {tool_calls}")

    if case.get("expect_first_tool"):
        if not tool_calls:
            failures.append(f"first tool = (none), want {case['expect_first_tool']}")
        elif tool_calls[0] != case["expect_first_tool"]:
            failures.append(f"first tool = {tool_calls[0]}, want {case['expect_first_tool']}")

    for tool in case.get("reject_tool_calls", []):
        if tool in tool_calls:
            failures.append(f"rejected tool appeared: {tool}")

    if case.get("expect_no_tool_call") and tool_calls:
        failures.append(f"expected no tool call, got {tool_calls}")

    if case.get("expect_display") and not parsed["display_lines"]:
        failures.append("expected display line, none found")

    for needle in case.get("reply_contains", []):
        if needle not in reply:
            failures.append(f"reply missing {needle!r}")

    any_needles = case.get("reply_contains_any", [])
    if any_needles and not any(needle in reply for needle in any_needles):
        failures.append(f"reply missing any of {any_needles!r}")

    for needle in case.get("reply_not_contains", []):
        if needle in reply:
            failures.append(f"reply should not contain {needle!r}")

    return {
        "passed": len(failures) == 0,
        "failures": failures,
    }


def read_until(proc: subprocess.Popen[bytes], markers: list[bytes], timeout: float) -> bytes:
    end = time.time() + timeout
    buf = bytearray()
    while time.time() < end:
        chunk = proc.stdout.read(1)
        if not chunk:
            break
        buf.extend(chunk)
        if any(marker in buf for marker in markers):
            break
    return bytes(buf)


def run_case(binary: str, config: str, case: dict[str, Any], timeout: float) -> dict[str, Any]:
    steps = normalize_to_steps(case)
    env = os.environ.copy()
    env.setdefault("PYTHONUTF8", "1")
    env.setdefault("PYTHONIOENCODING", "utf-8")

    proc = subprocess.Popen(
        [binary, "cli", "-c", config],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=env,
    )

    step_results: list[dict[str, Any]] = []
    all_failures: list[str] = []

    try:
        boot = read_until(proc, [BOOT_MARKER], timeout)
        if BOOT_MARKER not in boot:
            raise RuntimeError("failed to reach CLI prompt during boot")

        for i, step in enumerate(steps):
            # Send this step's input
            payload = (step["input"] + "\n").encode("utf-8")
            proc.stdin.write(payload)
            proc.stdin.flush()

            # Read this turn's delta output
            delta = read_until(proc, [BOOT_MARKER, CONFIRM_MARKER], timeout)
            if CONFIRM_MARKER in delta:
                answer = step.get("confirm", "n")
                proc.stdin.write((answer + "\n").encode("utf-8"))
                proc.stdin.flush()
                delta += read_until(proc, [BOOT_MARKER], timeout)

            # Parse and evaluate ONLY this turn's delta
            delta_text = delta.decode("utf-8", errors="replace")
            parsed = parse_session_output(delta_text)
            result = evaluate_case(step, parsed)

            step_result = {
                "step": i + 1,
                "input": step["input"],
                "parsed": parsed,
                "passed": result["passed"],
                "failures": result["failures"],
            }
            step_results.append(step_result)

            # Prefix step failures with step number
            for failure in result["failures"]:
                all_failures.append(f"[step {i + 1}] {failure}")

        proc.stdin.write(b"quit\n")
        proc.stdin.flush()
        try:
            proc.wait(timeout=20)
        except subprocess.TimeoutExpired:
            proc.kill()
    finally:
        if proc.poll() is None:
            proc.kill()

    return {
        "case_id": case["id"],
        "input": case.get("input", f"{len(steps)}-step conversation"),
        "steps": step_results,
        "parsed": step_results[-1]["parsed"] if step_results else {},
        "passed": len(all_failures) == 0,
        "failures": all_failures,
    }


def format_markdown(results: list[dict[str, Any]], title: str) -> str:
    lines = [f"# {title}", ""]
    passed = sum(1 for item in results if item["passed"])
    lines.append(f"**Summary**: {passed}/{len(results)} PASS")
    lines.append("")
    lines.append("| Case | Result | Notes |")
    lines.append("|------|:------:|-------|")
    for item in results:
        notes = "; ".join(item["failures"]) if item["failures"] else "PASS"
        lines.append(f"| {item['case_id']} | {'PASS' if item['passed'] else 'FAIL'} | {notes} |")
    lines.append("")

    for item in results:
        lines.append(f"## {item['case_id']}")
        lines.append("")
        lines.append(f"**Input**: `{item['input']}`")
        lines.append("")
        lines.append(f"**Result**: {'PASS' if item['passed'] else 'FAIL'}")
        if item["failures"]:
            lines.append("")
            for failure in item["failures"]:
                lines.append(f"- {failure}")
        lines.append("")

        step_results = item.get("steps", [])
        if len(step_results) > 1:
            for sr in step_results:
                lines.append(f"### Step {sr['step']}: `{sr['input']}`")
                lines.append("")
                lines.append(f"**Step result**: {'PASS' if sr['passed'] else 'FAIL'}")
                if sr["failures"]:
                    for sf in sr["failures"]:
                        lines.append(f"- {sf}")
                lines.append("")
                lines.append("```text")
                lines.append(sr["parsed"]["raw_output"].rstrip())
                lines.append("```")
                lines.append("")
        else:
            lines.append("```text")
            lines.append(item["parsed"]["raw_output"].rstrip())
            lines.append("```")
            lines.append("")
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description="Run real CLI golden cases with stable UTF-8 pipes.")
    parser.add_argument("--binary", required=True, help="Path to compshare-agent executable")
    parser.add_argument("--config", required=True, help="Path to agent YAML config")
    parser.add_argument("--cases", default=str(DEFAULT_CASES), help="Path to real CLI golden cases JSON")
    parser.add_argument("--case", dest="case_filters", action="append", default=[], help="Run only cases whose id contains this substring")
    parser.add_argument("--repeat", type=int, default=1, help="Repeat each selected case N times")
    parser.add_argument("--out-md", default="", help="Optional markdown report output path")
    parser.add_argument("--out-json", default="", help="Optional JSON report output path")
    parser.add_argument("--title", default="Real CLI Golden Report", help="Report title")
    parser.add_argument("--timeout", type=float, default=180.0, help="Per-phase timeout in seconds")
    args = parser.parse_args()

    sys.stdout.reconfigure(encoding="utf-8")

    cases = load_cases(args.cases)
    for case in cases:
        validate_case_schema(case)
    if args.case_filters:
        cases = [c for c in cases if any(flt in c["id"] for flt in args.case_filters)]
    if not cases:
        raise SystemExit("no cases selected")

    results: list[dict[str, Any]] = []
    for case in cases:
        for run_idx in range(args.repeat):
            label = case["id"] if args.repeat == 1 else f"{case['id']}#{run_idx + 1}"
            print(f"=== {label} ===")
            result = run_case(args.binary, args.config, case, args.timeout)
            result["run_index"] = run_idx + 1
            results.append(result)
            print("PASS" if result["passed"] else "FAIL")
            if result["failures"]:
                for failure in result["failures"]:
                    print(" -", failure)
            print()

    if args.out_json:
        with open(args.out_json, "w", encoding="utf-8") as f:
            json.dump(results, f, ensure_ascii=False, indent=2)
    if args.out_md:
        with open(args.out_md, "w", encoding="utf-8") as f:
            f.write(format_markdown(results, args.title))

    failed = [r for r in results if not r["passed"]]
    print(f"Summary: {len(results) - len(failed)}/{len(results)} PASS")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
