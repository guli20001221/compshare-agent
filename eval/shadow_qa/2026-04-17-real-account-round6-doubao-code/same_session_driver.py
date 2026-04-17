from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any


BOOT_MARKER = b"You> "
CONFIRM_MARKER = "确认执行？(y/N)".encode("utf-8")

ASSERTION_KEYS = [
    "expect_tool_calls", "expect_first_tool", "reject_tool_calls",
    "expect_no_tool_call", "reply_contains", "reply_contains_any", "reply_not_contains",
]


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


def parse_session_output(text: str) -> dict[str, Any]:
    tool_calls: list[str] = []
    errors: list[str] = []
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if "🔧 调用 " in line:
            fragment = line.split("🔧 调用 ", 1)[1]
            action = fragment.split(" ...", 1)[0].strip()
            if action:
                tool_calls.append(action)
        if line.startswith("错误:"):
            errors.append(line)

    assistant_reply = ""
    if "Assistant>" in text:
        tail = text.split("Assistant>", 1)[1]
        assistant_reply = tail.split("\nYou>", 1)[0].strip()

    return {
        "tool_calls": tool_calls,
        "assistant_reply": assistant_reply,
        "errors": errors,
        "raw_output": text,
    }


def evaluate_step(step: dict[str, Any], parsed: dict[str, Any]) -> list[str]:
    failures: list[str] = []
    tool_calls = parsed["tool_calls"]
    reply = parsed["assistant_reply"]

    for err in parsed["errors"]:
        failures.append(err)

    expected_tools = step.get("expect_tool_calls", [])
    if expected_tools and not any(tool in tool_calls for tool in expected_tools):
        failures.append(f"expected one of {expected_tools}, got {tool_calls}")

    if step.get("expect_first_tool"):
        if not tool_calls:
            failures.append(f"first tool = (none), want {step['expect_first_tool']}")
        elif tool_calls[0] != step["expect_first_tool"]:
            failures.append(f"first tool = {tool_calls[0]}, want {step['expect_first_tool']}")

    for tool in step.get("reject_tool_calls", []):
        if tool in tool_calls:
            failures.append(f"rejected tool appeared: {tool}")

    if step.get("expect_no_tool_call") and tool_calls:
        failures.append(f"expected no tool call, got {tool_calls}")

    for needle in step.get("reply_contains", []):
        if needle not in reply:
            failures.append(f"reply missing {needle!r}")

    any_needles = step.get("reply_contains_any", [])
    if any_needles and not any(needle in reply for needle in any_needles):
        failures.append(f"reply missing any of {any_needles!r}")

    for needle in step.get("reply_not_contains", []):
        if needle in reply:
            failures.append(f"reply should not contain {needle!r}")

    return failures


def run_hook(cmd: list[str], cwd: str, timeout: float) -> str:
    proc = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout, encoding="utf-8", errors="replace")
    if proc.returncode != 0:
        raise RuntimeError(f"hook failed: {' '.join(cmd)}\nSTDOUT:\n{proc.stdout}\nSTDERR:\n{proc.stderr}")
    return (proc.stdout + proc.stderr).strip()


def load_cases(path: str | Path) -> list[dict[str, Any]]:
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def run_case(binary: str, config: str, case: dict[str, Any], timeout: float, cwd: str) -> dict[str, Any]:
    env = os.environ.copy()
    env.setdefault("PYTHONUTF8", "1")
    env.setdefault("PYTHONIOENCODING", "utf-8")

    proc = subprocess.Popen(
        [binary, "cli", "-c", config],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=env,
        cwd=cwd,
    )

    results: list[dict[str, Any]] = []
    all_failures: list[str] = []

    try:
        boot = read_until(proc, [BOOT_MARKER], timeout)
        if BOOT_MARKER not in boot:
            raise RuntimeError("failed to reach CLI prompt during boot")

        for idx, step in enumerate(case["steps"], start=1):
            hook_before_output = ""
            hook_after_output = ""
            if step.get("hook_before"):
                hook_before_output = run_hook(step["hook_before"], cwd, timeout)

            proc.stdin.write((step["input"] + "\n").encode("utf-8"))
            proc.stdin.flush()
            delta = read_until(proc, [BOOT_MARKER, CONFIRM_MARKER], timeout)
            if CONFIRM_MARKER in delta:
                answer = step.get("confirm", "n")
                proc.stdin.write((answer + "\n").encode("utf-8"))
                proc.stdin.flush()
                delta += read_until(proc, [BOOT_MARKER], timeout)

            if step.get("hook_after"):
                hook_after_output = run_hook(step["hook_after"], cwd, timeout)

            text = delta.decode("utf-8", errors="replace")
            parsed = parse_session_output(text)
            failures = evaluate_step(step, parsed)
            for failure in failures:
                all_failures.append(f"[step {idx}] {failure}")
            results.append({
                "step": idx,
                "input": step["input"],
                "hook_before_output": hook_before_output,
                "hook_after_output": hook_after_output,
                "parsed": parsed,
                "failures": failures,
                "passed": not failures,
            })

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
        "passed": not all_failures,
        "failures": all_failures,
        "steps": results,
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
        lines.append(f"**Result**: {'PASS' if item['passed'] else 'FAIL'}")
        if item["failures"]:
            lines.append("")
            for failure in item["failures"]:
                lines.append(f"- {failure}")
        lines.append("")
        for step in item["steps"]:
            lines.append(f"### Step {step['step']}: `{step['input']}`")
            lines.append("")
            lines.append(f"**Step result**: {'PASS' if step['passed'] else 'FAIL'}")
            if step["failures"]:
                for failure in step["failures"]:
                    lines.append(f"- {failure}")
            if step["hook_before_output"]:
                lines.append("")
                lines.append("**Hook before output**")
                lines.append("")
                lines.append("```text")
                lines.append(step["hook_before_output"])
                lines.append("```")
            if step["hook_after_output"]:
                lines.append("")
                lines.append("**Hook after output**")
                lines.append("")
                lines.append("```text")
                lines.append(step["hook_after_output"])
                lines.append("```")
            lines.append("")
            lines.append("```text")
            lines.append(step["parsed"]["raw_output"].rstrip())
            lines.append("```")
            lines.append("")
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--config", required=True)
    parser.add_argument("--cases", required=True)
    parser.add_argument("--out-md", default="")
    parser.add_argument("--out-json", default="")
    parser.add_argument("--title", default="Same Session Shadow QA")
    parser.add_argument("--timeout", type=float, default=240.0)
    args = parser.parse_args()

    cases = load_cases(args.cases)
    cwd = str(Path(args.binary).resolve().parent.parent.parent.parent)  # back to repo root
    results = [run_case(args.binary, args.config, case, args.timeout, cwd) for case in cases]

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
