#!/usr/bin/env python3
"""A/B analysis for the knowledge_qa terminal-RAG -> agent-loop migration.

Compares condition A (COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP off = terminal RAG) vs
condition B (flag on = forced-SearchKnowledge agent loop) over the SAME probe
set. Both summaries come from eval/agentic_rag_probe.ps1.

Observable metrics (no LLM) per condition: intent mix, runtime-form mix,
SearchKnowledge-fired rate, retrieval-fired rate, refusal rate, expected-chunk
coverage, control-contamination. Faithfulness (LLM judge) scores each ACTUAL CLI
reply against the chunks that turn retrieved (runtime-faithful, not a re-generated
answer): fabrication = a specific tool/platform claim not supported by the shown
evidence. The corpus-gap probe is judged for honest-abstain (no fabricated
specifics), not grounding.

Judge = claude-opus-4-7 via ModelVerse (the evaluate_answers default). Reads the
judge key from RAG_EVAL_JUDGE_API_KEY, else MODELVERSE_API_KEY, else LLM_API_KEY.
No secret values are printed.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts" / "rag_w0"))
from model_smoke import DEFAULT_BASE_URL, ModelVerseClient  # noqa: E402

RAG_NO_EVIDENCE_REPLY = "当前知识库未覆盖该问题,我无法回答。"
JUDGE_MODEL = "claude-opus-4-7"


def read_jsonl_bom(path: Path) -> list[dict]:
    rows = []
    with open(path, encoding="utf-8-sig") as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def load_chunk_text(paths: list[Path]) -> dict[str, str]:
    out: dict[str, str] = {}
    for p in paths:
        if not p.exists():
            continue
        for row in read_jsonl_bom(p):
            cid = str(row.get("chunk_id") or "")
            if not cid:
                continue
            txt = row.get("content") or row.get("text") or ""
            title = row.get("title") or ""
            out[cid] = (title + "\n" + txt).strip()
    return out


def is_refusal(reply: str) -> bool:
    r = (reply or "").strip()
    return r == RAG_NO_EVIDENCE_REPLY or (len(r) <= 100 and ("知识库未覆盖" in r or "无法根据知识库回答" in r))


def judge_faithfulness(client: ModelVerseClient, question: str, reply: str, evidence: list[str], gap: bool) -> dict:
    ev = "\n\n".join(f"[证据{i+1}]\n{t[:1200]}" for i, t in enumerate(evidence)) or "(本轮未检索到证据)"
    task = (
        "你是严格的事实核查员。判断【回答】中的具体技术结论是否都能由【证据】支撑。\n"
        "fabrication=true 仅当：回答把某个证据里没有、且并非公认通用常识的『具体命令/参数/平台事实』当作权威结论给出。\n"
        "通用、公认的技术常识（如 tmux/nohup 的标准用法、PyTorch 标准 API）即使证据没逐字写出，也不算 fabrication。\n"
    )
    if gap:
        task += "本题知识库无专项覆盖：正确做法是诚实说明并给通用建议，不得编造平台专属步骤。若回答编造了具体平台专属步骤则 fabrication=true。\n"
    prompt = (
        f"{task}\n【问题】\n{question}\n\n【证据】\n{ev}\n\n【回答】\n{reply[:4000]}\n\n"
        '只输出 JSON：{"fabrication": true/false, "grounded": true/false, "reason": "一句话"}'
    )
    for attempt in range(3):
        try:
            raw = client.chat(model=JUDGE_MODEL, messages=[{"role": "user", "content": prompt}], max_tokens=400, json_mode=True)
            s = raw.find("{"); e = raw.rfind("}")
            return json.loads(raw[s : e + 1])
        except Exception as ex:  # noqa: BLE001
            if attempt == 2:
                return {"fabrication": None, "grounded": None, "reason": f"judge_error: {ex}"}
            time.sleep(2.0)
    return {"fabrication": None, "grounded": None, "reason": "unreachable"}


def analyze(summary_rows: list[dict], probes: dict[str, dict], chunks: dict[str, str], client, judge: bool, label: str) -> dict:
    n = len(summary_rows)
    intents: dict[str, int] = {}
    forms: dict[str, int] = {}
    sk_fired = retr_fired = refusals = cov_hits = cov_total = contam = 0
    fab = grounded = judged = 0
    rows_out = []
    for row in summary_rows:
        pid = row.get("probe_id")
        probe = probes.get(pid, {})
        kind = probe.get("kind", "")
        intent = row.get("intent") or "?"
        form = row.get("actual_runtime_form") or "?"
        intents[intent] = intents.get(intent, 0) + 1
        forms[form] = forms.get(form, 0) + 1
        if row.get("search_knowledge_fired"):
            sk_fired += 1
        if row.get("retrieval_fired"):
            retr_fired += 1
        reply = row.get("reply_full") or ""
        ref = is_refusal(reply)
        if ref:
            refusals += 1
        exp = probe.get("expect_chunk") or ""
        retrieved = row.get("retrieved_chunk_ids") or []
        if exp:
            cov_total += 1
            if exp in retrieved:
                cov_hits += 1
        if kind == "control-anticontam":
            for bad in probe.get("must_not_contain", []):
                if bad.lower() in reply.lower():
                    contam += 1
                    break
        verdict = {}
        if judge and not ref:
            ev = [chunks[c] for c in retrieved if c in chunks]
            verdict = judge_faithfulness(client, probe.get("question", ""), reply, ev, gap=(kind == "corpus-gap-abstain"))
            if verdict.get("fabrication") is not None:
                judged += 1
                if verdict.get("fabrication"):
                    fab += 1
                if verdict.get("grounded"):
                    grounded += 1
            time.sleep(1.2)
        rows_out.append({"probe_id": pid, "kind": kind, "intent": intent, "form": form,
                         "sk": bool(row.get("search_knowledge_fired")), "retr": bool(row.get("retrieval_fired")),
                         "refusal": ref, "exp_chunk": exp, "exp_in_retrieved": (exp in retrieved) if exp else None,
                         "verdict": verdict})
    return {"label": label, "n": n, "intents": intents, "forms": forms,
            "sk_fired_rate": sk_fired / n if n else 0, "retr_fired_rate": retr_fired / n if n else 0,
            "refusal_rate": refusals / n if n else 0, "expected_chunk_coverage": f"{cov_hits}/{cov_total}",
            "control_contaminated": contam, "judged": judged, "fabrications": fab, "grounded": grounded,
            "rows": rows_out}


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--a-summary", required=True)
    ap.add_argument("--b-summary", required=True)
    ap.add_argument("--probes", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--no-judge", action="store_true")
    args = ap.parse_args()

    root = Path(__file__).resolve().parents[1]
    probes = {p["id"]: p for p in json.loads(Path(args.probes).read_text(encoding="utf-8"))}
    chunks = load_chunk_text([root / "deploy/kb/external_w0.jsonl", root / "deploy/kb/stage2b_w0.jsonl"])

    client = None
    if not args.no_judge:
        key = os.environ.get("RAG_EVAL_JUDGE_API_KEY") or os.environ.get("MODELVERSE_API_KEY") or os.environ.get("LLM_API_KEY")
        if not key:
            print("no judge key in env; rerun with --no-judge or set RAG_EVAL_JUDGE_API_KEY", file=sys.stderr)
            sys.exit(2)
        client = ModelVerseClient(base_url=os.environ.get("MODELVERSE_BASE_URL", DEFAULT_BASE_URL), api_key=key)

    a = analyze(read_jsonl_bom(Path(args.a_summary)), probes, chunks, client, not args.no_judge, "A_terminal")
    b = analyze(read_jsonl_bom(Path(args.b_summary)), probes, chunks, client, not args.no_judge, "B_agent_loop")
    report = {"A": a, "B": b}
    Path(args.out).write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")

    for c in (a, b):
        print(f"\n=== {c['label']} (n={c['n']}) ===")
        print(f"  intents: {c['intents']}")
        print(f"  forms:   {c['forms']}")
        print(f"  sk_fired_rate={c['sk_fired_rate']:.2f} retr_fired_rate={c['retr_fired_rate']:.2f} refusal_rate={c['refusal_rate']:.2f}")
        print(f"  expected_chunk_coverage={c['expected_chunk_coverage']} control_contaminated={c['control_contaminated']}")
        print(f"  judged={c['judged']} fabrications={c['fabrications']} grounded={c['grounded']}")
    print(f"\nreport: {args.out}")


if __name__ == "__main__":
    main()
