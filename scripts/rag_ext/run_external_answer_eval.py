#!/usr/bin/env python3
"""Offline faithfulness/grounding first-pass for the external corpus.

Calls evaluate_answers directly (no platform-only guards). Defaults to a
deepseek-v4-flash answerer (runtime parity) + deepseek-v4-pro judge. NOTE: per
project memory (feedback-cli-eval-flash-only / -offline-eval-not-equal-cli-smoke)
this offline path bypasses the engine cited-guard/retry/timeout and v4-pro judges
tend to OVER-report fabrication — treat any fab>0 as a flag to inspect, not a
verdict. The authoritative faithfulness check is a CLI smoke with v4-flash after
the corpus is wired into the runtime (Phase 2).

Key handling: pass MODELVERSE_API_KEY via the shell env and point --env at a
nonexistent path so _load_env falls back to os.environ (avoids writing the key
to disk and avoids the dev .env.local MODELVERSE_DS_V4_PRO_MODEL override).
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from rag_w0.evaluate_answers import evaluate_answers  # noqa: E402


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--chunks", required=True)
    p.add_argument("--questions", required=True)
    p.add_argument("--retrieval-eval", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--answer-model", default="deepseek-v4-flash")
    p.add_argument("--judge-model", default="deepseek-v4-pro")
    p.add_argument("--env", default="__nonexistent__.env.local")
    a = p.parse_args()
    summary = evaluate_answers(
        a.chunks,
        a.questions,
        a.retrieval_eval,
        a.out,
        answer_model=a.answer_model,
        judge_model=a.judge_model,
        env_path=Path(a.env),
        progress=True,
    )
    ev = int(summary.get("evaluated") or 0)
    out = {k: summary.get(k) for k in ("evaluated", "grounded", "cited", "fabricated", "safety_failures", "internal_leakage")}
    print(json.dumps(out, ensure_ascii=False, indent=2))
    if ev:
        print(f"grounded_rate={summary.get('grounded',0)/ev:.3f} "
              f"cited_rate={summary.get('cited',0)/ev:.3f} "
              f"fab_rate={summary.get('fabricated',0)/ev:.3f}")
    for f in summary.get("failed_answers") or []:
        print("FAILED:", f.get("question_id"), f.get("reason"))
    # Report fabricated cases for inspection (do not auto-gate — see header).
    for r in summary.get("answers") or []:
        if r.get("fabricated"):
            print("FAB-FLAG:", r.get("question_id"), "-", str(r.get("reasoning") or "")[:200])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
