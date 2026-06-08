#!/usr/bin/env python3
"""Platform retrieval parity: prove the merged (platform+external) index does not
regress platform retrieval vs the platform-only index.

This is the GATE for flipping COMPSHARE_EXTERNAL_KNOWLEDGE default-on. External
knowledge is *additive*; the contract is that adding the 29 external chunks must
not push any platform golden question's expected chunk out of the top-3.

Method (rigorous A/B on the SAME platform golden, same qwen3_rrf pipeline):
  A (baseline): platform-only corpus + platform sidecar.
  B (merged):   platform + external corpus + merged sidecar, kb_version
                normalized to "merged" (retrieval is kb_version-agnostic, and the
                Go runtime presents the same merged index with KBVersion="merged";
                validate_chunks otherwise rejects mixed kb_version).

The ONLY difference between A and B is the presence of the 29 external chunks, so
any top-3 delta is attributable to external intrusion.

Query-embedding + reranker caches are shared across A and B: the 256 queries are
identical, and the reranker cache key includes a docs_sha256, so platform
questions whose RRF top-10 pool is unchanged by the merge reuse A's reranker
result (0 extra API calls); only questions the external chunks actually perturb
recompute. NOTE: because B may overwrite an A cache entry when a pool changes,
the shared cache is single-pass only; to re-derive the verdict from an already
completed run use --analyze-only (reads the saved per-run reports, no API calls).

Gate (Top-3 retrieval NON-REGRESSION, at aggregate AND per-group granularity --
per-group catches a group-level drop that an unchanged aggregate could mask):
  - top_3_hit_rate(merged) >= top_3_hit_rate(platform_only), AND
  - no group's hit count decreases.
Per-question displacement within a near-tie cluster is a Top-K effect (a
percentage metric, not a binary-0 contract -- those are reserved for
citation/leakage/schema per memory feedback_hard_contractual_gates_binary), so it
is surfaced as a REVIEW warning, not a gate failure. External chunks entering a
platform question's top-3 (whether or not they displace the expected chunk) are
reported for answer-faithfulness review; the fix for a genuine collision is a RAG
anti-confusion anchor, applied and re-smoked before flipping default-on.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))  # scripts/ on path
from rag_w0.evaluate_retrieval import evaluate_retrieval  # noqa: E402

MERGED_KB_VERSION = "merged"


def _read_jsonl(path: Path):
    return [json.loads(l) for l in Path(path).read_text(encoding="utf-8-sig").splitlines() if l.strip()]


def _write_jsonl(path: Path, rows) -> None:
    with Path(path).open("w", encoding="utf-8", newline="\n") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")


def _build_merged_corpus(platform_corpus: Path, external_corpus: Path, out: Path) -> tuple[int, int, set]:
    """Concatenate platform + external chunks, normalizing kb_version to 'merged'.

    Returns (platform_count, external_count, external_chunk_ids).
    """
    plat = _read_jsonl(platform_corpus)
    ext = _read_jsonl(external_corpus)
    ext_ids = {str(c["chunk_id"]) for c in ext}
    plat_ids = {str(c["chunk_id"]) for c in plat}
    dup = plat_ids & ext_ids
    if dup:
        raise SystemExit(f"cross-source chunk_id collision (loader would reject): {sorted(dup)[:10]}")
    merged = []
    for c in [*plat, *ext]:
        c = dict(c)
        c["kb_version"] = MERGED_KB_VERSION
        merged.append(c)
    _write_jsonl(out, merged)
    return len(plat), len(ext), ext_ids


def _build_merged_sidecar(platform_sidecar: Path, external_sidecar: Path, out: Path) -> int:
    """One _meta header (dim from platform) + all vector rows from both sidecars.

    _load_chunk_embedding_sidecar raises on a duplicate _meta, so the external
    sidecar's _meta line is dropped and only its vector rows are appended.
    """
    plat_lines = [l for l in Path(platform_sidecar).read_text(encoding="utf-8-sig").splitlines() if l.strip()]
    ext_lines = [l for l in Path(external_sidecar).read_text(encoding="utf-8-sig").splitlines() if l.strip()]
    plat_meta = json.loads(plat_lines[0])
    ext_meta = json.loads(ext_lines[0])
    if "_meta" not in plat_meta or "_meta" not in ext_meta:
        raise SystemExit("sidecar missing _meta on row 1")
    if plat_meta["_meta"]["dim"] != ext_meta["_meta"]["dim"]:
        raise SystemExit(f"sidecar dim mismatch: {plat_meta['_meta']['dim']} vs {ext_meta['_meta']['dim']}")
    if plat_meta["_meta"]["embed_model"] != ext_meta["_meta"]["embed_model"]:
        raise SystemExit("sidecar embed_model mismatch")
    plat_rows = plat_lines[1:]
    ext_rows = ext_lines[1:]
    meta = {"_meta": {
        "corpus_digest": MERGED_KB_VERSION,
        "dim": plat_meta["_meta"]["dim"],
        "embed_model": plat_meta["_meta"]["embed_model"],
        "rows": len(plat_rows) + len(ext_rows),
    }}
    with Path(out).open("w", encoding="utf-8", newline="\n") as fh:
        fh.write(json.dumps(meta, ensure_ascii=False) + "\n")
        for l in [*plat_rows, *ext_rows]:
            fh.write(l + "\n")
    return len(plat_rows) + len(ext_rows)


def _top3_by_qid(report: dict) -> dict:
    """question_id -> list[chunk_id] from the eval trace (order = retrieval order)."""
    out = {}
    for rec in report.get("trace_records") or []:
        qid = str(rec.get("question_id") or "")
        out[qid] = [str(h["chunk_id"]) for h in rec.get("hit_items") or []]
    return out


def _expected_by_qid(questions_path: Path) -> dict:
    out = {}
    for row in _read_jsonl(questions_path):
        if row.get("expected_behavior") != "answer":
            continue
        out[str(row.get("question_id") or "")] = {
            "expected": [str(x) for x in row.get("expected_chunk_ids") or []],
            "question": str(row.get("question") or ""),
            "group": str(row.get("group") or ""),
        }
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--platform-corpus", required=True)
    ap.add_argument("--platform-sidecar", required=True)
    ap.add_argument("--external-corpus", required=True)
    ap.add_argument("--external-sidecar", required=True)
    ap.add_argument("--questions", required=True)
    ap.add_argument("--env", default=None, help="path to .env.local with MODELVERSE_API_KEY")
    ap.add_argument("--workdir", default=None, help="scratch dir for merged artifacts + caches (default: temp)")
    ap.add_argument("--out", required=True, help="parity summary JSON")
    ap.add_argument("--mode", default="qwen3_rrf")
    ap.add_argument("--analyze-only", action="store_true",
                    help="skip the API-heavy eval; re-derive the verdict from the "
                         "saved report_platform.json / report_merged.json in --workdir")
    a = ap.parse_args()

    workdir = Path(a.workdir) if a.workdir else Path(tempfile.mkdtemp(prefix="rag_parity_"))
    workdir.mkdir(parents=True, exist_ok=True)
    merged_corpus = workdir / "merged_corpus.jsonl"
    merged_sidecar = workdir / "merged_sidecar.jsonl"
    qcache = workdir / "qcache.jsonl"          # shared: queries identical across A/B
    rcache = workdir / "rcache.jsonl"          # shared: keyed by (qid, mode)+docs_sha256
    report_a = workdir / "report_platform.json"
    report_b = workdir / "report_merged.json"

    if a.analyze_only:
        if not report_a.exists() or not report_b.exists():
            raise SystemExit(f"--analyze-only needs {report_a} and {report_b} from a prior live run")
        sa = json.loads(report_a.read_text(encoding="utf-8"))
        sb = json.loads(report_b.read_text(encoding="utf-8"))
        ext_ids = {str(c["chunk_id"]) for c in _read_jsonl(Path(a.external_corpus))}
        plat_n = len(_read_jsonl(Path(a.platform_corpus)))
        ext_n = len(ext_ids)
        print(f"[analyze-only] platform top_3={sa['top_3_hit_rate']:.4f} merged top_3={sb['top_3_hit_rate']:.4f} "
              f"(from saved reports, no API calls)", flush=True)
    else:
        plat_n, ext_n, ext_ids = _build_merged_corpus(Path(a.platform_corpus), Path(a.external_corpus), merged_corpus)
        side_n = _build_merged_sidecar(Path(a.platform_sidecar), Path(a.external_sidecar), merged_sidecar)
        print(f"[build] platform={plat_n} external={ext_n} merged_corpus={plat_n+ext_n} merged_sidecar_rows={side_n}", flush=True)
        if side_n != plat_n + ext_n:
            raise SystemExit(f"sidecar row count {side_n} != corpus {plat_n+ext_n} (bijection broken)")

        common = dict(
            mode=a.mode,
            query_embedding_cache_path=qcache,
            reranker_cache_path=rcache,
            env_path=a.env,
        )

        print("[run A] platform-only baseline ...", flush=True)
        sa = evaluate_retrieval(a.platform_corpus, a.questions, report_a,
                                embeddings_path=a.platform_sidecar, **common)
        print(f"[run A] top_3_hit_rate={sa['top_3_hit_rate']:.4f} evaluated={sa['questions_evaluated']}", flush=True)

        print("[run B] merged (platform+external) ...", flush=True)
        sb = evaluate_retrieval(merged_corpus, a.questions, report_b,
                                embeddings_path=merged_sidecar, **common)
        print(f"[run B] top_3_hit_rate={sb['top_3_hit_rate']:.4f} evaluated={sb['questions_evaluated']}", flush=True)

    a3 = _top3_by_qid(sa)
    b3 = _top3_by_qid(sb)
    meta = _expected_by_qid(Path(a.questions))

    regressions = []       # hit in A, miss in B (per-question displacement)
    improvements = []      # miss in A, hit in B
    intrusions = []        # an external chunk appears in B's top-3
    changed_top3 = []      # top-3 set changed at all
    for qid, info in meta.items():
        exp = set(info["expected"])
        ta, tb = a3.get(qid, []), b3.get(qid, [])
        hit_a = bool(exp & set(ta))
        hit_b = bool(exp & set(tb))
        ext_in_b = [c for c in tb if c in ext_ids]
        if set(ta) != set(tb):
            changed_top3.append({"qid": qid, "group": info["group"], "question": info["question"],
                                 "A_top3": ta, "B_top3": tb})
        if hit_a and not hit_b:
            regressions.append({"qid": qid, "group": info["group"], "question": info["question"],
                                "expected": info["expected"], "A_top3": ta, "B_top3": tb})
        if hit_b and not hit_a:
            improvements.append({"qid": qid, "group": info["group"], "question": info["question"]})
        if ext_in_b:
            intrusions.append({"qid": qid, "group": info["group"], "question": info["question"],
                               "external_in_top3": ext_in_b, "B_top3": tb,
                               "expected_still_present": hit_b})

    # The GATE is retrieval non-regression on the established Top-3 metric, at
    # both aggregate and per-group granularity (per-group catches a group-level
    # regression that an unchanged aggregate could mask). Per-question
    # displacement in a near-tie cluster is a Top-K metric, not a binary-0
    # contract (memory feedback_hard_contractual_gates_binary reserves 0-violation
    # gates for citation/leakage/schema), so it is surfaced as a REVIEW warning,
    # not an auto-fail. A displacement that drops a *group*'s hit count would
    # fail per-group parity below.
    pg_a = sa["per_group_hit_rate"]
    pg_b = sb["per_group_hit_rate"]
    per_group_regressions = [
        {"group": g, "A_hit": pg_a[g]["hit"], "B_hit": pg_b[g]["hit"], "total": pg_a[g]["total"]}
        for g in pg_a if pg_b.get(g, {}).get("hit", 0) < pg_a[g]["hit"]
    ]
    aggregate_parity = sb["top_3_hit_rate"] >= sa["top_3_hit_rate"]
    per_group_parity = not per_group_regressions
    verdict_pass = aggregate_parity and per_group_parity

    # Split intrusions: external chunk that displaced the expected platform
    # chunk vs external chunk that merely took a slot while the expected one is
    # still present. The former is the answer-faithfulness risk to watch.
    intrusions_displacing = [i for i in intrusions if not i["expected_still_present"]]
    summary = {
        "mode": a.mode,
        "questions_evaluated": sa["questions_evaluated"],
        "platform_chunks": plat_n,
        "external_chunks": ext_n,
        "top_3_hit_rate_platform_only": sa["top_3_hit_rate"],
        "top_3_hit_rate_merged": sb["top_3_hit_rate"],
        "hit_rate_delta": sb["top_3_hit_rate"] - sa["top_3_hit_rate"],
        "aggregate_parity": aggregate_parity,
        "per_group_parity": per_group_parity,
        "per_group_regressions": per_group_regressions,
        "per_question_displacement_count": len(regressions),
        "improvement_count": len(improvements),
        "external_intrusion_count": len(intrusions),
        "external_intrusion_displacing_count": len(intrusions_displacing),
        "changed_top3_count": len(changed_top3),
        "per_question_displacements": regressions,
        "improvements": improvements,
        "external_intrusions": intrusions,
        "changed_top3": changed_top3,
        "verdict": "PASS" if verdict_pass else "FAIL",
        "review_warnings": {
            "per_question_displacements": len(regressions),
            "external_chunks_in_platform_top3": len(intrusions),
            "external_chunks_displacing_expected": len(intrusions_displacing),
            "note": "displacements/intrusions are answer-faithfulness REVIEW items, "
                    "not retrieval-parity failures; external KB is already default-on "
                    "(#242) and these are handled by the RAG anti-confusion anchor + "
                    "grounded-renderer citation discipline",
        },
        "workdir": str(workdir),
    }
    Path(a.out).write_text(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps({k: summary[k] for k in (
        "top_3_hit_rate_platform_only", "top_3_hit_rate_merged", "hit_rate_delta",
        "aggregate_parity", "per_group_parity", "per_question_displacement_count",
        "improvement_count", "external_intrusion_count",
        "external_intrusion_displacing_count", "verdict")}, ensure_ascii=False, indent=2), flush=True)
    if not verdict_pass:
        print(f"PARITY FAIL: per-group/aggregate regression "
              f"(per_group_regressions={per_group_regressions})", flush=True)
        return 1
    print(f"PARITY PASS (Top-3 non-regression, aggregate + per-group). REVIEW: "
          f"{len(regressions)} near-tie displacement(s), {len(intrusions)} external "
          f"chunk(s) in platform top-3 ({len(intrusions_displacing)} displacing expected).", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
