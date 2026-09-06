"""Guest-worker semantics and bounded SSH-frame tests; no network or model calls."""
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import guest_search
import remote_search

FAILS = []


def check(name, condition):
    if not condition:
        FAILS.append(name)
        print("XX ", name)


class _Attrs:
    def __init__(self, path, filename):
        value = os.lstat(path)
        self.filename = filename
        self.st_mode = value.st_mode
        self.st_size = value.st_size


class _LocalFiles:
    @staticmethod
    def _local(path):
        return os.path.join(root, path.lstrip("/").replace("/", os.sep))

    def entries(self, path):
        local = self._local(path)
        return sorted((name, _Attrs(os.path.join(local, name), name)) for name in os.listdir(local))

    def lstat(self, path):
        return _Attrs(self._local(path), os.path.basename(path))

    def read(self, path, limit):
        with open(self._local(path), "rb") as handle:
            return handle.read(limit)


class _Channel:
    def __init__(self, filesystem=None, payload=None, exit_code=0, stalled=False):
        self.input = io.StringIO()
        self.output = b""
        self.filesystem = filesystem or _LocalFiles()
        self.payload = payload
        self.exit_code = exit_code
        self.stalled = stalled
        self.closed = False
        self.eof_received = not stalled

    def shutdown_write(self):
        request = json.loads(self.input.getvalue())
        self.output = (self.payload if self.payload is not None else
                       json.dumps(guest_search.run(request, self.filesystem), ensure_ascii=True).encode())

    def recv_ready(self):
        return bool(self.output) and not self.stalled

    def recv(self, count):
        data, self.output = self.output[:count], self.output[count:]
        return data

    def recv_stderr_ready(self):
        return False

    def exit_status_ready(self):
        return not self.stalled

    def recv_exit_status(self):
        return self.exit_code

    def close(self):
        self.closed = True


CLIENTS = []


class _Client:
    def __init__(self, channel=None):
        self.channel = channel or _Channel()
        self.commands = []
        self.closed = False
        CLIENTS.append(self)

    def open_sftp(self):
        raise AssertionError("tree traversal must execute in Guest, never use per-file SFTP")

    def exec_command(self, command, timeout):
        self.commands.append((command, timeout))
        return self.channel.input, SimpleNamespace(channel=self.channel), None

    def close(self):
        self.closed = True


def _open(_conn):
    return _Client(), None


def _unexpected_open(_conn):
    raise AssertionError("invalid input must fail before SSH is opened")


root = tempfile.mkdtemp(prefix="sshops-remote-search-test-")
try:
    app_root = "/root/LiveTalking"
    local_app = _LocalFiles._local(app_root)
    os.makedirs(os.path.join(local_app, "static", "js"), exist_ok=True)
    os.makedirs(os.path.join(local_app, ".ssh"), exist_ok=True)
    fake_key = "sk-test-" + ("m" * 24)
    with open(os.path.join(local_app, "static", "js", "client.js"), "w", encoding="utf-8") as handle:
        handle.write("const pc = new RTCPeerConnection({iceServers: []});\n")
        handle.write("const literal = 'a.*b';\n")
    with open(os.path.join(local_app, "app.py"), "w", encoding="utf-8") as handle:
        handle.write("# iceServers fallback\n")
        handle.write("api_key=" + fake_key + "\n")
    with open(os.path.join(local_app, ".env"), "w", encoding="utf-8") as handle:
        handle.write("iceServers=" + fake_key + "\n")
    with open(os.path.join(local_app, ".ssh", "config"), "w", encoding="utf-8") as handle:
        handle.write("iceServers secret\n")
    disabled_plugin = os.path.join(local_app, "custom_nodes", "ComfyUI-MiniMaxH3-Cache.disabled")
    os.makedirs(disabled_plugin, exist_ok=True)
    with open(os.path.join(disabled_plugin, "__init__.py"), "w", encoding="utf-8") as handle:
        handle.write("video_x, audio_x = x[0], x[1]\n")
    path_secret = "authorization-path-secret-012345"
    secret_named_file = os.path.join(local_app, "trace-" + path_secret + ".log")
    with open(secret_named_file, "w", encoding="utf-8") as handle:
        handle.write("authorization probe marker\n")

    result = remote_search.search({}, {
        "root": app_root, "query": "iceServers", "file_glob": "*.js",
    }, opener=_open)
    check("finds-literal-in-matching-basename-glob",
          result["ok"] is True and len(result["matches"]) == 1
          and result["matches"][0]["path"].endswith("/static/js/client.js")
          and result["matches"][0]["line_number"] == 1)
    check("result-carries-bounded-search-accounting",
          result["files_scanned"] == 1 and result["bytes_scanned"] > 0
          and result["truncated"] is False)
    check("one-ssh-exec-and-no-sftp-per-search",
          len(CLIENTS[-1].commands) == 1 and CLIENTS[-1].closed and CLIENTS[-1].channel.closed)
    check("search-source-uses-python-stdlib-not-preinstalled-rg",
          "exec python3 -I -B -c" in CLIENTS[-1].commands[0][0]
          and "exec python -I -B -c" in CLIENTS[-1].commands[0][0])

    all_source = remote_search.search({}, {
        "root": app_root, "query": "iceServers", "file_glob": "*.py",
    }, opener=_open)
    check("secret-paths-are-skipped-during-recursion",
          all_source["ok"] is True and len(all_source["matches"]) == 1
          and all("/.ssh/" not in item["path"] and not item["path"].endswith("/.env")
                  for item in all_source["matches"])
          and all_source["skipped"]["path_not_allowed"] >= 2)

    redacted = remote_search.search({}, {
        "root": app_root, "query": "api_key", "file_glob": "*.py",
    }, opener=_open)
    check("matching-lines-are-secret-scrubbed",
          redacted["ok"] is True and fake_key not in repr(redacted)
          and "REDACTED" in redacted["matches"][0]["line"])

    secret_path_search = remote_search.search({}, {
        "root": app_root, "query": "authorization probe marker", "file_glob": "*.log",
    }, secrets=(path_secret,), opener=_open)
    check("search-result-paths-are-secret-scrubbed",
          secret_path_search["ok"] is True
          and path_secret not in repr(secret_path_search)
          and "REDACTED" in secret_path_search["matches"][0]["path"])

    literal = remote_search.search({}, {
        "root": app_root, "query": "a.*b", "file_glob": "*.js",
    }, opener=_open)
    check("query-is-literal-not-regex",
          literal["ok"] is True and len(literal["matches"]) == 1)

    insensitive = remote_search.search({}, {
        "root": app_root, "query": "rtcpeerconnection", "file_glob": "*.js",
        "ignore_case": True, "max_matches": 1,
    }, opener=_open)
    check("casefold-and-match-limit-are-explicit",
          insensitive["ok"] is True and len(insensitive["matches"]) == 1
          and insensitive["truncated"] is True)

    check("broad-and-secret-roots-are-refused",
          all(remote_search._validated_args({"root": candidate, "query": "x"})[1]
              ["error_class"] == "root_not_allowed"
              for candidate in ("/", "/root", "/home", "/etc", "/root/.ssh")))
    denied = remote_search.search({}, {"root": "/root", "query": "x"}, opener=_unexpected_open)
    check("invalid-root-fails-before-connect", denied["error_class"] == "root_not_allowed")
    check("glob-cannot-select-a-descendant-path",
          remote_search._validated_args({"root": app_root, "query": "x",
                                         "file_glob": "../*.env"})[1]
          ["error_class"] == "invalid_file_glob")

    found = remote_search.find_paths({}, {
        "root": app_root, "name_glob": "*minimaxh3-cache*", "ignore_case": True,
        "max_depth": 4,
    }, opener=_open)
    check("find-paths-discovers-disabled-directory-without-reading-content",
          found["ok"] is True and found["matches"] == [{
              "path": app_root + "/custom_nodes/ComfyUI-MiniMaxH3-Cache.disabled",
              "type": "directory", "depth": 2,
          }])
    check("find-paths-skips-credential-directories",
          all("/.ssh" not in item["path"] for item in found["matches"])
          and found["skipped"]["path_not_allowed"] >= 2)
    secret_path_find = remote_search.find_paths({}, {
        "root": app_root, "name_glob": "trace-*", "max_depth": 1,
    }, secrets=(path_secret,), opener=_open)
    check("find-result-paths-are-secret-scrubbed",
          secret_path_find["ok"] is True
          and path_secret not in repr(secret_path_find)
          and "REDACTED" in secret_path_find["matches"][0]["path"])
    depth_limited = remote_search.find_paths({}, {
        "root": app_root, "name_glob": "*.py", "max_depth": 1,
    }, opener=_open)
    check("find-paths-respects-max-depth",
          depth_limited["ok"] is True
          and [item["path"] for item in depth_limited["matches"]] == [app_root + "/app.py"])
    check("find-paths-validates-before-connect",
          remote_search.find_paths({}, {"root": "/root", "name_glob": "*"},
                                  opener=_unexpected_open)["error_class"] == "root_not_allowed")

    whole_line_secret = "bound-secret-" + "s" * 180
    with open(os.path.join(local_app, "large.log"), "w", encoding="utf-8") as handle:
        for index in range(40):
            handle.write("large-marker " + str(index) + " " + "x" * 800 + "\n")
        handle.write("boundary-marker " + "z" * 990 + whole_line_secret + "\n")
        handle.write("casefold-marker Straße\n")
    large = remote_search.search({}, {"root": app_root, "query": "large-marker",
                                     "file_glob": "large.log"}, opener=_open)
    check("valid-json-over-shell-16k-limit-is-not-clipped",
          large["ok"] and len(large["matches"]) == 40 and len(json.dumps(large)) > 16000
          and large["matches"][-1]["line"].startswith("large-marker 39 "))
    boundary = remote_search.search({}, {"root": app_root, "query": "boundary-marker",
                                        "file_glob": "large.log"},
                                    secrets=(whole_line_secret,), opener=_open)
    check("redaction-precedes-line-clipping-on-the-complete-match",
          boundary["ok"] and whole_line_secret[:20] not in repr(boundary)
          and len(boundary["matches"][0]["line"].encode()) <= 1024)
    folded = remote_search.search({}, {"root": app_root, "query": "STRASSE", "ignore_case": True,
                                      "file_glob": "large.log"}, opener=_open)
    check("unicode-casefold-semantics-preserved", folded["ok"] and len(folded["matches"]) == 1)
    with open(os.path.join(local_app, "binary.dat"), "wb") as handle:
        handle.write(b"\xff\xfe\x80")
    binary = remote_search.search({}, {"root": app_root, "query": "marker", "file_glob": "binary.dat"},
                                  opener=_open)
    check("invalid-utf8-is-skipped-not-a-search-failure",
          binary["ok"] and binary["matches"] == [] and binary["skipped"]["not_utf8"] == 1)
    with patch.object(remote_search, "_MAX_DIRECTORIES", 1):
        bounded = remote_search.search({}, {"root": app_root, "query": "absent"}, opener=_open)
        bounded_find = remote_search.find_paths({}, {"root": app_root, "name_glob": "absent"}, opener=_open)
    check("both-tree-tools-keep-directory-bounds",
          bounded["truncated"] and bounded["directories_scanned"] == 1
          and bounded_find["truncated"] and bounded_find["directories_scanned"] == 1)
    with patch.object(remote_search, "_MAX_FILES", 1):
        bounded = remote_search.search({}, {"root": app_root, "query": "absent"}, opener=_open)
    check("content-search-keeps-file-count-bound", bounded["truncated"] and bounded["files_seen"] == 1)

    policy = remote_search._worker_request("search", {"root": app_root})["policy"]
    candidates = [app_root, "/", "/root/.env", "/root/.ENV.local", "/root/x.pem",
                  "/root/a/../b", "/root/a\x00b", "/root/" + "x" * 513, "/root/custom_nodes/x.py"]
    candidates += list(remote_search.remote_text._DENIED_EXACT)
    candidates += [prefix + "child" for prefix in remote_search.remote_text._DENIED_PREFIXES]
    candidates += [app_root + "/" + name + "/child" for name in remote_search.remote_text._DENIED_COMPONENTS]
    candidates += [app_root + "/" + name for name in remote_search.remote_text._DENIED_BASENAMES]
    check("worker-descendant-policy-matches-shared-remote-text-policy",
          all(guest_search.path_allowed(path, policy) == remote_search.remote_text._valid_path(path)
              for path in candidates))

    missing = remote_search.search({}, {"root": app_root + "/missing", "query": "x"}, opener=_open)
    not_directory = remote_search.find_paths({}, {"root": app_root + "/app.py", "name_glob": "*"}, opener=_open)
    check("observed-missing-and-wrong-root-types-remain-distinct",
          missing["error_class"] == "root_not_found"
          and not_directory["error_class"] == "root_not_directory")
    class _DeniedFiles(_LocalFiles):
        def entries(self, path):
            raise PermissionError("not exposed to the model")
    denied = remote_search.search({}, {"root": app_root, "query": "x"},
                                  opener=lambda conn: (_Client(_Channel(filesystem=_DeniedFiles())), None))
    check("guest-permission-error-is-not-empty-success", denied["error_class"] == "permission_denied")
    connection = remote_search.search({}, {"root": app_root, "query": "x"},
                                      opener=lambda conn: (None, {"error": "connect_failed", "detail": "TimeoutError"}))
    check("ssh-error-retains-concrete-class", connection["error_class"] == "connect_failed"
          and connection["detail"] == "TimeoutError")
    for payload, exit_code, expected in [(b"not JSON", 0, "search_failed"),
                                          (b"{}", 127, "search_failed"),
                                          (b"", 124, "exec_timeout")]:
        failed = remote_search.search({}, {"root": app_root, "query": "x"},
                                      opener=lambda conn: (_Client(_Channel(payload=payload, exit_code=exit_code)), None))
        check("worker-error-%s-is-not-a-negative-finding" % exit_code,
              not failed["ok"] and failed["error_class"] == expected
              and CLIENTS[-1].closed and CLIENTS[-1].channel.closed)
    with patch.object(remote_search, "_MAX_WIRE_BYTES", 100):
        oversize = remote_search.search({}, {"root": app_root, "query": "x"},
                                        opener=lambda conn: (_Client(_Channel(payload=b"x" * 101)), None))
    check("structured-output-overflow-fails-without-returning-broken-json",
          not oversize["ok"] and oversize["error_class"] == "search_failed")

    class _StatusBeforeEOF(_Channel):
        def __init__(self):
            super().__init__()
            self.output = b'{"ok":true,'
            self.eof_received = False

        def deliver_tail(self, _delay):
            self.output = b'"matches":[]}'
            self.eof_received = True

    delayed = _StatusBeforeEOF()
    with patch.object(remote_search.time, "sleep", side_effect=delayed.deliver_tail):
        payload, exit_code = remote_search._receive(delayed)
    check("exit-status-before-eof-preserves-complete-structured-frame",
          exit_code == 0 and json.loads(payload) == {"ok": True, "matches": []})

    class _BusyChannel(_Channel):
        def recv_ready(self):
            return True

        def recv(self, count):
            return b"x"

    with patch.object(remote_search.time, "monotonic", side_effect=[0, 0, 31]):
        timed_out = remote_search.search({}, {"root": app_root, "query": "x"},
                                         opener=lambda conn: (_Client(_BusyChannel()), None))
    check("continuous-search-output-still-checks-deadline-between-batches",
          timed_out["error_class"] == "exec_timeout" and CLIENTS[-1].channel.closed)
    with patch.object(remote_search.time, "monotonic", side_effect=[0, 31]):
        timed_out = remote_search.search({}, {"root": app_root, "query": "x"},
                                         opener=lambda conn: (_Client(_Channel(stalled=True)), None))
    check("stalled-search-has-time-bound-and-closes-channel", timed_out["error_class"] == "exec_timeout"
          and CLIENTS[-1].closed and CLIENTS[-1].channel.closed)

    # Exercise the exact standalone worker's stdin/stdout entrypoint, not only imported functions.
    request = remote_search._worker_request("search", {"root": "/sshops-nonexistent-worker-probe",
        "query": "x", "ignore_case": False, "file_glob": "*", "max_matches": 1})
    worker = subprocess.run([sys.executable, "-I", "-B", "-c",
        Path(guest_search.__file__).read_text(encoding="utf-8")],
        input=json.dumps(request), capture_output=True, text=True, timeout=10)
    check("standalone-worker-uses-stdin-without-project-imports",
          worker.returncode == 0 and json.loads(worker.stdout)["error_class"] == "root_not_found")
    if os.name == "posix":
        spec, error = remote_search._validated_args({"root": local_app, "query": "a.*b",
                                                   "file_glob": "*.js"})
        check("native-posix-fixture-root-is-valid", error is None)
        native = subprocess.run(["bash", "-c", remote_search.ssh_transport._bounded(
            remote_search._worker_command())],
            input=json.dumps(remote_search._worker_request("search", spec)),
            capture_output=True, text=True, timeout=35)
        native_result = json.loads(native.stdout) if native.returncode == 0 else {}
        check("exact-ssh-command-runs-real-local-filesystem-in-guest-process",
              native.returncode == 0 and native_result.get("ok")
              and len(native_result["matches"]) == 1
              and native_result["matches"][0]["line_number"] == 2)

    link_path = os.path.join(local_app, "linked")
    try:
        os.symlink(os.path.join(local_app, "static"), link_path, target_is_directory=True)
    except (OSError, NotImplementedError):
        print("-- symlink-tree-refused: SKIPPED (host does not permit symlink creation)")
    else:
        linked = remote_search.search({}, {
            "root": app_root, "query": "iceServers", "file_glob": "*.js",
        }, opener=_open)
        check("directory-symlinks-are-not-followed",
              linked["ok"] is True and len(linked["matches"]) == 1
              and linked["skipped"]["symlink"] == 1)
        symlink_root = remote_search.search({}, {
            "root": app_root + "/linked", "query": "iceServers", "file_glob": "*.js",
        }, opener=_open)
        check("root-symlink-is-refused-before-traversal",
              symlink_root["ok"] is False
              and symlink_root["error_class"] == "root_symlink_refused")
        symlink_find = remote_search.find_paths({}, {
            "root": app_root + "/linked", "name_glob": "*.js",
        }, opener=_open)
        check("find-paths-also-refuses-root-symlink",
              symlink_find["ok"] is False
              and symlink_find["error_class"] == "root_symlink_refused")
finally:
    shutil.rmtree(root, ignore_errors=True)

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("remote_search: ALL GREEN")
