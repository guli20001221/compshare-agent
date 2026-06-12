#!/usr/bin/env python3
"""Judge the ComfyUI external-corpus CLI smoke (companion to
rag_ext_comfyui_smoke.ps1). For each question reads the captured reply +
per-question JSONL trace and asserts, end-to-end on the merged runtime index:

  - comfyui questions: the expected ext-comfyui-* chunk was retrieved (its
    chunk_id appears in the turn's retrieval trace) AND the reply carries a
    grounded anchor token from that chunk (a real flag/path, not a paraphrase
    that could be free-LLM hallucination).
  - control (anti-contamination): a "call the PLATFORM model via OpenAI API"
    question must NOT drag ComfyUI into the answer (external image-gen chunks
    must not contaminate a platform-API answer).

Trace JSONL is read with utf-8-sig (PowerShell Add-Content / no-BOM writes vary;
utf-8-sig tolerates a BOM). Reports PASS/FAIL and writes a markdown summary.
"""
from __future__ import annotations

import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent
QPATH = ROOT / "rag_ext_comfyui_smoke_questions.json"
TRACE_BASE = ROOT / "traces_comfyui_smoke"
REPORT = ROOT / "rag_ext_comfyui_smoke_report.md"

ANSI = re.compile(r"\x1b\[[0-9;]*m")


def read_text(p: Path) -> str:
    try:
        return p.read_text(encoding="utf-8-sig")
    except FileNotFoundError:
        return ""


def assistant_reply(raw: str) -> str:
    """Isolate the agent's answer from captured CLI chrome. The PowerShell
    pipeline merges native stderr (2>&1), which PS 5.1 wraps as an ErrorRecord
    whose text includes the *script path* (…rag_ext_comfyui_smoke.ps1…). Matching
    must_not_contain against that path would false-flag 'comfyui'. The real answer
    is the text after the last 'Assistant>' prompt, up to the next CLI prompt."""
    txt = ANSI.sub("", raw)
    parts = re.split(r"Assistant>", txt)
    ans = parts[-1] if len(parts) > 1 else txt
    ans = re.split(r"\n\s*(You>|>\s)", ans)[0]
    return ans.strip()


def load_trace(qdir: Path):
    """Return (planner_intent, retrieved_ids, cited_ids, raw_text)."""
    raw = ""
    intent = ""
    retrieved: set[str] = set()
    cited: list[str] = []
    for f in qdir.glob("agent-trace-*.jsonl"):
        for line in read_text(f).splitlines():
            line = line.strip()
            if not line:
                continue
            raw += line + "\n"
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            pl = rec.get("intent_router") or {}
            if isinstance(pl, dict) and pl.get("intent"):
                intent = str(pl.get("intent"))
            rt = rec.get("retrieval") or {}
            if isinstance(rt, dict):
                for cid in rt.get("chunk_ids") or []:
                    retrieved.add(str(cid))
                for it in rt.get("hit_items") or []:
                    if isinstance(it, dict) and it.get("chunk_id"):
                        retrieved.add(str(it["chunk_id"]))
                for cid in rt.get("cited_chunk_ids") or []:
                    cited.append(str(cid))
    return intent, retrieved, cited, raw


def main() -> int:
    questions = json.loads(QPATH.read_text(encoding="utf-8"))
    rows = []
    for q in questions:
        qid = q["qid"]
        qdir = TRACE_BASE / qid
        reply = assistant_reply(read_text(qdir / "reply.txt"))
        intent, retrieved, cited, raw = load_trace(qdir)
        expect = q.get("expect_chunk") or ""
        anchors = q.get("anchors_any") or []
        kind = q.get("kind")

        # retrieved: chunk_id in structured retrieval set OR present anywhere in
        # the raw trace (covers trace-shape variations across lanes).
        got_chunk = bool(expect) and (expect in retrieved or expect in raw)
        got_cited = bool(expect) and expect in cited
        anchor_hit = [a for a in anchors if a in reply]
        anchor_ok = (not anchors) or bool(anchor_hit)

        if kind == "control-anticontam":
            bad = [m for m in (q.get("must_not_contain") or []) if m in reply]
            comfy_in_trace = "ext-comfyui" in raw
            ok = (not bad) and (not comfy_in_trace)
            detail = f"intent={intent} must_not_contain_hits={bad} comfyui_in_trace={comfy_in_trace}"
        else:
            ok = got_chunk and anchor_ok
            detail = (f"intent={intent} retrieved={got_chunk} cited={got_cited} "
                      f"anchors_hit={anchor_hit} reply_chars={len(reply)}")

        rows.append({"qid": qid, "kind": kind, "ok": ok, "detail": detail,
                     "expect": expect, "intent": intent,
                     "retrieved": got_chunk, "cited": got_cited,
                     "anchor_hit": anchor_hit})
        flag = "PASS" if ok else "FAIL"
        print(f"[{flag}] {qid:22s} {detail}")

    n = len(rows)
    npass = sum(1 for r in rows if r["ok"])
    print(f"\n=== ComfyUI smoke: {npass}/{n} PASS ===")

    lines = ["# ComfyUI external-corpus CLI smoke report", "",
             f"Merged runtime index (platform 687 + external 36, default-on), "
             f"deepseek-v4-flash, read-only. {npass}/{n} PASS.", "",
             "One run; deepseek-v4-flash is non-deterministic, so per-row `intent`",
             "(knowledge_qa vs diagnosis lane — both ground on the same evidence) and",
             "the exact `anchors hit` vary between runs. The PASS criterion is robust to",
             "that: a ComfyUI row passes on `retrieved`(expected chunk in the turn's",
             "retrieval trace) AND `anchors_any` (>=1 grounded token in the reply); the",
             "control passes on no-ComfyUI-contamination. Regenerate via",
             "`pwsh -File eval/rag_ext_comfyui_smoke.ps1 && python eval/rag_ext_comfyui_smoke_judge.py`.",
             "",
             "| qid | kind | verdict | intent | retrieved | cited | anchors hit |",
             "|---|---|---|---|---|---|---|"]
    for r in rows:
        lines.append(f"| {r['qid']} | {r['kind']} | "
                     f"{'PASS' if r['ok'] else 'FAIL'} | {r['intent']} | "
                     f"{r['retrieved']} | {r['cited']} | {', '.join(r['anchor_hit'])} |")
    REPORT.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"report -> {REPORT}")
    return 0 if npass == n else 1


if __name__ == "__main__":
    raise SystemExit(main())
