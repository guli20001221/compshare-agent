# CI behavioral-gate spec generator + reference checker (axis-A, machine-checkable).
#
# Source of gates: golden_two_axis_2026-06-22.jsonl (axisA strong predicates).
# Signal: replay-output JSONL (http_session_replay.go) per-turn steps[]/confirmations[]/reply.
#   - steps[].type in {tool_call, tool_result, confirm_needed, blocked, error}
#   - steps[].action = real tool/workflow name (DescribeCompShareInstance, DiagnosePortOrFirewall,
#     CreateInstanceWorkflow, StopInstanceWorkflow, ...)
#   - confirmations[].action = confirm-frame workflow identity
#   - reply / final_reply = text
# DECOUPLING: every gate asserts an OBSERVABLE outcome (tool/workflow invoked, confirm frame,
#   reply text). NEVER asserts intent_router.intent / actual_execution_path / bucket.
#
# Usage:
#   python ci_behavioral_gates.py gen     # -> ci_behavioral_gates_2026-06-22.jsonl
#   python ci_behavioral_gates.py check <replay_output.jsonl>   # evaluate + summary

import json, io, sys, re, os
from collections import Counter, defaultdict

_HERE = os.path.dirname(os.path.abspath(__file__))
# gen() input — the two-axis golden set (a research artifact, not committed; obtain
# it to regenerate the contract). check()/the committed contract use OUT_SPEC only.
TWO_AXIS = os.path.join(_HERE, "golden_two_axis_2026-06-22.jsonl")
OUT_SPEC = os.path.join(_HERE, "ci_behavioral_gates_2026-06-22.jsonl")

# strong predicates (the CI hard-gate set); non_empty kept as an explicit baseline gate.
STRONG = {"route_not_create","route_no_misroute","route_to_diagnosis","reach_confirm",
          "slot_retain","no_false_cancel","read_truth","state_honest","must_confirm_mutating"}

CREATE_ACTIONS = ["CreateInstanceWorkflow","CreateInstanceDef","deploy_create"]
# raw mutating API executes — these run ONLY after a confirmation; in a confirm=false replay
# any of these = the confirm gate was bypassed.
RAW_MUTATE = ["StartCompShareInstance","StopCompShareInstance","RebootCompShareInstance",
              "ResetCompShareInstancePassword","CreateCompShareInstance","DeleteCompShareInstance",
              "TerminateCompShareInstance","ResizeCompShareInstance","ModifyCompShareInstanceName",
              "AttachCompShareDisk","DetachCompShareDisk"]
CANCEL_PHRASES = ["已取消","取消了","已为您取消","已为你取消","取消关机","取消了关机操作","已撤销","已停止操作","已帮你取消","已帮您取消"]
REASK_PHRASES  = ["是哪台实例","请选择哪","请选择实例","请提供实例","请告诉我实例","您要操作哪","请指定实例","请问要操作哪","要查询哪台","请选择要"]

READ_TOOLS = {
    "DescribeCompShareInstance":            ["列实例","列出实例","实例列表","实例状态","哪些实例","我的实例","describecompshareinstance","实例真实状态","各实例"],
    "DescribeAvailableCompShareInstanceTypes":["在售","可用类型","型号","describeavailable","可用卡型","gpu 列表","在售型号"],
    "DescribeCommunityImages":              ["社区镜像","describecommunityimages"],
    "DescribeCompShareImages":              ["平台镜像","describecompshareimages"],
    "DescribeCompShareCustomImages":        ["自制镜像","自定义镜像","describecompsharecustomimages"],
    "GetCompShareInstanceMonitor":          ["监控","利用率","getcompshareinstancemonitor","gpu/显存/cpu"],
    "GetCompShareInstanceUserPrice":        ["价格","价目","getcompshareinstanceuserprice","getcompshareinstanceprice","真值"],
}
DIAG_TOOLS = {
    "DiagnosePortOrFirewall": ["diagnoseportorfirewall","端口","防火墙","连接被拒","comfyui","err_connection"],
    "DiagnoseGPU":            ["diagnosegpu","显卡","识别","多卡","nvidia"],
    "DiagnoseInitFailure":    ["diagnoseinitfailure","启动中","初始化","开机失败","init"],
    "DiagnoseBilling":        ["diagnosebilling","费率"],
}
CONFIRM_WF = {
    "StopInstanceWorkflow":       ["关机","stop","已 stopped","stopinstance"],
    "StartInstanceWorkflow":      ["开机","start","无卡","startinstance"],
    "CreateInstanceWorkflow":     ["创建","create","新建"],
    "CreateDiskWorkflow":         ["数据盘","磁盘","disk","挂载","createdisk"],
    "SetStopSchedulerWorkflow":   ["定时关机","定时","scheduled","stopscheduler"],
    "ResizeInstanceWorkflow":     ["变配","resize"],
    "RebootInstanceWorkflow":     ["重启","reboot"],
}

def pick(amap, text, default=None):
    t = text.lower()
    for tool, kws in amap.items():
        if any(k.lower() in t for k in kws):
            return tool
    return default

def gen():
    cases = [json.loads(l) for l in io.open(TWO_AXIS, encoding="utf-8") if l.strip()]
    specs = []
    for c in cases:
        cid = c["case"]; a = set(c.get("axisA", [])); s = c.get("assertion","")
        strong = a & STRONG
        if not strong and "non_empty" not in a:
            continue
        if "route_not_create" in a:
            specs.append(dict(case_id=cid, gate="route_not_create", kind="forbid_step_action",
                              params=dict(actions=CREATE_ACTIONS), note="must-not 进创建/部署确认 saga"))
        if "route_no_misroute" in a:
            # best-effort: if the misroute risk is create/terminate, forbid create; else flag reply-semantic
            forbid = CREATE_ACTIONS if any(k in s for k in ["创建","Terminate","删除实例","注销","Coding"]) else CREATE_ACTIONS
            specs.append(dict(case_id=cid, gate="route_no_misroute", kind="forbid_step_action",
                              params=dict(actions=forbid), note="未误路由到无关 mutating 流程",
                              reply_semantic=not any(k in s for k in ["创建","Terminate","删除实例","注销","Coding"])))
        if "route_to_diagnosis" in a:
            tool = pick(DIAG_TOOLS, s, default=None)
            actions = [tool] if tool else list(DIAG_TOOLS.keys())
            specs.append(dict(case_id=cid, gate="route_to_diagnosis", kind="require_step_action",
                              params=dict(actions=actions), note="进入对应诊断"))
        if "read_truth" in a:
            tool = pick(READ_TOOLS, s, default="DescribeCompShareInstance")
            actions = [tool]
            if tool == "GetCompShareInstanceUserPrice":
                actions = ["GetCompShareInstanceUserPrice","GetCompShareInstancePrice"]
            specs.append(dict(case_id=cid, gate="read_truth", kind="require_step_action",
                              params=dict(actions=actions), note="调对应只读 API 反映真值"))
        if "reach_confirm" in a:
            wf = pick(CONFIRM_WF, s, default=None)
            if wf:
                specs.append(dict(case_id=cid, gate="reach_confirm", kind="require_confirm_action",
                                  params=dict(actions=[wf]), note="到达对应变更确认门"))
            else:
                specs.append(dict(case_id=cid, gate="reach_confirm", kind="require_any_confirm",
                                  params=dict(), note="到达确认门(workflow 未定)"))
        if "must_confirm_mutating" in a:
            specs.append(dict(case_id=cid, gate="must_confirm_mutating", kind="forbid_step_action",
                              params=dict(actions=RAW_MUTATE),
                              note="mutating 须经确认门:replay confirm=false→任何 raw 执行=确认门被绕过=FAIL"))
        if "no_false_cancel" in a:
            specs.append(dict(case_id=cid, gate="no_false_cancel", kind="forbid_reply_substring",
                              params=dict(substrings=CANCEL_PHRASES), note="不谎称已取消"))
        if "slot_retain" in a:
            specs.append(dict(case_id=cid, gate="slot_retain", kind="forbid_reply_substring",
                              params=dict(substrings=REASK_PHRASES), note="保持已选实例不重问",
                              reply_semantic=True))
        if "state_honest" in a:
            specs.append(dict(case_id=cid, gate="state_honest", kind="judge_assisted",
                              params=dict(), note="如实反映实例状态(需 ground-truth fixture / judge)"))
        if "non_empty" in a:
            specs.append(dict(case_id=cid, gate="non_empty", kind="require_nonempty",
                              params=dict(), note="非空、非裸错误、非 abort"))
    with io.open(OUT_SPEC, "w", encoding="utf-8") as f:
        for sp in specs:
            f.write(json.dumps(sp, ensure_ascii=False) + "\n")
    by_gate = Counter(sp["gate"] for sp in specs)
    by_kind = Counter(sp["kind"] for sp in specs)
    ncases = len(set(sp["case_id"] for sp in specs))
    print(f"wrote {len(specs)} assertions over {ncases} cases -> {OUT_SPEC}")
    print("by gate:", dict(by_gate))
    print("by kind:", dict(by_kind))

# ---------- checker ----------
def load_replay(path):
    by_case = {}
    for l in io.open(path, encoding="utf-8"):
        l=l.strip()
        if not l: continue
        o=json.loads(l)
        by_case[o.get("case_id","")] = o
    return by_case

def all_step_actions(rec):
    acts=set()
    for t in rec.get("turns",[]):
        for st in t.get("steps",[]) or []:
            if st.get("type") in ("tool_call","confirm_needed") and st.get("action"):
                acts.add(st["action"])
    return acts

def confirm_reached_for(rec, want):
    """A confirm gate was reached for workflow X iff some turn has a confirm_needed step
    AND that turn carries the workflow's identity — either confirmations[].action==X, the
    confirm_needed step's action==X, or (old harness) a tool_call step action==X in the
    same turn (the *Workflow tool_call that precedes the confirm)."""
    want=set(want)
    for t in rec.get("turns",[]):
        steps=t.get("steps",[]) or []
        has_cn = any(s.get("type")=="confirm_needed" for s in steps) or t.get("confirmation_count",0)>0
        if not has_cn:
            continue
        ids=set()
        for c in t.get("confirmations",[]) or []:
            if c.get("action"): ids.add(c["action"])
        for s in steps:
            if s.get("type") in ("confirm_needed","tool_call") and s.get("action"):
                ids.add(s["action"])
        if ids & want:
            return True
    return False

def has_any_confirm(rec):
    for t in rec.get("turns",[]):
        if t.get("confirmation_count",0)>0: return True
        for st in t.get("steps",[]) or []:
            if st.get("type")=="confirm_needed": return True
    return False

def all_replies(rec):
    out=[]
    for t in rec.get("turns",[]):
        if t.get("reply"): out.append(t["reply"])
    if rec.get("final_reply"): out.append(rec["final_reply"])
    return out

def final_reply_nonempty(rec):
    fr = (rec.get("final_reply") or "").strip()
    if not fr: return False
    if rec.get("error"): return False
    # last turn must not be a bare error / empty
    turns = rec.get("turns",[])
    if turns and (turns[-1].get("error_code")): return False
    return True

def evaluate(specs, by_case):
    results=[]
    for sp in specs:
        cid=sp["case_id"]; rec=by_case.get(cid)
        if rec is None:
            results.append((sp,"NOCASE")); continue
        kind=sp["kind"]; p=sp["params"]
        if kind=="require_step_action":
            ok = bool(all_step_actions(rec) & set(p["actions"]))
            results.append((sp,"PASS" if ok else "FAIL"));
        elif kind=="forbid_step_action":
            ok = not (all_step_actions(rec) & set(p["actions"]))
            results.append((sp,"PASS" if ok else "FAIL"))
        elif kind=="require_confirm_action":
            ok = confirm_reached_for(rec, p["actions"])
            results.append((sp,"PASS" if ok else "FAIL"))
        elif kind=="require_any_confirm":
            results.append((sp,"PASS" if has_any_confirm(rec) else "FAIL"))
        elif kind=="forbid_reply_substring":
            subs=p["substrings"]; bad=any(any(x in r for x in subs) for r in all_replies(rec))
            results.append((sp,"PASS" if not bad else "FAIL"))
        elif kind=="require_nonempty":
            results.append((sp,"PASS" if final_reply_nonempty(rec) else "FAIL"))
        elif kind=="judge_assisted":
            results.append((sp,"SKIP_JUDGE"))
        else:
            results.append((sp,"UNKNOWN_KIND"))
    return results

def check(path):
    specs=[json.loads(l) for l in io.open(OUT_SPEC,encoding="utf-8") if l.strip()]
    by_case=load_replay(path)
    res=evaluate(specs, by_case)
    agg=Counter(v for _,v in res)
    per_gate=defaultdict(Counter)
    for sp,v in res: per_gate[sp["gate"]][v]+=1
    print(f"replay: {path}  cases_in_replay={len(by_case)}")
    print("OVERALL:", dict(agg))
    print("\nper-gate verdicts:")
    for g in sorted(per_gate):
        print(f"  {g:22s} {dict(per_gate[g])}")
    # show concrete FAILs (machine-checkable ones, exclude judge/nocase)
    fails=[(sp,v) for sp,v in res if v=="FAIL"]
    print(f"\nFAIL count (checkable): {len(fails)}  (note: 220 ran on stale 7638fe7; FAILs validate the CHECKER, not current-main bugs)")
    for sp,v in fails[:25]:
        print(f"  {sp['case_id']:5s} {sp['gate']:20s} {sp['kind']}")

if __name__=="__main__":
    cmd = sys.argv[1] if len(sys.argv)>1 else "gen"
    if cmd=="gen":
        gen()
    elif cmd=="check":
        check(sys.argv[2])
    else:
        print("usage: ci_behavioral_gates.py gen | check <replay.jsonl>")
