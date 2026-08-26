"""Local fake-SFTP tests for bounded remote application-tree text search."""
import os
import shutil
import stat
import tempfile

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


class _SFTP:
    @staticmethod
    def _local(path):
        return os.path.join(root, path.lstrip("/").replace("/", os.sep))

    def listdir_attr(self, path):
        local = self._local(path)
        return [_Attrs(os.path.join(local, name), name) for name in os.listdir(local)]

    def lstat(self, path):
        return _Attrs(self._local(path), os.path.basename(path))

    def file(self, path, mode):
        return open(self._local(path), mode)  # noqa: PTH123 — local test double only

    def close(self):
        pass


class _Client:
    def open_sftp(self):
        return _SFTP()

    def close(self):
        pass


def _open(_conn):
    return _Client(), None


def _unexpected_open(_conn):
    raise AssertionError("invalid input must fail before SSH is opened")


root = tempfile.mkdtemp(prefix="sshops-remote-search-test-")
try:
    app_root = "/root/LiveTalking"
    local_app = _SFTP._local(app_root)
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
