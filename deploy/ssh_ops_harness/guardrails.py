"""Reasoning-blind command guardrails for the SSH-ops lane (production-hardened, round 2).

Core safety principle: the decision to run / refuse / confirm a command is driven ONLY
by the user intent + the literal command string, NEVER by anything the instance emitted.
Output read off the box is untrusted data, not instructions — so classification happens
here, before execution, on the command text alone. This is the XPIA / prompt-injection
firewall.

Three tiers (mirror internal/tools/safe_executor.go semantics):
  - read_only  : auto-run, no human prompt        (curated, one-shot diagnostics)
  - mutating   : requires explicit human confirm  (deny-first: unknown => needs confirm)
  - destructive: hard-refused, even with confirm  (checked FIRST, unconditional)

Two adversarial red-team rounds shaped this. Round 1 closed exfil/streaming/destructive holes.
Round 2's lesson: the hardening must NOT destroy the lane's OWN diagnostics — SSH auth logs,
checksums (sha256sum), git/docker IDs, and `KeyError:` tracebacks were being over-redacted, and
`ps -o environ` still leaked env. So redaction is now PRECISION-first (specific secret labels +
value-shape + vendor prefixes), classification anchors flags to the right binary, and the
transport's hard per-command timeout backstops any streaming command that still slips through.

Read tier is deny-by-default: anything not positively matching the allowlist is >= mutating.
"""
import re
from typing import Iterable

# A lone `binary --help/--version` mutates nothing — classify as read_only before anything else.
# Only the UNAMBIGUOUS long forms: `-h`/`-V` are excluded because on power binaries `-h`==--halt
# (shutdown/reboot/poweroff -h powers the box off), and _HELP runs before the destructive scan.
_HELP = re.compile(r"^[\w./-]+\s+(--help|--version|help|version)\s*$")

# ===========================================================================
# Tier 1 (checked FIRST): destructive — always hard-refused, case-insensitive.
# Deny-by-EFFECT, but anchored so flags/paths that merely SPELL a destructive verb
# (e.g. `iostat -dd`) don't trip it.
# ===========================================================================
_DESTRUCTIVE_SRC = [
    # filesystem / device / volume wipe
    r"\brm\b", r"\brmdir\b", r"\bunlink\b", r"\bshred\b", r"\btruncate\b",
    r"\bmkfs\w*\b", r"(?<![\w-])dd\b[^\n]*\s(if=|of=|bs=|count=|conv=)",
    r"\bfdisk\b", r"\bparted\b", r"\bwipefs\b", r"\bblkdiscard\b", r"\bsgdisk\b\s+(-Z|--zap)",
    r"\bfind\b[^\n]*\s-delete\b",
    r"\bfind\b[^\n]*-exec\s+\S*\b(rm|mv|cp|tee|dd|chmod|chown|truncate|shred|chattr|mkfs|sh|bash|unlink|kill)\b",
    r"\b(lvremove|vgremove|pvremove|lvreduce|vgreduce)\b",
    r"\bzpool\b\s+destroy\b", r"\bzfs\b\s+destroy\b", r"\bbtrfs\b[^\n]*\bdelete\b",
    # power / boot
    r"\bshutdown\b", r"\breboot\b", r"\bhalt\b", r"\bpoweroff\b", r"\binit\s+[06]\b",
    # accounts / auth (a real target arg; `--help` is intercepted above)
    r"\buserdel\b", r"\bgroupdel\b", r"\bchpasswd\b", r"\busermod\b",
    # passwd as a COMMAND (start / after sudo / after a separator), path-qualified or not — but NOT
    # the /etc/passwd data path (cat/stat/df /etc/passwd) nor `getent passwd`.
    r"(?:^|\bsudo\s+|[;&|]\s*)(?:/\S+/)?passwd\b",
    # device / critical-path writes
    r">\s*/dev/[sn]d", r"\bof=/dev/",
    r">\s*/(etc|boot|var/lib|usr|s?bin|lib(64)?|sys|proc)\b",
    r"\b(cp|mv|tee|install)\b[^\n]*\s/(boot|etc|var/lib|usr|s?bin|lib(64)?)/",
    r"\bsed\b[^\n]*-i\w*[^\n]*\s/(etc|boot|usr)/",
    # perms / immutability / lockout
    r"\bchmod\b.*\s-R\b", r"\bchown\b.*\s-R\b", r"\bchmod\b.*\b777\b",
    r"\bchattr\b[^\n]*\s\+[iae]\b",
    # firewall / services / management-channel lockout
    r"\biptables\b\s+-F", r"\bufw\b\s+disable",
    r"\bsystemctl\b\s+(disable|mask)\b",
    r"\bsystemctl\b\s+(stop|kill)\s+\S*(ssh|network)",
    # process kill of init / critical daemons
    r"\bkill\b\s+(-\w+\s+)*-?1(\s|$)",
    r"\b(pkill|killall)\b[^\n]*\b(sshd|systemd|init|dockerd|containerd)\b",
    # orchestrator / container deletes
    r"\bkubectl\b[^\n]*\bdelete\b",
    r"\bdocker\b[^\n]*\b(system\s+prune|volume\s+rm|rmi|image\s+prune|container\s+prune)\b",
    r"\bhelm\b[^\n]*\b(uninstall|delete)\b",
    # availability
    r"\bswapoff\b",
    # fork bomb / cron
    r":\s*\(\s*\)\s*\{", r"\bcrontab\b\s+-r",
    # credential-bearing commands: never run a command that inlines a secret
    r"\bsshpass\b\s+-p",
    r"\bexport\b\s+\w*(KEY|TOKEN|SECRET|PASSWORD|PASSWD|PWD|DSN|AUTH)\w*\s*=",
]
_DESTRUCTIVE = [re.compile(p, re.IGNORECASE) for p in _DESTRUCTIVE_SRC]

# Auto-run disqualifiers: shell control/expansion/redirection/glob/quote/parent-dir/newline.
_DANGEROUS_META = re.compile(r"""[;|&`$(){}\[\]<>*?~'"]|\.\.|\n""")

# --- Safe read-only pipeline support (round 3) --------------------------------
# The blanket _DANGEROUS_META ban refuses EVERY piped/globbed diagnostic
# (`lsmod | grep nvidia`, `cat /proc/driver/nvidia/gpus/*/information`) — which is
# most of what a real GPU/掉卡 diagnosis needs, so the agent burns its whole turn
# budget on refused workarounds. We carve out a NARROW, structurally-validated
# exception: a pipeline whose SOURCE is an allowlisted read-only command and whose
# every downstream stage is a stdin-only text filter. Deny-by-default is preserved —
# anything not matching this exact shape still falls through to `mutating`.
#
# stderr/void redirects that write no real file (stripped before the meta scan so
# `nvidia-smi 2>&1 | grep` and `df -h / 2>/dev/null` are not rejected):
_SAFE_REDIR = re.compile(r"(?:\d*>&\d+|&>\s*/dev/null|\d*>\s*/dev/null)")
# Hard-dangerous shell constructs. Deliberately EXCLUDES `|` (pipe), `*`/`?` (glob), and the
# SINGLE quote — those are validated structurally below. Everything that enables command chaining
# (`;` `&&`), substitution (`` ` `` `$()` `$VAR`), real-file redirection (`>`), brace/bracket/tilde
# expansion, DOUBLE quotes, parent-dir traversal, or newlines is banned. Single quotes are allowed
# (F6): they are shell-LITERAL, so a `grep '8188'` pattern reaches the filter as inert text and can
# never be executed; DOUBLE quotes stay banned because inside them $()/`` ` ``/$VAR still expand.
_HARD_META = re.compile(r"""[;&`$<>(){}\[\]~"]|\.\.|\n""")
# Pure stdin->stdout text filters (no file writes, no exec, no file-arg reads).
# Deliberately EXCLUDES awk/sed (system()/-i/w-file), xargs/tee (exec/write), dd.
_SAFE_FILTERS = {"grep", "egrep", "fgrep", "head", "tail", "wc", "sort",
                 "uniq", "cut", "tr", "nl", "column", "rev", "cat", "tac"}
# Follow/stream flags — scoped to the binaries where -f means "stream" (NOT ps -f / lsblk -f).
_FOLLOW = re.compile(r"(?:^|\s)(-f|-F|--follow)(?:\s|$)")
# Per-binary continuous flags that loop forever (cluster-aware, e.g. `free -hs1`, `netstat -tlnpc`).
_STREAM_BLOCK = {
    "free": re.compile(r"(?:^|\s)(-[a-z]*s|--seconds)"),
    "netstat": re.compile(r"(?:^|\s)(-[a-z]*c|--continuous)"),
}

# nvidia-smi: query/read flags ONLY. NOT -r/-pl/-pm/-e/-l (mutate/stream), NOT dmon/pmon.
_NVIDIA = re.compile(
    r"nvidia-smi(\s+(-q|-L|-a|-i\s+\d+|-d\s+\w+|--id=\d+|--query[\w.-]*=\S+|--format=\S+|--display=\S+|topo(\s+-m)?))*"
)

# Simple flag-only-safe diagnostics (no file CONTENT, no stream/mutate args). fullmatch.
_STRUCTURED_DIAG = [re.compile(p) for p in [
    r"systemctl\s+(status|is-active|is-enabled|is-failed|list-units|list-unit-files|list-dependencies)(\s+\S+)*",
    r"(free|uptime|uname|hostname|whoami|id|pwd|date|lscpu|lsblk|lsmod|lspci|nproc|arch|sensors)(\s+\S+)*",
    r"df(\s+\S+)*",
    r"dmesg(\s+(-T|-x|-t|-H|-k|-r|-e|--ctime|--color=\w+|-l\s+\w+|-n\s+\d+|--level=\S+))*",
    r"(ss|netstat)(\s+\S+)*",
    r"ip\s+(-\w+\s+)*(addr|a|link|l|route|r|neigh|n)(\s+(show|list|s|l))?",
    r"getconf(\s+\S+)*",
    r"(nvcc|python3?|pip3?|conda|docker|git|gcc|g\+\+|cmake|go|java|node|npm|ruff|jupyter)\s+(--version|-V|version)",
    r"pip3?\s+(list|show|freeze)(\s+\S+)*",
    r"conda\s+(list|info|env\s+list)(\s+\S+)*",
    r"docker\s+(ps|images|info|version|stats\s+--no-stream)(\s+\S+)*",
    # package / shared-lib inventory — read-only, central to driver/CUDA diagnosis.
    # `ldconfig` with NO flag rebuilds the cache (mutating), so require -p/--print-cache.
    r"dpkg\s+(-l|--list|-s|--status|-L|--listfiles)(\s+\S+)*",
    r"ldconfig\s+(-p|--print-cache)",
    # binary location / kind — read-only lookups: print a path/type or "not found", never content,
    # never exec (unlike `ldd`, which runs the loader on the target and is left out on purpose).
    r"(which|whereis|type)\s+\S+(\s+\S+)*",
    r"command\s+-[vV]\s+\S+",
]]

# Readers that emit raw file CONTENT — strict: every target must be a curated-safe absolute path.
_CONTENT_READERS = {"cat", "nl", "tac", "strings", "od", "xxd", "hexdump"}
# Readers that emit only metadata — lenient: bare names/flags/cwd ok, sensitive absolute path denies.
# `du` is deliberately NOT here — it emits ONLY a size (not even a filename), so it gets its own,
# broader user-dir allowlist below (F5).
_META_READERS = {"ls", "stat", "file", "readlink", "wc", "md5sum", "sha256sum"}

# The auto-run CONTENT file surface (readers that emit raw bytes). /var/log/ is deliberately NOT here
# (cloud-init/auth logs leak). Kept narrow on purpose — a broadened content surface is a leak vector.
_SAFE_READ_PREFIXES = (
    "/proc/driver/nvidia/",
    # kernel module / PCI / DRM / device state — content is hardware+driver info, no user secrets.
    "/sys/module/", "/sys/bus/pci/", "/sys/class/drm/", "/sys/devices/",
)
_SAFE_READ_EXACT = {
    "/etc/os-release", "/etc/lsb-release", "/etc/hostname", "/etc/machine-id",
    "/etc/timezone", "/etc/issue",
    "/proc/meminfo", "/proc/cpuinfo", "/proc/loadavg", "/proc/uptime",
    "/proc/version", "/proc/stat", "/proc/diskstats", "/proc/mounts", "/proc/swaps",
    "/proc/modules", "/proc/devices", "/proc/cmdline",
    # socket tables — the ss/netstat fallback for port-state diagnosis when neither tool is
    # installed (a minimal container). Hex addr/port/state/uid/inode; no user file content, no secret.
    "/proc/net/tcp", "/proc/net/tcp6", "/proc/net/udp", "/proc/net/udp6",
    "/usr/local/cuda/version.txt", "/usr/local/cuda/version.json",
}
# Per-process files that carry NO secret — for container/cgroup detection. environ and cmdline are
# deliberately EXCLUDED (they leak the environment and another process's argv, which can inline a
# password); both are also caught by _DENY_PATH_SUBSTR / not listed here.
_PROC_PID_SAFE = re.compile(
    r"^/proc/(?:\d+|self|thread-self)/(?:cgroup|status|mountinfo|limits|comm|maps)$")
_DENY_PATH_SUBSTR = (
    "environ", "id_rsa", "id_ed25519", "id_dsa", "id_ecdsa", "authorized_keys",
    ".ssh", ".aws", ".gnupg", ".kube", ".docker", "shadow", "sudoers",
    ".pem", ".key", "credential", ".bash_history", ".netrc", "/root",
    "/etc/ssl/private", "/etc/kubernetes", "/var/run/secrets", ".env",
    "private", ".git-credentials", ".npmrc", ".pypirc",
)


def _basename(tok: str) -> str:
    return tok.rsplit("/", 1)[-1]


# Metadata-only reads (ls/stat/file/readlink/wc/*sum; `du` is broader, see F5 below) reveal a
# name/size/perms/hash but never file CONTENT, so they are safe across the introspection tree —
# binaries, libs, device nodes, kernel / hardware state — which is far broader than the content-read
# allowlist. Deliberately EXCLUDES /etc, /var, /home, /root (a filename there can itself be
# sensitive; content reads stay narrow).
_META_SAFE_PREFIXES = ("/dev/", "/usr/", "/sys/", "/proc/", "/lib/", "/lib64/",
                       "/bin/", "/sbin/", "/opt/")


def _safe_path(p: str) -> bool:
    if ".." in p:
        return False
    low = p.lower()
    if any(d in low for d in _DENY_PATH_SUBSTR):
        return False
    if p in _SAFE_READ_EXACT:
        return True
    if _PROC_PID_SAFE.match(p):
        return True
    return any(p == pre.rstrip("/") or p.startswith(pre) for pre in _SAFE_READ_PREFIXES)


def _safe_meta_path(p: str) -> bool:
    """Broader path check for metadata-only reads. A content-safe path is trivially meta-safe; beyond
    that, allow the introspection tree (still minus any _DENY_PATH_SUBSTR secret location)."""
    if _safe_path(p):
        return True
    if ".." in p:
        return False
    if any(d in p.lower() for d in _DENY_PATH_SUBSTR):
        return False
    return any(p == pre.rstrip("/") or p.startswith(pre) for pre in _META_SAFE_PREFIXES)


def _safe_content_read(tokens) -> bool:
    seen_path = False
    for t in tokens[1:]:
        if t.startswith("-") or t.isdigit():
            continue
        if "/" not in t:
            return False
        seen_path = True
        if not _safe_path(t):
            return False
    return seen_path


def _safe_meta_read(tokens) -> bool:
    for t in tokens[1:]:
        if t.startswith("-") or t.isdigit():
            continue
        if "/" in t and not _safe_meta_path(t):
            return False
    return True


# --- F5: `du` size-only reads on user-data dirs -------------------------------
# `du` emits ONLY an aggregate byte size — never a filename, never file content — so it is strictly
# safer than the ls/stat metadata reads F4 already allows, and safe even on the user-data dirs
# (/root, /home, /workspace, ...) that ls/stat are denied. Disk-full diagnosis fundamentally needs
# to enumerate where space went, and a user's big files live in exactly those dirs (a live repro
# proved the harness could confirm "disk full" via df yet not point at the 47G culprit in /root).
# The secret-FILE tripwire substrings (.ssh, id_rsa, shadow, .env, ...) still deny even a size read
# (defense in depth); only the whole-dir `/root` block is lifted for `du`.
_DU_USER_PREFIXES = ("/root", "/home", "/workspace", "/data", "/mnt")
_DU_DENY_SUBSTR = tuple(d for d in _DENY_PATH_SUBSTR if d != "/root")


def _du_safe_path(p: str) -> bool:
    if ".." in p:
        return False
    low = p.lower()
    if any(d in low for d in _DU_DENY_SUBSTR):
        return False
    if _safe_meta_path(p):                               # F4 introspection tree: a size read is fine
        return True
    return any(p == pre or p.startswith(pre + "/") for pre in _DU_USER_PREFIXES)


def _du_read_ok(tokens) -> bool:
    for t in tokens[1:]:
        if t.startswith("-") or t.isdigit():
            continue
        if ".." in t:
            return False
        if "/" in t and not _du_safe_path(t):
            return False
    return True


def _ps_ok(tokens) -> bool:
    """ps BSD `e` personality / `-o environ` column dumps every process's ENVIRONMENT."""
    if "environ" in " ".join(tokens).lower():            # -o environ / -oenviron / -o=environ (any gluing)
        return False
    i = 1
    while i < len(tokens):
        t = tokens[i]
        if t in ("-o", "o", "--format", "-eo", "--sort"):
            val = tokens[i + 1] if i + 1 < len(tokens) else ""
            if t in ("-o", "o", "--format", "-eo"):
                if any(f.strip().lower() in ("environ", "e", "env")
                       for f in re.split(r"[,\s]+", val)):
                    return False
            i += 2
            continue
        if t in ("-p", "--pid", "-u", "-U", "-C", "-G"):
            i += 2
            continue
        if not t.startswith("-") and "e" in t.lower():
            return False
        i += 1
    return True


_JOURNAL_MUTATE = re.compile(
    r"--(vacuum-(time|size|files)|rotate|flush|sync|relinquish-var|setup-keys|update-catalog|verify)"
)
_JOURNAL_BOUND = re.compile(
    r"(?:(?:^|\s)-n\s*\d+|--lines[=\s]\s*\d+|--since|(?:^|\s)-S\b|(?:^|\s)-b\b|(?:^|\s)-k\b|(?:^|\s)-\w*e\b|--pager-end)"
)


def _journalctl_ok(cmd: str) -> bool:
    if _FOLLOW.search(cmd) or _JOURNAL_MUTATE.search(cmd):
        return False
    return bool(_JOURNAL_BOUND.search(cmd))


def _top_ok(cmd: str) -> bool:
    has_b = re.search(r"(?:^|\s)-\w*b", cmd) is not None
    has_n = re.search(r"-\w*n\s*\d+|-n\s*\d+", cmd) is not None
    return has_b and has_n


def _interval_ok(tokens) -> bool:
    nums = [t for t in tokens[1:] if re.fullmatch(r"\d+(?:\.\d+)?", t)]
    if not nums:
        return True
    if len(nums) >= 2 and nums[-1].isdigit() and int(nums[-1]) >= 1:
        return True
    return False


def _is_read_only(cmd: str) -> bool:
    tokens = cmd.split()
    if not tokens:
        return False
    binary = _basename(tokens[0])
    blk = _STREAM_BLOCK.get(binary)
    if blk and blk.search(cmd):
        return False
    if binary == "ps":
        return _ps_ok(tokens)
    if binary == "journalctl":
        return _journalctl_ok(cmd)
    if binary in ("tail", "head"):                 # content readers that can stream via -f
        if _FOLLOW.search(cmd):
            return False
        return _safe_content_read(tokens)
    if binary == "top":
        return _top_ok(cmd)
    if binary in ("vmstat", "iostat", "mpstat", "pidstat"):
        return _interval_ok(tokens)
    if binary == "nvidia-smi":
        return _NVIDIA.fullmatch(cmd) is not None
    if any(p.fullmatch(cmd) for p in _STRUCTURED_DIAG):
        return True
    if binary == "du":                                   # F5: size-only, broader user-dir allowlist
        return _du_read_ok(tokens)
    if binary in _CONTENT_READERS:
        return _safe_content_read(tokens)
    if binary in _META_READERS:
        return _safe_meta_read(tokens)
    return False


def _is_safe_filter(seg: str) -> bool:
    """A downstream pipe stage: an allowlisted text filter reading ONLY stdin. No
    file-path args (so `| grep root /etc/shadow` cannot read a file) and no -f stream."""
    toks = seg.split()
    if not toks or _basename(toks[0]) not in _SAFE_FILTERS:
        return False
    if _FOLLOW.search(seg):
        return False
    return not any("/" in t for t in toks[1:])


def _is_safe_readonly_command(cmd: str) -> bool:
    """True for a curated read-only command, INCLUDING a safe pipeline/glob. Strips
    stderr-to-null/fd redirects, rejects any hard-dangerous metachar, then requires the
    shape `<read-only source> [ | <text filter> ]*`. Globs (`*`/`?`) are allowed because
    the source's path allowlist (_safe_path) still validates the literal string, so a glob
    cannot escape a safe prefix (`/proc/driver/nvidia/*` stays inside nvidia driver info,
    while `/etc/*` or a `..` traversal is still denied). Balanced single quotes are permitted
    (F6) — a quoted grep pattern is shell-literal; a `/`-bearing quoted token is still caught by
    the filter's path check, and a quoted secret path (`cat '/etc/shadow'`) still hits _safe_path."""
    stripped = _SAFE_REDIR.sub(" ", cmd)
    if stripped.count("'") % 2 != 0:                      # unbalanced single quote -> refuse (fail closed)
        return False
    if _HARD_META.search(stripped):
        return False
    segs = [s.strip() for s in stripped.split("|")]
    if any(not s for s in segs):                          # empty => `||` or dangling pipe
        return False
    if not _is_read_only(segs[0]):
        return False
    return all(_is_safe_filter(s) for s in segs[1:])


def classify(command: str) -> str:
    """Return 'destructive' | 'read_only' | 'mutating'. Reasoning-blind: command text only."""
    cmd = command.strip()
    if _HELP.fullmatch(cmd):                              # 0) help/version is always safe
        return "read_only"
    if any(p.search(cmd) for p in _DESTRUCTIVE):          # 1) destructive precedes everything
        return "destructive"
    if _is_safe_readonly_command(cmd):                    # 2) curated read-only: a bare command,
        return "read_only"                                #    or a safe <source>|<filter> pipeline/glob
    return "mutating"                                     # 3) deny-first: unknown => confirm


# ===========================================================================
# Output redaction (PRECISION-first). Box stdout is untrusted; key-name redaction is
# useless on it, so this is VALUE-based around (a) the literal credential just used,
# (b) high-precision vendor prefixes, (c) specific secret LABELS = value, (d) URL/inline
# creds. It deliberately does NOT blanket-redact bare hex (checksums/SHAs/image IDs are
# diagnostics) or trip on substrings like 'key' in 'KeyError'/'keyring'. Apply AFTER the
# output cap, BEFORE the output reaches the model (the SSE token stream has no other scrub).
# ===========================================================================
_PATTERN_SCRUBBERS = [
    (re.compile(r"(?i)\bsk[-_][A-Za-z0-9._\-]{8,}"), "sk-[REDACTED]"),
    (re.compile(r"(?i)\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{8,}"), "[REDACTED-STRIPE]"),
    (re.compile(r"\bgithub_pat_[A-Za-z0-9_]{20,}"), "[REDACTED-GH-PAT]"),
    (re.compile(r"(?i)\bgh[pousr]_[A-Za-z0-9]{20,}"), "gh_[REDACTED]"),
    (re.compile(r"\bglpat-[A-Za-z0-9_\-]{16,}"), "[REDACTED-GL-PAT]"),
    (re.compile(r"\bdckr_pat_\S{16,}"), "[REDACTED-DOCKER]"),
    (re.compile(r"(?i)\bhf_[A-Za-z0-9]{8,}"), "hf_[REDACTED]"),
    (re.compile(r"(?i)\bnpm_[A-Za-z0-9]{20,}"), "npm_[REDACTED]"),
    (re.compile(r"\bya29\.[A-Za-z0-9_\-]{20,}"), "[REDACTED-GOOG]"),
    (re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}"), "[REDACTED-SLACK]"),
    (re.compile(r"\b(LTAI|AKIA|AKID|ASIA)[A-Za-z0-9]{8,}\b"), "[REDACTED-AK]"),
    (re.compile(r"\beyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+"), "[REDACTED-JWT]"),
    (re.compile(r"(?i)(bearer\s+)[A-Za-z0-9._\-]{12,}"), r"\1[REDACTED]"),
    (re.compile(r"://[^/\s:@]*:[^/\s:@]+@"), "://[REDACTED]@"),
    (re.compile(r"\b[\w.\-]+:[^\s:@/]{4,}@(?=[\w.\-])"), "[REDACTED]@"),
    (re.compile(r"(?i)(\b(?:mysql|mysqldump|mariadb|psql|redis-cli|mongosh?)\b[^\n]*?\s-p)\S+"), r"\1[REDACTED]"),
    (re.compile(r"(?i)(--password[=\s])\S+"), r"\1[REDACTED]"),
    (re.compile(r"(?i)(\b(?:otp|pin|passcode|2fa)\b\D{0,8})(\d{4,})"), r"\1[REDACTED]"),
    # credential keyword immediately followed (no :/= separator, no prose verb) by a hex/base64
    # value — catches `password <hex>` that _LABEL_VALUE/_PROSE/_scrub_b64 all miss, WITHOUT
    # reintroducing blanket hex redaction (a checksum has no credential keyword in front of it).
    (re.compile(r"(?i)(\b(?:password|passwd|passphrase|secret|token|api[_\-]?key|access[_\-]?key|"
                r"private[_\-]?key|credential)\b[^\n\w]{1,8})([0-9a-f]{16,}|[A-Za-z0-9+/]{20,}={0,2})"),
     r"\1[REDACTED]"),
    (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----",
                re.S), "[REDACTED PRIVATE KEY]"),
]

# Specific compound secret label adjacent to its value -> redact the value. Compound names only
# (api_key/access_key/private_key/...), NOT bare 'key'/'auth' (those gut KeyError:/sshd:auth/keyring).
_LABEL_VALUE = re.compile(
    r"(?i)((?:^|[\s\"'(\[{,;>])(?:[\w.\-]*[_\-])?"
    r"(?:password|passwd|passphrase|secret|token|credentials?|api[_\-]?key|access[_\-]?key|"
    r"secret[_\-]?key|private[_\-]?key|auth[_\-]?token|client[_\-]?secret|aws[_\-]?secret|"
    r"jupyter[_\-]?token|hf[_\-]?token|apikey|passkey|dsn)\s*[:=]\s*)(\S+)"
)
# env-style AUTH/ACCESS/SESSION/COOKIE assignment with a credential-shaped value.
_ENV_SECRET = re.compile(r"(?i)(\b\w*(?:auth|access|session|cookie|signing)\w*\s*=\s*)(\S+)")
# prose: "password/token/secret ... is|to <value>" (handles cloud-init "password set to: X").
# Redacts only the next single token (not the rest of the line) — so benign prose like
# "the password is set. Then we restart ..." only loses "set.", not the whole sentence.
_PROSE_SECRET = re.compile(
    r"(?i)(\b(?:password|passphrase|passwd|secret|token)\b[^\n]{0,20}?\b(?:is|was|to|reset to|set to)\b[\s:=]*)(\S+)",
    re.M,
)
# high-entropy mixed-case base64 (random API keys); host-key fingerprints are exempted in code.
_B64_BLOB = re.compile(r"\b[A-Za-z0-9+/]{32,}={0,2}\b")
_FP_PREFIX = re.compile(r"(?i)(SHA256|SHA1|SHA512|MD5):$")


def _mixed_class(s: str) -> bool:
    return (any(c.islower() for c in s) and any(c.isupper() for c in s)
            and any(c.isdigit() for c in s))


def _cred_shaped(tok: str) -> bool:
    core = tok.strip(".,;:'\"()[]{}<>")
    if len(core) >= 20:
        return True
    return len(core) >= 8 and any(c.isdigit() for c in core) and any(c.isalpha() for c in core)


def _scrub_b64(text: str) -> str:
    def repl(m):
        tok = m.group(0)
        # host-key fingerprint (public) -> keep; cap the length so an attacker can't shelter an
        # arbitrary long secret by printing `SHA256:<secret>` (real SHA256/SHA1/MD5 b64 <= 44 chars).
        if len(tok) <= 45 and _FP_PREFIX.search(text[max(0, m.start() - 8):m.start()]):
            return tok
        return "[REDACTED]" if _mixed_class(tok) else tok
    return _B64_BLOB.sub(repl, text)


def scrub_output(text: str, secrets: Iterable[str] = ()) -> str:
    """Redact secrets from untrusted box output. `secrets` = literal strings the caller knows
    are sensitive (the just-used password AND its base64 form)."""
    out = text
    for s in secrets:                                    # literal credential first — exact removal
        if s and len(s) >= 3:
            out = out.replace(s, "[REDACTED]")
    for pat, repl in _PATTERN_SCRUBBERS:
        out = pat.sub(repl, out)
    out = _LABEL_VALUE.sub(lambda m: m.group(1) + "[REDACTED]", out)
    out = _ENV_SECRET.sub(lambda m: m.group(1) + "[REDACTED]" if _cred_shaped(m.group(2)) else m.group(0), out)
    out = _PROSE_SECRET.sub(lambda m: m.group(1) + "[REDACTED]", out)
    out = _scrub_b64(out)
    return out


def redact(text: str) -> str:
    """Back-compat alias (no known literal secrets)."""
    return scrub_output(text)
