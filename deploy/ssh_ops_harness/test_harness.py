"""Offline gate for the harness wrapper's SDK-independent logic (no network / no SSH / no SDK).

Run:  python test_harness.py   ->  exits non-zero on ANY failure.

Asserts the boundary contract: stdin handshake (never env), Phase-1 read-only enforcement, the
credential never reaching the audit/output, and INV-9 (only ssh_exec exposed). The transport is
monkeypatched so nothing actually connects.
"""
import os
import sys
import time
import types

import guardrails
import harness
import ssh_transport

_REAL_RUN_SSH = ssh_transport.run_ssh        # captured before the dispatch tests monkeypatch it

FAILS = []


def check(name, cond):
    if not cond:
        FAILS.append(name)
        print(f"XX  {name}")


# --- handshake parsing: required fields, and the credential goes to a module var, NOT os.environ ---
conn = harness.read_handshake('{"host":"1.2.3.4","user":"ubuntu","port":22,"password":"Pl4inPwd77x"}')
check("handshake-parses", conn["host"] == "1.2.3.4" and conn["password"] == "Pl4inPwd77x")

for bad in ['{"user":"u","port":22,"password":"x"}',     # missing host
            '{"host":"h","user":"u","port":22}',         # missing password/key
            'not json']:
    try:
        harness.read_handshake(bad)
        check(f"handshake-rejects::{bad[:20]}", False)
    except Exception:
        check(f"handshake-rejects::{bad[:20]}", True)

harness.set_conn(conn)
check("cred-not-in-environ", "Pl4inPwd77x" not in "".join(os.environ.values()))
check("secrets-has-pw-and-b64", harness._secrets()[0] == "Pl4inPwd77x" and len(harness._secrets()) == 2)


# --- CLI selection: the SDK bundles an older CLI and prefers it unless cli_path is explicit. ---
_real_which = harness.shutil.which
harness.shutil.which = lambda name: "/usr/local/bin/claude" if name == "claude" else None
check("cli-selects-operator-pinned-binary", harness.resolve_claude_cli() == "/usr/local/bin/claude")
harness.shutil.which = lambda _name: None
try:
    harness.resolve_claude_cli()
    check("cli-missing-fails-closed", False)
except SystemExit:
    check("cli-missing-fails-closed", True)
finally:
    harness.shutil.which = _real_which


# --- Phase-1 dispatch: read_only runs, mutating/destructive are refused WITHOUT touching SSH ---
calls = []


def fake_run_ssh(c, command, secrets=()):
    calls.append((command, secrets))
    # faithfully simulate the REAL transport: it scrubs (incl. the literal credential) before returning
    raw = f"ok {c['password']} TOKEN={c['password']}"
    return {"exit_code": 0, "stdout": guardrails.scrub_output(raw, secrets),
            "stderr": "", "truncated": False}


ssh_transport.run_ssh = fake_run_ssh        # monkeypatch the transport
harness.AUDIT.clear()

r_destr = harness.run_command("rm -rf /")
check("destructive-refused", r_destr["is_error"] and not r_destr["executed"] and r_destr["tier"] == "destructive")

r_mut = harness.run_command("systemctl restart vllm")
check("mutating-refused", r_mut["is_error"] and not r_mut["executed"] and r_mut["tier"] == "mutating")

check("no-ssh-on-refusal", len(calls) == 0)   # neither refused command reached the transport

r_ro = harness.run_command("nvidia-smi -q")
check("readonly-executed", r_ro["executed"] and not r_ro["is_error"] and r_ro["tier"] == "read_only")
check("readonly-reached-ssh", len(calls) == 1 and calls[0][0] == "nvidia-smi -q")
check("secrets-passed-to-transport", "Pl4inPwd77x" in calls[0][1])
check("output-credential-scrubbed", "Pl4inPwd77x" not in r_ro["text"])   # box echoed it; must be gone

# --- audit: one record per command, tier+disposition set, credential never recorded ---
check("audit-count", len(harness.AUDIT) == 3)
check("audit-dispositions",
      [e["disposition"] for e in harness.AUDIT]
      == ["refused_destructive", "refused_mutating_phase1", "ran_read_only"])
check("audit-no-credential", all("Pl4inPwd77x" not in str(e) for e in harness.AUDIT))


# --- auth failure (stale credential) fails fast, credential-free, not executed ---
ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "auth_failed"}
r_auth = harness.run_command("df -h /")
check("auth-fail-not-executed", r_auth["is_error"] and not r_auth["executed"])
check("auth-fail-no-credential", "Pl4inPwd77x" not in r_auth["text"])
check("auth-fail-hints-stale", "stale" in r_auth["text"])


# --- connect_failed is a CATCH-ALL, so the exception class ssh_transport recorded has to reach the
# text. It was captured and dropped on every failure, which is how a 2026-08-05 investigation spent
# its time on a port that was in fact correct. The class name is a type name — no credential, no
# host — so it is safe to show. ---
ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed",
                                                        "detail": "NoValidConnectionsError"}
r_conn = harness.run_command("df -h /")
check("connect-fail-not-executed", r_conn["is_error"] and not r_conn["executed"])
check("connect-fail-carries-exception-class", "NoValidConnectionsError" in r_conn["text"])
check("connect-fail-no-credential", "Pl4inPwd77x" not in r_conn["text"])
# It must NOT assert a cause it never observed; "unreachable" was stated as fact for every class.
check("connect-fail-states-dial-not-verdict", "did not complete" in r_conn["text"])

ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed"}
r_conn_bare = harness.run_command("df -h /")
check("connect-fail-without-detail-has-no-empty-parens", "()" not in r_conn_bare["text"])


# --- F2 preflight: reachable -> None; connect/auth failure -> actionable Chinese reason (fast-fail).
# WHY: without it, an unreachable instance makes the agent spend its whole time budget with every
# proposed command hanging at the SSH connect timeout (observed as a 5-minute 0-output timeout). ---
_pf_cmds = []


def _pf_ok(c, command, secrets=()):
    _pf_cmds.append(command)
    return {"exit_code": 0, "stdout": "", "stderr": "", "truncated": False}


ssh_transport.run_ssh = _pf_ok
check("preflight-reachable-none", harness.preflight_probe(harness._CONN) is None)
check("preflight-uses-fixed-benign-cmd", _pf_cmds == ["true"])   # deterministic, never model-chosen

ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed"}
_pf_conn = harness.preflight_probe(harness._CONN)
check("preflight-unreachable-reason", isinstance(_pf_conn, str) and "无法建立 SSH 连接" in _pf_conn)
# The candidate that was missing and that actually mattered: a user who can SSH from their own
# machine cannot see that the host running this service has no route to the same box.
check("preflight-names-this-hosts-route", "运行本服务的主机" in _pf_conn)
check("preflight-without-detail-has-no-empty-parens", "（）" not in _pf_conn)

ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed",
                                                        "detail": "gaierror"}
_pf_cls = harness.preflight_probe(harness._CONN)
check("preflight-carries-exception-class", "gaierror" in _pf_cls)
# A class the map does not know must still reach the operator rather than collapsing to a generic
# sentence — an unrecognised error is exactly the case where the class name is the only evidence.
ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "ssh_transport_exploded",
                                                        "detail": "TypeError"}
_pf_unknown = harness.preflight_probe(harness._CONN)
check("preflight-unknown-class-still-reports-both",
      "ssh_transport_exploded" in _pf_unknown and "TypeError" in _pf_unknown)

ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "auth_failed"}
_pf_auth = harness.preflight_probe(harness._CONN)
check("preflight-auth-reason", isinstance(_pf_auth, str) and "认证失败" in _pf_auth)
check("preflight-no-credential", "Pl4inPwd77x" not in (_pf_conn + _pf_auth))


# --- The dial names the PORT, and says what the exception class actually establishes. ---
# WHY the port: this lane dials 22 on a VM, 23 on a container image, and an arbitrary high forward
# port on a pod. "SSH 端口未放通" without the number sends whoever reads it to test the wrong one —
# on 2026-08-06 the failing instance had BOTH 22 and 23 open and the dial still timed out, so the
# port the reader would have checked by default was open and proved nothing.
# WHY per-class wording: calibrated against real endpoints on paramiko 3.5.1 (the >=3.4,<4 line prod
# pins). A cloud security group DROPS rather than RSTs, so "port blocked" arrives as TimeoutError,
# never as a refusal; offering "端口未放通" for a timeout is the same wrong-layer push #516 removed.
_conn23 = dict(harness._CONN, port=23)


def _dial(detail):
    ssh_transport.run_ssh = lambda c, command, secrets=(): (
        {"error": "connect_failed"} if detail is None else {"error": "connect_failed", "detail": detail})
    return harness.preflight_probe(_conn23)


_pf_timeout = _dial("TimeoutError")
check("preflight-names-the-dialed-port", "23 端口" in _pf_timeout)
check("preflight-timeout-says-no-response", "没有收到任何响应" in _pf_timeout)
# A timeout is NOT a refusal and NOT a resolution failure; saying so is what stops the reader from
# testing the port and concluding the caller is at fault.
check("preflight-timeout-rules-out-refusal", "既不是被拒绝" in _pf_timeout)
check("preflight-timeout-still-names-this-hosts-route", "运行本服务的主机" in _pf_timeout)
check("preflight-timeout-carries-class", "TimeoutError" in _pf_timeout)

_pf_refused = _dial("NoValidConnectionsError")
check("preflight-refused-says-refused", "明确拒绝了连接" in _pf_refused)
check("preflight-refused-names-the-dialed-port", "23 端口" in _pf_refused)
# The two dial failures must not read the same: a refusal proves the host WAS reached, a timeout
# proves nothing came back. Collapsing them is what made connect_failed useless in the first place.
# Compared with the trailing （ClassName） stripped — otherwise the class suffix alone satisfies
# this, and the check passes even when both sentences collapse back to one generic paragraph.
def _body(reason):
    return reason.rsplit("（", 1)[0]


check("preflight-refused-differs-from-timeout", _body(_pf_refused) != _body(_pf_timeout))
check("preflight-refused-does-not-claim-drop", "没有收到任何响应" not in _pf_refused)

_pf_dns = _dial("gaierror")
check("preflight-dns-says-resolution", "无法解析" in _pf_dns)

# An UNKNOWN class must keep the generic candidate list (we have not calibrated it) and still name
# the port and the class — an unrecognised failure is exactly when both are the only evidence.
_pf_novel = _dial("SSHException")
check("preflight-unknown-dial-class-keeps-candidates", "运行本服务的主机" in _pf_novel)
check("preflight-unknown-dial-class-names-port", "23 端口" in _pf_novel)
check("preflight-unknown-dial-class-carries-class", "SSHException" in _pf_novel)
check("preflight-no-detail-still-names-port", "23 端口" in _dial(None))

# The per-command path names it too: mid-run failures are read by the model, which relays them.
harness.set_conn(dict(conn, port=23))
ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed",
                                                        "detail": "TimeoutError"}
_rc_port = harness.run_command("df -h /")["text"]
check("connect-fail-names-the-dialed-port", "port 23" in _rc_port)
harness.set_conn(conn)


# --- The dial also names WHICH ROUTE it took: the instance's internal IPv6, or its public address. ---
# The two failures are byte-identical without it and mean opposite things. A timeout on the public
# address says this host has no route out (the 2026-08-06 production failure: 3 instances x 2 ports x
# 2 regions all timed out from the deployment while the identical code connected in under 1.2s from a
# normal network). A timeout on the internal IPv6 says the address resolved and nothing answered on
# it — a different team and a different fix. Whoever reads the message cannot tell them apart from
# the port, the class, or anything else in the sentence.
check("route-v6-named", harness._dialled_route("2003:da8:2004:1000::1") == "内网 IPv6 地址")
check("route-v4-named", harness._dialled_route("203.0.113.10") == "公网地址")
check("route-absent-is-silent", harness._dialled_route(None) == "" and harness._dialled_route("") == "")

_conn_v6 = dict(harness._CONN, port=23, host="2003:da8:2004:1000:a3c:7623:2712:f9c0")
_conn_v4 = dict(harness._CONN, port=23, host="203.0.113.10")


def _dial_via(host_conn, detail="TimeoutError"):
    ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed", "detail": detail}
    return harness.preflight_probe(host_conn)


_pf_v6, _pf_v4 = _dial_via(_conn_v6), _dial_via(_conn_v4)
check("preflight-names-the-internal-route", "内网 IPv6 地址" in _pf_v6)
check("preflight-names-the-public-route", "公网地址" in _pf_v4)
check("preflight-routes-differ", _pf_v6 != _pf_v4)
# The literal address stays OUT of user-facing text: an internal IPv6 is not something the user can
# act on, and the port already carries everything they can check for themselves.
check("preflight-does-not-echo-the-address", "2003:da8" not in _pf_v6 and "203.0.113.10" not in _pf_v4)
# An auth failure is about the credential, not the route, but which address was reached still
# distinguishes "wrong password" from "wrong box" — so it is named there too.
ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "auth_failed"}
check("preflight-auth-names-the-route", "内网 IPv6 地址" in harness.preflight_probe(_conn_v6))

# The per-command path names it too, in the model-facing English half.
harness.set_conn(dict(conn, port=23, host="2003:da8:2004:1000:a3c:7623:2712:f9c0"))
ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed",
                                                        "detail": "TimeoutError"}
_rc_v6 = harness.run_command("df -h /")["text"]
check("connect-fail-names-the-internal-route", "internal IPv6" in _rc_v6)
harness.set_conn(dict(conn, port=23, host="203.0.113.10"))
_rc_v4 = harness.run_command("df -h /")["text"]
check("connect-fail-names-the-public-route", "public address" in _rc_v4)
harness.set_conn(conn)


# --- the REAL ssh_transport.run_ssh genuinely scrubs box output (fake paramiko -> no connection) ---
_PW = "Pl4in" + "Pwd77x"
_AWS = "wJalr" + "XUtnFE" + "MIK7MD" + "ENGbPx"


def _make_fake_paramiko(never_exits=False):
    m = types.ModuleType("paramiko")

    class _Chan:
        """Models the paramiko Channel surface the transport pumps. `never_exits` reproduces a
        blocking command (`cat` with no file, a wedged mount): bytes arrive, the exit status never
        does. That is the shape that silently ate the whole 12m wall clock in 3 of 9 live runs."""

        def __init__(self, out, err):
            self._out, self._err, self._done = out, err, False

        def recv_ready(self):
            return bool(self._out)

        def recv_stderr_ready(self):
            return bool(self._err)

        def recv(self, n):
            d, self._out = self._out[:n], self._out[n:]
            if not self._out:
                self._done = True
            return d

        def recv_stderr(self, n):
            d, self._err = self._err[:n], self._err[n:]
            return d

        def exit_status_ready(self):
            return self._done and not never_exits

        def recv_exit_status(self):
            return 0

        def close(self):
            pass

    class _Stream:
        def __init__(self, chan):
            self.channel = chan

    class _Client:
        def set_missing_host_key_policy(self, *a):
            pass

        def connect(self, **k):
            pass

        def exec_command(self, command, timeout=None):
            out = f"role pw {_PW} and AWS_SECRET_ACCESS_KEY={_AWS}".encode()
            chan = _Chan(out, b"")
            return (None, _Stream(chan), _Stream(chan))

        def close(self):
            pass

    m.SSHClient = _Client
    m.AutoAddPolicy = object
    m.AuthenticationException = type("AuthenticationException", (Exception,), {})
    return m


sys.modules["paramiko"] = _make_fake_paramiko()
_res = _REAL_RUN_SSH(                          # the real transport, not the dispatch-test monkeypatch
    {"host": "1.2.3.4", "user": "ubuntu", "port": 22, "password": _PW}, "nvidia-smi",
    secrets=[_PW, "ignored"])
check("transport-scrubs-literal-credential", _PW not in _res["stdout"])
check("transport-scrubs-labeled-secret", _AWS not in _res["stdout"])

# A command that never returns must be cut at _EXEC_TIMEOUT, not left to eat the run's wall clock.
# The classifier refuses the KNOWN blockers, but it cannot prove termination, so the transport is
# the only place this can be enforced.
sys.modules["paramiko"] = _make_fake_paramiko(never_exits=True)
_saved_to = ssh_transport._EXEC_TIMEOUT
ssh_transport._EXEC_TIMEOUT = 1                # keep the suite fast; the mechanism is what matters
_t0 = time.monotonic()
_hung = _REAL_RUN_SSH({"host": "1.2.3.4", "user": "ubuntu", "port": 22, "password": _PW},
                      "cat", secrets=[_PW])
_elapsed = time.monotonic() - _t0
ssh_transport._EXEC_TIMEOUT = _saved_to
check("blocking-command-times-out", _hung.get("error") == "exec_timeout")
check("blocking-command-bounded-by-timeout", _elapsed < 5)
check("blocking-command-keeps-partial-output", "role pw" in _hung.get("partial", ""))
check("blocking-command-partial-is-scrubbed", _PW not in _hung.get("partial", ""))
check("transport-keeps-benign", "role pw" in _res["stdout"])


# --- INV-9: only ssh_exec + the text-only Skill tool; every other built-in stripped; only the
# "project" setting source (needed for skill discovery). Fail CLOSED otherwise. ---
def opts(allowed, disallowed, sources, tools="DEFAULT"):
    if tools == "DEFAULT":
        tools = list(harness.TOOLS_BASE)                 # valid: only Skill exists, no Bash/Read/Write
    return types.SimpleNamespace(tools=tools, allowed_tools=allowed, disallowed_tools=disallowed,
                                 setting_sources=sources)


good = opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, ["project"])
try:
    harness.assert_single_tool(good)
    check("inv9-accepts-good", True)
except SystemExit:
    check("inv9-accepts-good", False)

for name, bad in [
    ("inv9-rejects-extra-allowed", opts(harness.ALLOWED_TOOLS + ["Bash"], harness.DISALLOWED_TOOLS, ["project"])),
    ("inv9-rejects-missing-disallowed", opts(harness.ALLOWED_TOOLS, ["Read"], ["project"])),
    ("inv9-rejects-empty-allowed", opts([], harness.DISALLOWED_TOOLS, ["project"])),
    # setting_sources must be EXACTLY ["project"]: "user"/"local" would pull in the operator's own
    # ~/.claude config, and [] would stop the CLI discovering the staged skill at all.
    ("inv9-rejects-user-source", opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, ["user"])),
    ("inv9-rejects-project-plus-user", opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, ["project", "user"])),
    ("inv9-rejects-empty-sources", opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, [])),
    # `tools` is the existence off-switch. Only the exact Skill-only base passes: adding an executing
    # built-in, None (the SDK default = everything exists), or a missing field (an older SDK without
    # `tools`) all mean Bash/Read could exist on the CONTROL-PLANE host — each must fail closed.
    ("inv9-rejects-tools-with-builtin",
     opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, ["project"], tools=["Skill", "Bash"])),
    ("inv9-rejects-tools-none", opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, ["project"], tools=None)),
    ("inv9-rejects-tools-empty-kills-skill",
     opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, ["project"], tools=[])),
    ("inv9-rejects-tools-missing", types.SimpleNamespace(
        allowed_tools=harness.ALLOWED_TOOLS, disallowed_tools=harness.DISALLOWED_TOOLS,
        setting_sources=["project"])),
]:
    try:
        harness.assert_single_tool(bad)
        check(name, False)
    except SystemExit:
        check(name, True)


# The staging root must be reachable-CLAUDE.md-free: the CLI walks up from cwd for both skills AND
# CLAUDE.md, so a root inside the repo would inject this project's architecture doc — and one under
# $HOME the operator's personal one — into an agent whose verdict is shown to the customer.
_stage_root = harness.stage_skills()
check("stage-skill-present",
      os.path.isfile(os.path.join(_stage_root, ".claude", "skills", "instance-triage", "SKILL.md")))
check("stage-chdir-took-effect", os.path.realpath(os.getcwd()) == os.path.realpath(_stage_root))
check("stage-no-claude-md-leak", harness._claude_md_ancestors(_stage_root) == [])
check("stage-outside-repo",
      not os.path.realpath(_stage_root).startswith(os.path.realpath(os.path.dirname(harness.__file__))))


# --- stdout line protocol: @@STEP metadata-only per command, one terminal VERDICT block ------------
# The Go supervisor parses stdout; these pin the wire shape so a format regression fails offline
# instead of at the far end of a live run.

# D2 mapping: the six run_command dispositions collapse onto exactly three wire values, and anything
# unmapped (a future ssh error class, the "" left by an exception) is a failure, never a success.
check("wire-ran", harness._wire_disposition("ran_read_only") == "ran")
check("wire-refused-destructive", harness._wire_disposition("refused_destructive") == "refused")
check("wire-refused-mutating", harness._wire_disposition("refused_mutating_phase1") == "refused")
check("wire-no-connection", harness._wire_disposition("no_connection") == "failed")
check("wire-auth-failed", harness._wire_disposition("auth_failed") == "failed")
check("wire-connect-failed", harness._wire_disposition("connect_failed") == "failed")
check("wire-empty-is-failed", harness._wire_disposition("") == "failed")
check("wire-unknown-is-failed", harness._wire_disposition("something_new") == "failed")

import io as _io  # noqa: E402
import json as _json  # noqa: E402


def _capture(fn):
    real = sys.stdout
    buf = _io.StringIO()
    sys.stdout = buf
    try:
        fn()
    finally:
        sys.stdout = real
    return buf.getvalue()


# run three commands (refused, refused, ran) with the box echoing the credential, then emit a verdict.
harness.AUDIT.clear()
_BOXPW = "Pl4in" + "Pwd77x"
harness.set_conn({"host": "h", "user": "u", "port": 22, "password": _BOXPW})
ssh_transport.run_ssh = lambda c, command, secrets=(): {
    "exit_code": 0,
    "stdout": guardrails.scrub_output(f"secret-in-output {c['password']}", secrets),
    "stderr": "", "truncated": False}

proto = _capture(lambda: (
    harness.run_command("rm -rf /"),
    harness.run_command("systemctl restart vllm"),
    harness.run_command("nvidia-smi -q"),
    harness._emit_verdict("结论：显存 512MiB 已用，健康。"),
))
_plines = proto.splitlines()
_psteps = [l for l in _plines if l.startswith("@@STEP ")]

check("proto-one-step-per-command", len(_psteps) == 3)

_pobjs = []
_ok_json = True
for s in _psteps:
    try:
        _pobjs.append(_json.loads(s[len("@@STEP "):]))
    except Exception:
        _ok_json = False
check("proto-steps-are-json", _ok_json and len(_pobjs) == 3)
check("proto-dispositions",
      [o.get("disposition") for o in _pobjs] == ["refused", "refused", "ran"])
# Exact set, not a subset: an @@STEP line is metadata that reaches the user's activity stream and an
# audit row, so a field appearing here is a decision, never a leak. `reason` joined on 2026-08-08 —
# it carries the SIX-valued disposition the three-valued `disposition` above throws away, which is
# what lets the server say WHICH gate refused instead of one sentence covering four of them.
check("proto-step-fields-present",
      all(set(o) == {"command", "tier", "disposition", "reason", "exit", "bytes"} for o in _pobjs))
# ...and it must be the specific value, or the server is back to guessing.
check("proto-step-reason-is-the-specific-disposition",
      [o.get("reason") for o in _pobjs][:2] == ["refused_destructive", "refused_mutating_phase1"])

# INV-6: the box echoed the credential and a marker; NEITHER may appear in an @@STEP line.
check("proto-inv6-no-output-in-step",
      all("secret-in-output" not in s for s in _psteps))
check("proto-inv6-no-credential-in-step",
      all(_BOXPW not in s for s in _psteps))

# ordering: every @@STEP precedes the single <<<VERDICT>>> block.
_vidx = next((i for i, l in enumerate(_plines) if l.strip() == "<<<VERDICT>>>"), -1)
_eidx = next((i for i, l in enumerate(_plines) if l.strip() == "<<<END>>>"), -1)
_sidx = [i for i, l in enumerate(_plines) if l.startswith("@@STEP ")]
check("proto-verdict-block-present", _vidx >= 0 and _eidx > _vidx)
check("proto-exactly-one-verdict", _plines.count("<<<VERDICT>>>") == 1 and _plines.count("<<<END>>>") == 1)
check("proto-steps-before-verdict", _vidx >= 0 and (not _sidx or max(_sidx) < _vidx))

# a command's bytes count reflects (scrubbed) output length, and refused commands carry 0.
check("proto-ran-has-bytes", _pobjs[2]["bytes"] > 0)
check("proto-refused-zero-bytes", _pobjs[0]["bytes"] == 0 and _pobjs[1]["bytes"] == 0)

# the verdict body is scrubbed too (V5): a credential quoted into the model's own text is removed.
_vbody = _capture(lambda: harness._emit_verdict(f"密码是 {_BOXPW} 请注意"))
check("proto-verdict-scrubbed", _BOXPW not in _vbody)

# --- @@OUTCOME: the audit must be able to tell a refused dial from a real diagnosis --------------
# Before this line existed, both ended with exit 0 and a verdict, so ssh_ops_audit recorded 12/12
# attempts as disposition='ok' — including every one whose dial never landed.
ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "connect_failed",
                                                        "detail": "TimeoutError"}
harness._PREFLIGHT_ERR_CLASS = ""
harness.preflight_probe(harness._CONN)
check("outcome-records-the-exception-class", harness._PREFLIGHT_ERR_CLASS == "TimeoutError")

# A reachable box must leave the class EMPTY, or "there is a class" stops meaning "the dial failed".
ssh_transport.run_ssh = _pf_ok
harness._PREFLIGHT_ERR_CLASS = ""
harness.preflight_probe(harness._CONN)
check("outcome-clean-dial-sets-no-class", harness._PREFLIGHT_ERR_CLASS == "")

_oc = _capture(lambda: harness._emit_outcome("preflight_failed", "TimeoutError"))
check("outcome-line-is-one-json-line", _oc.startswith("@@OUTCOME {") and _oc.count("\n") == 1)
check("outcome-line-carries-class", '"err_class": "TimeoutError"' in _oc.replace('"err_class":"', '"err_class": "'))
# INV-6 applies here exactly as it does to @@STEP: metadata only, never the credential or the host.
check("outcome-line-has-no-secret", _BOXPW not in _oc and "10.0.0.9" not in _oc)


def main():
    if FAILS:
        print(f"\nFAIL: {len(FAILS)} check(s): {FAILS}")
        raise SystemExit(1)
    print("\nALL GREEN")


if __name__ == "__main__":
    main()
