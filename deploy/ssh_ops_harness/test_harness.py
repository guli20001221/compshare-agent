"""Offline gate for the harness wrapper's SDK-independent logic (no network / no SSH / no SDK).

Run:  python test_harness.py   ->  exits non-zero on ANY failure.

Asserts the boundary contract: stdin handshake (never env), Phase-1 read-only enforcement, the
credential never reaching the audit/output, and INV-9 (only ssh_exec exposed). The transport is
monkeypatched so nothing actually connects.
"""
import os
import sys
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

ssh_transport.run_ssh = lambda c, command, secrets=(): {"error": "auth_failed"}
_pf_auth = harness.preflight_probe(harness._CONN)
check("preflight-auth-reason", isinstance(_pf_auth, str) and "认证失败" in _pf_auth)
check("preflight-no-credential", "Pl4inPwd77x" not in (_pf_conn + _pf_auth))


# --- the REAL ssh_transport.run_ssh genuinely scrubs box output (fake paramiko -> no connection) ---
_PW = "Pl4in" + "Pwd77x"
_AWS = "wJalr" + "XUtnFE" + "MIK7MD" + "ENGbPx"


def _make_fake_paramiko():
    m = types.ModuleType("paramiko")

    class _Chan:
        def recv_exit_status(self):
            return 0

    class _Stream:
        def __init__(self, data):
            self._d = data
            self.channel = _Chan()

        def read(self):
            return self._d

    class _Client:
        def set_missing_host_key_policy(self, *a):
            pass

        def connect(self, **k):
            pass

        def exec_command(self, command, timeout=None):
            out = f"role pw {_PW} and AWS_SECRET_ACCESS_KEY={_AWS}".encode()
            return (None, _Stream(out), _Stream(b""))

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
check("transport-keeps-benign", "role pw" in _res["stdout"])


# --- INV-9: only ssh_exec exposed; built-ins stripped; settings isolated. Fail CLOSED otherwise. ---
def opts(allowed, disallowed, sources):
    return types.SimpleNamespace(allowed_tools=allowed, disallowed_tools=disallowed, setting_sources=sources)


good = opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, [])
try:
    harness.assert_single_tool(good)
    check("inv9-accepts-good", True)
except SystemExit:
    check("inv9-accepts-good", False)

for name, bad in [
    ("inv9-rejects-extra-allowed", opts(harness.ALLOWED_TOOLS + ["Bash"], harness.DISALLOWED_TOOLS, [])),
    ("inv9-rejects-missing-disallowed", opts(harness.ALLOWED_TOOLS, ["Read"], [])),
    ("inv9-rejects-nonempty-sources", opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, ["user"])),
    ("inv9-rejects-empty-allowed", opts([], harness.DISALLOWED_TOOLS, [])),
]:
    try:
        harness.assert_single_tool(bad)
        check(name, False)
    except SystemExit:
        check(name, True)


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
check("proto-step-fields-present",
      all(set(o) == {"command", "tier", "disposition", "exit", "bytes"} for o in _pobjs))

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


def main():
    if FAILS:
        print(f"\nFAIL: {len(FAILS)} check(s): {FAILS}")
        raise SystemExit(1)
    print("\nALL GREEN")


if __name__ == "__main__":
    main()
