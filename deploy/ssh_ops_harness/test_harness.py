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


# --- api-read mode: handshake (credential-free), the client-side allowlist gate, the HTTP client
# plumbing (against a mock loopback proxy), and INV-9 for the api tool. ---
api_hs = harness.read_handshake(
    '{"mode":"api","api_url":"http://127.0.0.1:9/x","api_token":"tok","api_actions":["DescribeBilling"]}')
check("api-handshake-parses", api_hs.get("mode") == "api" and api_hs["api_url"].endswith("/x"))
check("api-handshake-no-ssh-required", "host" not in api_hs and "password" not in api_hs)

for bad in ['{"mode":"api","api_token":"t"}',              # missing api_url
            '{"mode":"api","api_url":"http://x"}']:         # missing api_token
    try:
        harness.read_handshake(bad)
        check(f"api-handshake-rejects::{bad[:26]}", False)
    except Exception:
        check(f"api-handshake-rejects::{bad[:26]}", True)

# client-side allowlist: a non-allowed action is refused WITHOUT any HTTP call, recorded in AUDIT.
harness.AUDIT.clear()
harness.set_api({"url": "http://127.0.0.1:9/api_read", "token": "tok", "actions": ["DescribeBilling"]})
r_deny = harness.api_read_call("TerminateCompShareInstance", {})
check("api-allowlist-refuses", r_deny["is_error"] and "not allowed" in r_deny["text"].lower())
check("api-allowlist-audit",
      harness.AUDIT[-1]["disposition"] == "refused_action_not_allowed" and harness.AUDIT[-1]["executed"] is False)

# happy path + bad-token path against a REAL mock loopback proxy (exercises the urllib client).
import http.server
import json as _json
import socketserver
import threading


class _MockProxy(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        if self.headers.get("Authorization") != "Bearer tok":
            self.send_response(401)
            self.end_headers()
            return
        length = int(self.headers.get("Content-Length", 0))
        req = _json.loads(self.rfile.read(length) or b"{}")
        resp = _json.dumps({"echo_action": req.get("action"), "Password": "[已设置]"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)

    def log_message(self, *a):        # silence the default stderr access log
        pass


_srv = socketserver.TCPServer(("127.0.0.1", 0), _MockProxy)
_port = _srv.server_address[1]
threading.Thread(target=_srv.serve_forever, daemon=True).start()

harness.AUDIT.clear()
harness.set_api({"url": f"http://127.0.0.1:{_port}/api_read", "token": "tok", "actions": ["DescribeBilling"]})
r_ok = harness.api_read_call("DescribeBilling", {"Range": "7d"})
check("api-read-ok", (not r_ok["is_error"]) and "DescribeBilling" in r_ok["text"])
check("api-read-audit-ran",
      harness.AUDIT[-1]["disposition"] == "ran_api_read" and harness.AUDIT[-1]["executed"] is True)

harness.set_api({"url": f"http://127.0.0.1:{_port}/api_read", "token": "WRONG", "actions": ["DescribeBilling"]})
r_401 = harness.api_read_call("DescribeBilling", {})
check("api-read-bad-token-errors", r_401["is_error"] and "401" in r_401["text"])
_srv.shutdown()

# INV-9 for api mode: EXACTLY api_read exposed; the ssh tool set must be rejected in api mode.
good_api = opts(harness.API_ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, [])
try:
    harness.assert_single_tool(good_api, harness.API_ALLOWED_TOOLS)
    check("inv9-api-accepts-good", True)
except SystemExit:
    check("inv9-api-accepts-good", False)

try:
    harness.assert_single_tool(opts(harness.ALLOWED_TOOLS, harness.DISALLOWED_TOOLS, []), harness.API_ALLOWED_TOOLS)
    check("inv9-api-rejects-ssh-tool", False)
except SystemExit:
    check("inv9-api-rejects-ssh-tool", True)


def main():
    if FAILS:
        print(f"\nFAIL: {len(FAILS)} check(s): {FAILS}")
        raise SystemExit(1)
    print("\nALL GREEN")


if __name__ == "__main__":
    main()
