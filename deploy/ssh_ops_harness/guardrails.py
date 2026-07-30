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
import ast
import posixpath
import re
import shlex
from typing import Iterable

# A lone `binary --help/--version` mutates nothing — classify as read_only before anything else.
# Only the UNAMBIGUOUS long forms: `-h`/`-V` are excluded because on power binaries `-h`==--halt
# (shutdown/reboot/poweroff -h powers the box off), and _HELP runs before the destructive scan.
_HELP = re.compile(r"^[\w./-]+\s+(--help|--version|help|version)\s*$")

# Verbs that write EVERY path they are handed, so a lockout path anywhere in the argv is a write.
# `mv` belongs here rather than below because its SOURCE disappears too: `mv /etc/fstab /tmp/bak`
# leaves the box unbootable just as surely as overwriting it does.
_WRITE_ANY_ARG = r"(?:tee|truncate|chmod|chown|chgrp|dd|rm|mv)"
# Verbs that take a SOURCE first and write only their LAST argument. Reading a lockout path with
# these — `cp /etc/fstab /tmp/fstab.bak`, i.e. backing it up before editing — is the careful thing
# to do, and refusing it is the exact over-strictness this re-tiering exists to remove.
_WRITE_LAST_ARG = r"(?:cp|install|ln)"
# Paths where a write is unrecoverable from inside the box, or destroys the only channel we would
# need in order to recover it. These are the deliberate CARVE-OUTS from the 2026-07-30 narrowing
# further down: /etc as a whole moved to `mutating` because that is where a broken service's config
# lives and refusing it refused the repair — but these specific paths are not service config. A bad
# fstab line makes the box unbootable; passwd/shadow/group/sudoers/ssh each end the login path;
# /var/lib holds live database and container state, so a write there is data loss, not a config fix.
_LOCKOUT_BODY = (r"/(?:etc/(?:fstab|crypttab|passwd|shadow|gshadow|group|sudoers(?:\.d)?|ssh"
                 r"|default/grub)|var/lib)")
_LOCKOUT_PATHS = _LOCKOUT_BODY + r"(?:/|\s|$)"
# The same paths, but only where they are the LAST argument — i.e. the destination.
_LOCKOUT_PATHS_DEST = _LOCKOUT_BODY + r"(?:/\S*)?/?\s*$"
# Broad system prefixes, used only where the RULE is about a path being system-owned at all.
_SYSTEM_PATHS = r"/(?:etc|usr|lib(?:64)?|s?bin|var|boot|opt|root)(?:/|\s|$)"

# ===========================================================================
# Tier 1 (checked FIRST): destructive — always hard-refused, case-insensitive.
# Deny-by-EFFECT, but anchored so flags/paths that merely SPELL a destructive verb
# (e.g. `iostat -dd`) don't trip it.
#
# Every pattern here is matched against BOTH the raw command and a path-normalized copy of it
# (see _normalize_paths / classify), so a path rule cannot be defeated by respelling the path.
# ===========================================================================
_DESTRUCTIVE_SRC = [
    # ---- deletion -------------------------------------------------------------------------
    # `rm` was UNCONDITIONALLY destructive until 2026-07-30. Measured cost of that on a live run:
    # a purely READ-ONLY diagnostic probe (curl a port, print the body) was hard-refused because it
    # tidied up its own `/tmp` scratch file at the end. The gate was punishing careful behaviour.
    # Now: irreversible or wide-blast-radius deletes stay refused; a targeted delete of one
    # non-system path falls through to `mutating`, i.e. it still needs the operator's consent card.
    r"\brm\b[^\n]*\s-{1,2}[a-zA-Z-]*[rR]",                 # recursive: rm -r / -rf / --recursive
    # Every shell expansion that makes ONE `rm` delete an unknown number of files. `*`/`?` were
    # covered from the start; brace and bracket expansion were not, so `rm -f /etc/{a,b}` and
    # `rm -f /etc/host[s1]` slipped past the system-path rule below — which claimed in its own
    # comment that system-path deletes stay refused. A rule whose comment overstates it is worse
    # than no rule, because the next reader trusts the comment.
    r"\brm\b[^\n]*[*?]",
    r"\brm\b[^\n]*\{[^}]*,[^}]*\}",                         # brace: /etc/{a,b}
    r"\brm\b[^\n]*\[[^\]]+\]",                              # bracket: /etc/host[s1]
    r"\brm\b[^\n]*\s/(etc|boot|s?bin|lib(64)?|usr|sys|proc|dev|var/lib)(/|\s|$)",
    r"\brm\b[^\n]*\s/\s*$",                                 # rm /
    r"\bunlink\b", r"\bshred\b", r"\btruncate\b",
    r"\bmkfs\w*\b", r"(?<![\w-])dd\b[^\n]*\s(if=|of=|bs=|count=|conv=)",
    # fdisk/parted stay destructive EXCEPT the pure LIST mode (-l/--list), which only prints the
    # partition table — that form is allowlisted read-only in _STRUCTURED_DIAG (F9). The interactive
    # editor (`fdisk /dev/sda`) and every other invocation still hard-refuse.
    r"\bfdisk\b(?!\s+(?:-l|--list)\b)", r"\bparted\b(?!\s+(?:-l|--list)\b)",
    r"\bwipefs\b", r"\bblkdiscard\b", r"\bsgdisk\b\s+(-Z|--zap)",
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
    # ---- device / critical-path writes ----------------------------------------------------
    # Raw disks, the boot chain and the kernel interfaces stay HARD-REFUSED: a bad write there is
    # either unrecoverable or unreasonable to reason about from a consent card.
    r">\s*/dev/[sn]d", r"\bof=/dev/",
    r">\s*/(boot|sys|proc)\b",
    r"\b(cp|mv|tee|install)\b[^\n]*\s/boot/",
    r"\bsed\b[^\n]*-i\w*[^\n]*\s/boot/",
    # The lockout carve-outs described at _LOCKOUT_BODY. Every shape a write can take, with source
    # and destination kept apart so that READING one of these paths stays allowed.
    rf"\b{_WRITE_ANY_ARG}\b[^\n]*\s{_LOCKOUT_PATHS}",
    rf"\b{_WRITE_LAST_ARG}\b[^\n]*\s{_LOCKOUT_PATHS_DEST}",
    rf"\bsed\b[^\n]*-i\w*[^\n]*\s{_LOCKOUT_PATHS}",
    rf">\s*{_LOCKOUT_PATHS}",
    # Copying FROM /dev/null can only blank the destination — it is `truncate` wearing another
    # name, and truncate is unconditionally destructive above. Scoped to system paths so that
    # `cp /dev/null /tmp/marker` still only needs a consent card.
    rf"\b(?:cp|install)\b[^\n]*\s/dev/null\s+{_SYSTEM_PATHS}",
    # /etc, /usr, /lib, /bin, /sbin, /var/lib were in the three patterns above until 2026-07-30 and
    # are deliberately NOT any more. They are where a broken service actually lives, so refusing
    # them refused the repair itself. Measured on a live run: the fault was injected by renaming a
    # file under /usr/lib, and the agent — having diagnosed it exactly right — could not rename it
    # back, because `mv ... /usr/lib/...` was destructive. Every natural form (mv / cp / install /
    # `cat X > Y`) was refused; only contortions (`ln -s`, `python3 shutil.move`) got through, which
    # is not a fix path anyone should have to find. Writing a log next to the image's own
    # /usr/local/jupyterlab.log was refused for the same reason.
    # They now fall through to `mutating`: still gated, but by the operator reading the exact
    # command on a consent card rather than by a blanket path ban. The exceptions are the
    # _LOCKOUT_PATHS above (/etc/fstab, the account and ssh files, /var/lib): those are not service
    # config, so nothing in the measured runs needed them and a bad write there is either
    # unbootable or unrecoverable. The first cut of this narrowing dropped /var/lib along with the
    # rest, which was loosening with no evidence behind it.
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
# Command substitution — denied on the RAW command even inside single quotes (see F14).
_SUBSTITUTION = re.compile(r"\$\(|`")
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
    # Process-manager state: on these GPU images the web app (ComfyUI/Jupyter/filebrowser) is run by
    # supervisord, so "is my service supposed to be running, and did it die?" is answered here.
    # ONLY the reporting subcommands — start/stop/restart/reload/update stay unmatched => mutating.
    r"supervisorctl(\s+-c\s+\S+)?\s+(status|avail|pid|version)(\s+\S+)*",
    # package / shared-lib inventory — read-only, central to driver/CUDA diagnosis.
    # `ldconfig` with NO flag rebuilds the cache (mutating), so require -p/--print-cache.
    r"dpkg\s+(-l|--list|-s|--status|-L|--listfiles)(\s+\S+)*",
    r"ldconfig\s+(-p|--print-cache)",
    # binary location / kind — read-only lookups: print a path/type or "not found", never content,
    # never exec (unlike `ldd`, which runs the loader on the target and is left out on purpose).
    r"(which|whereis|type)\s+\S+(\s+\S+)*",
    r"command\s+-[vV]\s+\S+",
    # F9: root-privileged READ-ONLY hardware / disk / kernel-param introspection. These have no
    # write/mutate mode in the forms allowed here; on a VM they typically need `sudo`, which is
    # stripped before this check (the destructive scan already ran on the sudo-inclusive string).
    r"blkid(\s+\S+)*",                                   # fs type / UUID of a block device
    r"dmidecode(\s+\S+)*",                               # DMI/SMBIOS hardware inventory (read-only)
    r"lshw(\s+\S+)*",                                    # hardware tree (read-only)
    r"smartctl\s+(-[aAiHxc]+|--all|--info|--health|--attributes|--scan)(\s+\S+)*",  # disk SMART read
    r"sysctl\s+(-a|-A|--all)",                           # dump all kernel params
    r"sysctl\s+[\w.]+",                                  # read ONE key (a `name=value` write has '=', fails)
    r"(fdisk|parted)\s+(-l|--list)(\s+\S+)*",            # partition LIST (interactive editor stays destructive)
    r"sgdisk\s+(-p|--print)(\s+\S+)*",                   # GPT partition table print
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
    # F10: image service-launch definitions. "Why isn't ComfyUI running / how is it supposed to
    # start / what flags does it get (is `--listen` there?)" is answered ONLY by these files, and
    # the launch flags are the root cause for the bind-127.0.0.1 class. These are image build
    # artifacts, not user data; any token/password that a conf inlines is still value-scrubbed on
    # the way out by scrub_output, and _DENY_PATH_SUBSTR still tripwires secret file names.
    "/start.d/", "/etc/supervisor/", "/usr/supervisor/",
)
_SAFE_READ_EXACT = {
    "/etc/os-release", "/etc/lsb-release", "/etc/hostname", "/etc/machine-id",
    "/etc/timezone", "/etc/issue",
    "/entrypoint.sh",                                    # F10: container launch script
    # F8: mount/partition config — the core of data-disk "为什么 df 看不到我的盘" diagnosis. A
    # fresh cloud data disk is raw+unmounted, and a WRONG /etc/fstab entry is the classic reason a
    # mount silently fails on boot; both are diagnosed by reading these. fstab/mtab hold device
    # UUIDs + mount options, not user secrets (secret files still tripwire via _DENY_PATH_SUBSTR).
    "/etc/fstab", "/etc/mtab",
    "/proc/meminfo", "/proc/cpuinfo", "/proc/loadavg", "/proc/uptime",
    "/proc/version", "/proc/stat", "/proc/diskstats", "/proc/mounts", "/proc/swaps",
    "/proc/partitions",                                  # F8: raw block-device table (name/size)
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


def _mask_single_quoted(s: str) -> str:
    """Blank out the CONTENT of single-quoted spans, preserving length/offsets. Used only for the
    metachar scan and pipe-boundary detection (F14) — never for path validation."""
    out, inq = [], False
    for ch in s:
        if ch == "'":
            inq = not inq
            out.append("'")
        else:
            out.append("x" if inq else ch)
    return "".join(out)


def _mask_quoted(s: str) -> str:
    """Blank out the CONTENT of BOTH single- and double-quoted spans, preserving length/offsets.
    Used for chain-boundary detection, where quoted data must not look like shell syntax."""
    out, quote = [], ""
    for ch in s:
        if quote:
            out.append(ch if ch == quote else "x")
            if ch == quote:
                quote = ""
        elif ch in "'\"":
            quote = ch
            out.append(ch)
        else:
            out.append(ch)
    return "".join(out)


def _unquote(p: str) -> str:
    """Strip balanced surrounding single quotes so a quoted path is validated on its real value."""
    return p[1:-1] if len(p) >= 2 and p[0] == "'" and p[-1] == "'" else p


# Metadata-only reads (ls/stat/file/readlink/wc/*sum; `du` is broader, see F5 below) reveal a
# name/size/perms/hash but never file CONTENT, so they are safe across the introspection tree —
# binaries, libs, device nodes, kernel / hardware state — which is far broader than the content-read
# allowlist. Deliberately EXCLUDES /etc, /var, /home, /root (a filename there can itself be
# sensitive; content reads stay narrow).
_META_SAFE_PREFIXES = ("/dev/", "/usr/", "/sys/", "/proc/", "/lib/", "/lib64/",
                       "/bin/", "/sbin/", "/opt/")
# F11: non-home application/data dirs, where these GPU images put the served app (/workspace/ComfyUI,
# /data/..., mounted volumes). Metadata-only, and deliberately NOT /root or /home — see _safe_meta_path.
_META_APP_PREFIXES = ("/workspace", "/data", "/mnt")

# --- F7: venv / Python-package introspection (content + metadata) ------------------------------
# Python-env diagnosis ("torch.cuda.is_available() 是 False / import 报没这个模块") fundamentally
# needs to inspect the interpreter's INSTALLED packages, which live under a venv the user created
# in a user-data dir (/root/badenv/.../site-packages, /home/u/venv/...). A live CPU-only-torch
# repro proved the harness lands the correct dominant cause but CANNOT confirm it: every venv read
# is refused (running python/pip stays mutating by design; ls/cat under /root is denied by F4).
# A `.../site-packages/` tree holds DEPENDENCY CODE (public library files — torch/version.py's
# `cuda = None` / `__version__ = '...+cpu'` is the actual tell), not user config/secrets, so both
# ls/stat and cat/head are opened there. Scope is a path SHAPE (site-packages / *.dist-info /
# pyvenv.cfg), not a prefix — so it works wherever the venv lives — and the secret-FILE tripwire
# substrings still deny even inside it (only the whole-dir `/root` block is lifted, as with F5 du).
_PKG_DENY_SUBSTR = tuple(d for d in _DENY_PATH_SUBSTR if d != "/root")


def _pkg_safe_path(p: str) -> bool:
    if ".." in p:
        return False
    if any(d in p.lower() for d in _PKG_DENY_SUBSTR):    # secret files still deny inside site-packages
        return False
    # site-packages covers the package tree AND the *.dist-info metadata dirs inside it; pyvenv.cfg
    # sits at the venv root. Require the trailing slash / exact end so `/site-packagesfoo/x` (a
    # look-alike dir) does NOT match.
    return ("/site-packages/" in p or p.endswith("/site-packages")
            or p.endswith("/pyvenv.cfg"))


# F13: application log files. "Why did my service die?" is answered by the service's OWN log and
# nothing else — a live run showed the agent trying `tail` on the app log on nearly every round, being
# refused every time, and then INVENTING a cause ("核心包未安装" / "启动脚本本轮未执行"). Scope is a
# path SHAPE (a .log/.out/.err under an application dir), never /var/log — system/auth logs there leak
# credentials and login records. Values inside are still secret-scrubbed by scrub_output on the way
# out (a Jupyter `token=` in a startup log is redacted), and the secret-FILE tripwires still deny.
_APP_LOG_PREFIXES = ("/workspace/", "/data/", "/mnt/", "/opt/", "/start.d/", "/usr/local/")
_APP_LOG_SHAPE = re.compile(r"(?:\.(?:log|out|err)|(?:^|/)nohup\.out)$", re.I)


def _app_log_path(p: str) -> bool:
    if ".." in p or p.startswith("/var/log"):
        return False
    if any(d in p.lower() for d in _DU_DENY_SUBSTR):     # secret files still deny
        return False
    return bool(_APP_LOG_SHAPE.search(p)) and any(p.startswith(pre) for pre in _APP_LOG_PREFIXES)


def _safe_path(p: str) -> bool:
    p = _unquote(p)
    if ".." in p:
        return False
    if _app_log_path(p):                                 # F13: the service's own log
        return True
    if _pkg_safe_path(p):                                # F7: venv package internals (minus secret files)
        return True
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
    that, allow the introspection tree AND the user/app data dirs (still minus any secret-FILE
    location).

    F11 — why the non-home APP dirs are included: a live run proved the cost of denying them. With
    the app dir unlistable the agent could not find ComfyUI's source checkout at /workspace/ComfyUI,
    searched pip site-packages instead, and confidently concluded "镜像没装 ComfyUI 本体" — while the
    app was fully installed and merely STOPPED. Evidence starvation produced a WRONG root cause,
    which is worse than the disclosure it avoided: these reads return names/sizes/perms, never file
    CONTENT.

    /root and /home stay EXCLUDED: a red-team round locked `ls /root` / `ls -la /root/models` as
    refused (a filename in a home dir is itself potentially sensitive) and that gate is unchanged.
    Where the app lives under /root, its existence is still provable with the size-only `du`
    allowance (F5) — so the agent is never forced to guess."""
    p = _unquote(p)
    if _safe_path(p):
        return True
    if ".." in p:
        return False
    if any(d in p.lower() for d in _DENY_PATH_SUBSTR):
        return False
    if any(p == pre or p.startswith(pre + "/") for pre in _META_APP_PREFIXES):
        return True
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


# F12: a `du` walk rooted at `/` may descend ONE level (top-level dirs + their sizes). Deeper walks
# would enumerate subdirectory NAMES inside home dirs, which the `ls /root` red-team lock forbids.
_DU_DEPTH = re.compile(r"(?:--max-depth[=\s]\s*|(?:^|\s)-d\s*)(\d+)")


def _du_safe_path(p: str) -> bool:
    p = _unquote(p)
    if ".." in p:
        return False
    low = p.lower()
    if any(d in low for d in _DU_DENY_SUBSTR):
        return False
    if _safe_meta_path(p):                               # F4 introspection tree: a size read is fine
        return True
    # F12: the filesystem ROOT only — `du -d 1 /` and `du -sh /*`. "Disk is full, WHERE did it go?" is
    # answered by exactly these two shapes, and refusing them is not a safe default: a live 95%-full
    # repro proved the agent then blamed "容器镜像层" and recommended clearing apt/pip caches while a
    # single 46G file sat unmentioned. They emit standard FHS directory names plus SIZES, never file
    # contents; the secret-file tripwires and the F12 depth cap still apply.
    #
    # Deliberately NOT "any top-level dir": a red-team round locked `du -sh /etc` as refused, and that
    # gate is unchanged. Drill-down after the root sweep still works through the F5 user-dir allowance
    # (`du -sh /root`, `du -sh /root/data`).
    if p == "/" or re.fullmatch(r"/\*+", p):
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
    # F12 depth cap: only when the walk is rooted at `/`. A bounded walk elsewhere (e.g.
    # `du -d 2 /workspace`) stays allowed — that tree is already listable (F11).
    if any(t == "/" for t in tokens[1:]):
        m = _DU_DEPTH.search(" ".join(tokens))
        if m and int(m.group(1)) > 1:
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


# --- F10: loopback-only HTTP probe -------------------------------------------
# "Does the service actually answer on its port?" is THE discriminator between the two most common
# real web-service failures — process DEAD vs process ALIVE but bound to 127.0.0.1 (the classic
# missing `--listen 0.0.0.0`, which makes the public/pod URL 403 while the box is perfectly healthy).
# Neither `ss` nor `ps` can answer it: only asking the port does. A live run proved the agent burns
# its whole turn budget when this is refused.
#
# Scope is deliberately airtight so this can never exfiltrate or mutate:
#   - every non-flag argument MUST be a loopback URL (127.0.0.1 / localhost / ::1 / 0.0.0.0),
#   - flags are a closed allowlist; anything unknown DENIES (deny-by-default preserved),
#   - every flag that sends a body, uploads, overrides the HTTP method, attaches auth/headers, or
#     writes a real file is banned (`-o` is permitted ONLY to /dev/null).
# So the worst case is a GET against a service already running on this box, whose response is then
# capped + secret-scrubbed like any other command output.
_LOOPBACK_URL = re.compile(
    r"^https?://(?:127(?:\.\d{1,3}){3}|localhost|\[::1\]|0\.0\.0\.0)(?::\d+)?(?:/[^\s]*)?$", re.I)
_PROBE_VALUE_FLAGS = {"-m", "--max-time", "--connect-timeout", "--max-redirs", "-o", "--output",
                      "--timeout", "--tries", "-t", "-w", "--write-out"}
_PROBE_BOOL_FLAGS = {"-s", "--silent", "-S", "--show-error", "-I", "--head", "-i", "--include",
                     "-L", "--location", "-f", "--fail", "-k", "--insecure", "-4", "-6",
                     "-q", "--quiet", "--spider", "-nv", "--no-verbose", "-O-", "--server-response"}
# Short-flag letters that are boolean (take no value) — used to accept clusters like `-sS`.
_PROBE_BOOL_SHORT = set("sSIiLfk46q")
# Anything that can send data, upload, re-method, authenticate, or name an output file.
# case-SENSITIVE on purpose: CLI flags are, and `-O` (write file) must not be conflated with the
# permitted `-o /dev/null`. `-T` is banned outright — it is --upload-file in curl even though it is
# --timeout in wget, and the safe wget form (`--timeout=N`) is still available.
_PROBE_BANNED = re.compile(
    r"(?:^|\s)(?:-d|--data\S*|-F|--form\S*|-T|--upload-file|-X|--request|-u|--user|-H|--header|"
    r"-b|--cookie|-c|--cookie-jar|-K|--config|-e|--referer|-A|--user-agent|-O|--remote-name|"
    r"-D|--dump-header|--output-document\S*|-P|--directory-prefix)(?:[=\s]|$)")


def _http_probe_ok(tokens) -> bool:
    joined = " ".join(tokens)
    if _PROBE_BANNED.search(joined):
        return False
    try:
        # Re-tokenize with quote awareness: `-w "%{http_code} %{time_total}"` is ONE value, and
        # naive whitespace splitting turned its tail into a bogus non-loopback "URL".
        tokens = shlex.split(joined)
    except ValueError:
        return False                                     # unbalanced quotes -> cannot reason -> deny
    urls = []
    i = 1
    while i < len(tokens):
        t = tokens[i]
        base, _, inline = t.partition("=")
        if base in _PROBE_VALUE_FLAGS:
            val = inline if inline else (tokens[i + 1] if i + 1 < len(tokens) else "")
            if base in ("-o", "--output") and val != "/dev/null":
                return False                             # never write a real file
            if base in ("-w", "--write-out") and "%output{" in val.lower():
                return False                             # curl >=7.87: %output{f} WRITES a file
            i += 1 if inline else 2
            continue
        if t.startswith("-"):
            if t in _PROBE_BOOL_FLAGS:
                i += 1
                continue
            # clustered short booleans (`-sS`, `-sSL`): accept only when EVERY letter is itself an
            # allowlisted boolean. A value-taking or unknown letter (`-so /dev/null`, `-sO`) denies.
            if re.fullmatch(r"-[a-zA-Z0-9]+", t) and all(c in _PROBE_BOOL_SHORT for c in t[1:]):
                i += 1
                continue
            return False                                 # unknown flag -> deny
        urls.append(t)
        i += 1
    return bool(urls) and all(_LOOPBACK_URL.match(u) for u in urls)


def _is_read_only(cmd: str) -> bool:
    tokens = cmd.split()
    if not tokens:
        return False
    binary = _basename(tokens[0])
    if binary in ("curl", "wget"):                       # F10: loopback-only service probe
        return _http_probe_ok(tokens)
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
    # Match the allowlist on the BASENAME-normalized command as well: a venv/conda interpreter is
    # invoked by absolute path (`/usr/local/miniconda3/envs/comfyui/bin/python --version`), which is
    # exactly as read-only as the bare form. `_HELP` above already permits the path-qualified
    # `--version`, so this only closes the gap for forms that carry a redirect (`2>&1`) and for the
    # other allowlisted read-only subcommands (`pip list`, `conda env list`, ...).
    norm = " ".join([binary] + tokens[1:])
    if any(p.fullmatch(cmd) or p.fullmatch(norm) for p in _STRUCTURED_DIAG):
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
    # F14: scan for hard metachars on a copy whose SINGLE-QUOTED spans are blanked out. Inside single
    # quotes the shell expands nothing, so `grep 'comfy$'` / `grep 'a|b'` are inert text — yet the flat
    # scan refused them, and anchoring a grep is routine (a live run lost ~1/4 of its budget to exactly
    # this). Masking preserves offsets, so pipe boundaries are located outside quotes too. Path safety
    # is UNAFFECTED: segments are taken from the ORIGINAL text and every path token is still validated,
    # so `cat '/etc/shadow'` remains refused.
    masked = _mask_single_quoted(stripped)
    if _HARD_META.search(masked):
        return False
    # Command substitution stays refused even when quoted. Inside single quotes `$(...)` is inert in a
    # correct shell, but this is the one construct where a quoting bug turns into arbitrary execution,
    # so it is belt-and-braces denied on the RAW text (a red-team round locked `grep '$(whoami)'`).
    # A bare `$` anchor (`grep 'comfy$'`) carries no such risk and stays allowed.
    if _SUBSTITUTION.search(stripped):
        return False
    segs, prev = [], 0
    for i, ch in enumerate(masked):
        if ch == "|":
            segs.append(stripped[prev:i])
            prev = i + 1
    segs.append(stripped[prev:])
    segs = [s.strip() for s in segs]
    if any(not s for s in segs):                          # empty => `||` or dangling pipe
        return False
    if not _is_read_only(segs[0]):
        return False
    return all(_is_safe_filter(s) for s in segs[1:])


# F9: a leading `sudo` on an otherwise read-only command. On a VM many genuinely read-only
# hardware/disk reads (blkid, dmidecode, smartctl, cat /etc/fstab, fdisk -l) need root. We strip a
# leading `sudo` (+ its no-op flags / `-u user`) so `sudo <read-only>` inherits the read-only tier.
# This is SAFE because the destructive scan in classify() runs FIRST on the ORIGINAL sudo-inclusive
# string — so `sudo rm`/`sudo mkfs`/`sudo passwd`/`sudo dd of=...` stay destructive — and a stripped
# command that isn't in the read-only allowlist (`sudo cat /etc/shadow`, `sudo vim`) still lands in
# `mutating` (needs confirm, never auto-run). Flags that turn sudo into a shell / editor
# (-i / -s / -e / sudoedit) are NOT stripped, so those never reach the read-only check.
def _strip_sudo(cmd: str) -> str:
    toks = cmd.split()
    if not toks or toks[0] != "sudo":
        return cmd
    i = 1
    while i < len(toks):
        t = toks[i]
        if t in ("-u", "-g", "-U"):                      # sudo -u user <cmd>: skip flag + its value
            i += 2
            continue
        if t in ("-i", "-s", "-e"):                      # login shell / sudoedit -> NOT a read-only strip
            return cmd
        if t.startswith("-"):                            # -n/-E/-H/-A/-k/-S ...: no-op for classification
            i += 1
            continue
        break
    return " ".join(toks[i:]) if i < len(toks) else cmd


# A command refused for its FORM (chaining / substitution / find) rather than because it mutates.
# This is ONLY used to word the refusal message — it never grants execution, so it cannot widen the
# security boundary. It matters because the refusal text is the model's sole feedback channel: a live
# run showed a form-refused `ls /a; ls /b` answered with "this changes the box" makes the model retry
# ANOTHER chained variant instead of splitting into single commands, burning the whole turn budget
# (24/50 commands refused, max_turns hit, no verdict).
# =============================================================================
# Tier 2: mutating — refused in Phase 1. Everything NOT matching is read_only.
#
# POLICY CHANGE (2026-07-23, product owner's call). This lane is read-only
# DIAGNOSIS, so the boundary is now defined by EFFECT instead of by a curated
# allowlist of blessed commands. Two things forced the change:
#   * the allowlist was a treadmill — every new image/scenario needed new entries;
#   * it actively produced WRONG answers. A live N=3 repro went from 1/3 to 3/3
#     correct root causes purely by widening what the agent could READ; every
#     fabrication traced to evidence starvation, not to bad reasoning.
# Reads are therefore allowed by default, and only these classes stay refused:
#   1. writes / state changes on the box
#   2. execution of arbitrary code (a `python -c` is an unbounded write primitive)
#   3. network egress off the box (exfil channel; loopback probes stay allowed)
#   4. commands that stream or block forever (they burn the entire turn budget)
#
# Secret-bearing READS (env, cloud-init logs, /proc/*/environ, `ps auxe`) are now
# ALLOWED by explicit product decision: on this platform those are the operator's
# own platform keys, and the instance password is already visible in the console,
# so reading them discloses nothing the requesting tenant cannot already see. The
# literal SSH credential is still stripped from output by scrub_output as defense
# in depth, and the destructive tier above is unchanged and still checked first.
# =============================================================================

# Binaries whose invocation changes the box (or holds the session open forever).
_MUTATING_BINARIES = {
    # `rm`/`unlink` belong here as of 2026-07-30. They used to be unconditionally destructive, so
    # this set never needed them; once the destructive patterns were narrowed to recursive/glob/
    # system-path deletes, a targeted `rm -f /tmp/scratch` fell through to read_only — i.e. a delete
    # that executes with NO consent card. Narrowing a refusal must never turn into an auto-run.
    "rm", "unlink",
    # These write a file on EVERY invocation — there is no read-only form of them to preserve, and
    # all four classified read_only until 2026-07-30, i.e. they wrote with no consent card at all.
    "split", "csplit", "mknod", "mkfifo",
    # Wrappers that run another command but whose OWN argument grammar cannot be parsed reliably
    # (taskset/numactl take either a positional mask or a flag; flock takes a path or an fd). We
    # cannot tell where the wrapper ends and the inner command begins, so they fail closed here
    # instead of being unwrapped. See _WRAPPER_BINARIES for the ones that CAN be unwrapped.
    "taskset", "numactl", "flock", "chroot", "unshare", "nsenter", "script", "strace", "ltrace",
    "cp", "mv", "ln", "mkdir", "rmdir", "touch", "tee", "install", "patch", "rename",
    "chmod", "chown", "chgrp", "setfacl", "chattr", "mktemp",
    "mount", "umount", "swapon", "swapoff", "mkswap", "fallocate", "resize2fs",
    "xfs_growfs", "growpart", "e2fsck", "fsck", "sfdisk", "dd", "shred", "truncate",
    "tar", "unzip", "zip", "gzip", "gunzip", "bzip2", "bunzip2", "xz", "unxz", "7z", "cpio",
    "kill", "pkill", "killall", "renice", "modprobe", "insmod", "rmmod", "depmod",
    "nvidia-modprobe", "useradd", "adduser", "groupadd", "usermod",
    "logrotate", "updatedb", "mandb", "make", "cmake", "gcc", "g++", "ld", "strip",
    # interactive / pager / streaming: these hold the channel until the hard timeout
    "vi", "vim", "nano", "emacs", "ed", "less", "more", "man", "watch", "htop",
    "screen", "tmux", "at", "batch", "systemd-run",
}
# Wrapper prefixes: the command's EFFECT belongs to the inner command, not to the wrapper's name.
# Until 2026-07-30 none of these appeared in any set, which left the gate structurally asymmetric:
# the destructive scan is a regex over the WHOLE string, so `nice rm -rf /x` was caught, but the
# mutating check reads only the FIRST token, so `nice touch /etc/x` read as read_only and auto-ran
# with no consent card. Wrapping is the cheapest bypass there is, and it defeated exactly one half
# of the gate — the half a human is standing in.
#
# The value is how many of the wrapper's OWN positionals precede the inner command once flags are
# consumed (`timeout 5 cmd` has one: the duration). Failing to find an inner command at all fails
# CLOSED, so `ionice -p 1234 -c 3` (which renices a running pid rather than running anything) lands
# in mutating rather than being waved through as an empty read.
_WRAPPER_BINARIES = {
    "nice": 0, "ionice": 0, "nohup": 0, "setsid": 0, "stdbuf": 0, "eatmydata": 0,
    "busybox": 0, "command": 0, "timeout": 1,
}
# Wrapper flags that consume a SEPARATE value token (`-n 5`, `-o L`, `-s KILL`, `-k 10`). Getting
# this list wrong cannot fail open: a value token mistaken for the inner command is itself
# classified, and a bare `5` or `L` is not a known binary, so it lands in mutating.
_WRAPPER_VALUE_FLAGS = {"-n", "-c", "-o", "-e", "-i", "-s", "-k", "-p", "-N", "-m"}
# `command -v/-V foo` only LOOKS UP foo (like `which`) rather than running it, and it is a common
# environment probe. Every other `command` form executes.
_COMMAND_LOOKUP = re.compile(r"(?:^|\s)-[vV](?:\s|$)")

# Binaries that READ by default but take a destination path, so the write hides in a FLAG rather
# than in the binary's name. All of these classified read_only on 2026-07-30 — they wrote a file
# with no consent card. `base64 -d -o out` and `xxd -r in out` are the standard "write arbitrary
# bytes to an arbitrary path" idioms, so this was not a corner case.
# A blanket "-o means output" rule would be wrong: `ps -o pid,comm`, `lsblk -o NAME`, `findmnt -o`
# and `df -o` all use -o for FORMAT and are core diagnostics. Hence per-binary scoping.
_OUTPUT_FLAG_WRITERS = {
    "openssl": re.compile(r"(?:^|\s)-(?:out|keyout)(?:[=\s]|$)"),
    "sort": re.compile(r"(?:^|\s)(?:-o|--output)(?:[=\s]|$)"),
    "base64": re.compile(r"(?:^|\s)(?:-o|--output)(?:[=\s]|$)"),
    "gpg": re.compile(r"(?:^|\s)(?:-o|--output)(?:[=\s]|$)"),
    # `xxd -r` exists to turn hex back into bytes; its whole purpose is producing binary output.
    "xxd": re.compile(r"(?:^|\s)-(?:r|revert)(?:\s|$)"),
}

# Running arbitrary code is an unbounded write primitive, so these stay refused even
# though they can look read-only.
_EXEC_BINARIES = {
    "eval", "exec", "source", "xargs", "awk", "gawk", "mawk", "nawk",
    # shells: `nvidia-smi | sh` executes whatever the box printed — untrusted output
    # becoming code is the XPIA hole this whole module exists to prevent.
    "sh", "bash", "zsh", "ksh", "dash", "fish", "csh", "tcsh", "su", "sudo",
    # `ldd` runs the dynamic loader against its target (LD_* hooks), so it is execution.
    "ldd",
}
# Interpreters are execution UNLESS the invocation only asks for a version/help banner
# (`python --version` is a genuine, and heavily used, environment probe).
_INTERPRETERS = {"python", "python2", "python3", "perl", "ruby", "node", "php", "lua", "Rscript"}
_VERSION_ONLY = re.compile(r"^(--version|-V|--help|version|help)$")
# Executing a file on the box (a script, or anything invoked by relative path) is
# execution regardless of what it is named — this is what keeps an unknown binary from
# becoming an arbitrary write primitive now that the read allowlist is gone.
_SCRIPT_SHAPE = re.compile(r"\.(sh|bash|py|pl|rb|js|php|lua|ksh|zsh|run|bin|out)$", re.I)
# Reads that never terminate or that stream a whole block device.
_BLOCKING_PATHS = re.compile(r"^/proc/kmsg$|^/dev/(sd|nvme|vd|hd|xvd|loop|zero|random|urandom|full|port|mem|kmem)")
# Readers that emit raw byte CONTENT — only these turn a device path into an endless stream.
_RAW_READERS = {"cat", "tac", "nl", "strings", "od", "xxd", "hexdump", "base64",
                "head", "tail", "split", "cmp", "diff", "wc"}
# Leaving the box is an exfil channel. curl/wget are handled separately below, since
# loopback probes are how "is my service actually up?" gets answered.
_NET_BINARIES = {"nc", "ncat", "netcat", "socat", "telnet", "ftp", "tftp", "sftp",
                 "scp", "rsync", "ssh"}

# Subcommand-sensitive: the binary is fine but a particular verb is not. Anchored to
# the START of the segment so a PATH that merely contains the word (e.g.
# `cat /var/log/cuda-installer.log`) cannot trip it.
_MUTATING_FORMS = [re.compile(p, re.I) for p in [
    r"^(apt|apt-get|aptitude|yum|dnf|apk|pacman|zypper)\b.*\b(install|remove|purge|upgrade|update|autoremove)\b",
    r"^(pip|pip3|conda|mamba|npm|yarn|pnpm|cargo|gem|poetry|uv)\b.*\b(install|uninstall|remove|update|upgrade|add|sync|clean)\b",
    r"^go\b\s+(get|install|mod|build|run|clean)\b",
    r"^systemctl\b.*\b(start|stop|restart|reload|enable|disable|mask|unmask|daemon-reload|set-property|edit|kill)\b",
    r"^service\b\s+\S+\s+(start|stop|restart|reload)\b",
    r"^supervisorctl\b.*\b(start|stop|restart|reload|update|add|remove|clear|shutdown)\b",
    r"^(docker|podman|nerdctl)\b\s+(run|exec|start|stop|restart|kill|rm|rmi|build|pull|push|create|cp|commit|load|import|tag|prune|compose)\b",
    r"^kubectl\b\s+(apply|create|delete|edit|patch|scale|drain|cordon|uncordon|exec|cp|rollout|label|annotate)\b",
    r"^helm\b\s+(install|upgrade|uninstall|delete|rollback)\b",
    r"^git\b\s+(clone|pull|fetch|push|checkout|switch|reset|revert|merge|rebase|commit|add|rm|mv|clean|init|stash|apply|cherry-pick)\b",
    r"^crontab\b\s+(-e|-r)\b",
    r"^sysctl\b\s+(-w\b|\S+=)",
    r"^(iptables|ip6tables|nft|ufw|firewall-cmd)\b\s*(-A|-I|-D|-F|-P|-X|add|delete|insert|allow|deny|enable|disable|reload)\b",
    r"^ip\b\s+.*\b(set|add|del|change|replace|flush)\b",
    r"^(nmcli|hostnamectl|timedatectl|localectl|loginctl)\b\s+\S*set",
    r"^date\b\s+(-s|--set)\b", r"^hwclock\b\s+(-w|--systohc)\b",
    r"^journalctl\b.*--(vacuum|rotate|flush|sync|relinquish|setup-keys|update-catalog)",
    r"^nvidia-smi\b.*\s(-r|--gpu-reset|-pl|-pm|-e\b|--ecc-config|-ac|-lgc|-rgc|--applications-clocks|--persistence-mode)",
    r"^sed\b.*\s-i",
    r"^(python\d?(\.\d+)?|perl|ruby|node|php|lua)\b.*\s-(c|e)\b",
    r"^(sh|bash|zsh|ksh|dash)\b.*\s-c\b",
    # find's -exec/-execdir are handled by _find_is_mutating (below): they run a command per match,
    # so the inner command is classified with this SAME gate and only a read-only inner is allowed.
    # The primaries here always write or block regardless of any inner command, so they stay a flat
    # refusal (backstop; the find branch already covers them).
    r"^find\b.*\s-(delete|ok|okdir|fprint|fprintf|fls)\b",
    r"^ldconfig\b(?!\s+(-p|--print-cache))",             # a bare ldconfig REBUILDS the cache
]]

# ---------------------------------------------------------------------------
# Two NARROW escape hatches, both preserving "never trust an arbitrary payload".
#
# `python -c` is classified STRUCTURALLY: the payload is parsed and every node must
# be on an allowlist, every call must resolve to a read-only callable, and `open`
# must be in a read mode. Default-deny — anything unrecognized is refused.
#
# This replaced an allowlist of seven literal torch payloads. That list was sound but
# useless in practice: reading a YAML/JSON config field, or checking any library's
# version other than torch's, was refused even though it is plainly read-only, and
# every new probe needed a code change. Judging structure instead accepts any
# composition of read-only primitives while keeping the same guarantee — the gate
# never has to "trust" a payload, it proves each construct.
#
# Imports are deliberately NOT restricted, because imports are not the dangerous
# part: reaching `os.system` requires CALLING it, and a call is refused unless its
# resolved dotted path is allowlisted. Aliases are resolved through the payload's own
# imports, so `from os import system as s; s(...)` is judged as `os.system` and
# refused exactly like the spelled-out form. Dunder attribute access is refused apart
# from a few metadata names, which closes the `__class__.__bases__.__subclasses__`
# style escapes. Reading a file is allowed because `cat FILE` already is — same
# threat model, no new exposure.
#
# find `-exec`/`-execdir` runs a command per matched file; instead of refusing
# the whole class, _find_is_mutating extracts the inner command and classifies it
# with this very gate — so `-exec grep`/`cat`/`ls` is allowed and `-exec rm`,
# `-exec sh -c ...`, `-exec python -c ...`, `-exec chmod ...` are refused, exactly
# as those commands would be on their own. `-delete`/`-ok*`/`-fprint*` always write
# or block and stay refused above.
# ---------------------------------------------------------------------------
# Calls allowed by resolved dotted path. Everything here is read-only: it inspects
# state or parses data, and none of it touches the filesystem, processes or network.
_PY_SAFE_CALLS = {
    "print", "len", "str", "repr", "int", "float", "bool", "list", "tuple", "set", "dict",
    "sorted", "sum", "min", "max", "round", "abs", "type", "enumerate", "zip", "range",
    "any", "all", "format", "hex", "oct", "bin", "chr", "ord", "divmod",
    "open",                                              # mode-checked below
    "json.load", "json.loads", "json.dumps",
    "yaml.safe_load", "yaml.safe_load_all",
    "glob.glob", "glob.iglob",
    "pkg_resources.get_distribution",
    "shutil.which",                                      # PATH lookup only; no copy/move
    "socket.gethostname",                                # local name, opens nothing
}
# Read-only namespaces. A prefix is used only where EVERY public callable under it is
# an inspector. `sys.` is deliberately absent: `sys.stdin.read()` would block until the
# hard timeout, and version data is an attribute read that needs no call.
_PY_SAFE_CALL_PREFIXES = (
    "torch.cuda.", "torch.backends.", "torch.version.",
    "os.path.", "platform.", "sysconfig.", "importlib.metadata.",
)
# Methods safe on any value: string/dict/list inspection and file reads. Judged by name
# because the receiver is a local whose type is not knowable statically.
_PY_SAFE_METHODS = {
    "read", "readline", "readlines", "close", "decode", "encode",
    "split", "rsplit", "splitlines", "strip", "lstrip", "rstrip", "partition",
    "lower", "upper", "title", "startswith", "endswith", "count", "find", "index",
    "replace", "zfill", "ljust", "rjust", "join", "format",
    "get", "keys", "values", "items", "isdigit", "isalpha", "group", "groups",
}
# Attribute reads that look like dunders but are plain metadata. Anything else starting
# with "__" is refused: __class__/__bases__/__subclasses__/__globals__/__builtins__ are
# the standard route from a harmless object to arbitrary execution.
_PY_SAFE_DUNDERS = {"__version__", "__file__", "__name__", "__doc__", "__path__", "__spec__"}
# `python -m <module>`: fixed set of read-only module entrypoints.
_PY_SAFE_MODULE_RUNS = {"json.tool", "platform", "sysconfig", "site", "torch.utils.collect_env"}
# Receivers whose reads never return. Refused ahead of the by-name method fallback.
_PY_BLOCKING_CALL_PREFIXES = ("sys.stdin",)
_PIP_READ_SUBCOMMANDS = {"list", "show", "freeze", "--version", "-V"}

_PY_ALLOWED_NODES = tuple(node for node in (
    ast.Module, ast.Expr, ast.Assign, ast.If, ast.For, ast.Try, ast.ExceptHandler,
    ast.Pass, ast.Break, ast.Continue, ast.Import, ast.ImportFrom, ast.alias,
    ast.Call, ast.keyword, ast.Name, ast.Attribute, ast.Subscript, ast.Slice,
    ast.Constant, ast.JoinedStr, ast.FormattedValue, ast.Tuple, ast.List, ast.Dict,
    ast.Set, ast.ListComp, ast.SetComp, ast.DictComp, ast.GeneratorExp,
    ast.comprehension, ast.BinOp, ast.UnaryOp, ast.BoolOp, ast.Compare, ast.IfExp,
    ast.Starred, ast.expr_context, ast.operator, ast.cmpop, ast.boolop, ast.unaryop,
    # 3.8 shapes; absent on newer interpreters.
    getattr(ast, "Index", None), getattr(ast, "ExtSlice", None),
) if node is not None)
# Deliberately absent, therefore refused: FunctionDef/ClassDef/Lambda (code objects),
# While (can never terminate), Delete/AugAssign (mutation), Global/Nonlocal, Await/
# AsyncFor/Yield, and the walrus-free rest of the grammar.


def _py_import_aliases(tree) -> dict:
    """Map each name the payload binds via import to the dotted path it refers to, so a
    call can be judged by what it actually resolves to rather than how it is spelled."""
    aliases = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for entry in node.names:
                if entry.asname:
                    aliases[entry.asname] = entry.name
                else:
                    root = entry.name.split(".")[0]      # `import os.path` binds `os`
                    aliases[root] = root
        elif isinstance(node, ast.ImportFrom):
            module = node.module or ""
            for entry in node.names:
                full = module + "." + entry.name if module else entry.name
                aliases[entry.asname or entry.name] = full
    return aliases


def _py_dotted(node, aliases):
    """Resolve a call target to its dotted path, or None when the target is not a plain
    name/attribute chain (a call result or subscript — never allowlistable, because what
    it evaluates to is not decidable here)."""
    parts = []
    current = node
    while isinstance(current, ast.Attribute):
        parts.append(current.attr)
        current = current.value
    if not isinstance(current, ast.Name):
        return None
    parts.append(aliases.get(current.id, current.id))
    return ".".join(reversed(parts))


def _py_open_is_read(call) -> bool:
    """`open(path)` and `open(path, 'r...')` are reads. Any mode carrying w/a/x/+ writes,
    and a computed mode cannot be proven a read, so both are refused. A literal path that
    never ends (a raw block device, /proc/kmsg, stdin, a tty) is refused too — the same
    endless-stream rule the raw readers get above, applied to this new surface."""
    path = call.args[0] if call.args else None
    if isinstance(path, ast.Constant) and isinstance(path.value, str):
        if _BLOCKING_PATHS.match(path.value) or path.value in ("/dev/stdin", "/dev/tty"):
            return False
    mode = call.args[1] if len(call.args) >= 2 else None
    for kw in call.keywords:
        if kw.arg == "mode":
            mode = kw.value
    if mode is None:
        return True                                      # default mode is 'r'
    if not isinstance(mode, ast.Constant) or not isinstance(mode.value, str):
        return False
    return not any(ch in mode.value for ch in "wax+")


def _py_payload_is_readonly(payload: str) -> bool:
    """True when every construct in a `python -c` payload is provably read-only."""
    try:
        tree = ast.parse(payload)
    except (SyntaxError, ValueError, MemoryError, RecursionError):
        return False                                     # unparseable: fail closed
    aliases = _py_import_aliases(tree)
    for node in ast.walk(tree):
        if not isinstance(node, _PY_ALLOWED_NODES):
            return False
        if isinstance(node, ast.Attribute):
            if node.attr.startswith("__") and node.attr not in _PY_SAFE_DUNDERS:
                return False
        if isinstance(node, ast.Call):
            dotted = _py_dotted(node.func, aliases)
            if dotted == "open" and not _py_open_is_read(node):
                return False
            # Reading a stream that never ends holds the channel until the hard
            # timeout. Checked before the by-name method fallback, which would
            # otherwise accept `sys.stdin.read()` on the strength of `read` alone.
            if dotted is not None and dotted.startswith(_PY_BLOCKING_CALL_PREFIXES):
                return False
            if dotted is not None and (dotted in _PY_SAFE_CALLS
                                       or dotted.startswith(_PY_SAFE_CALL_PREFIXES)):
                continue
            # A method on a local value (`fh.read()`, `cfg.get(...)`) is judged by name.
            if isinstance(node.func, ast.Attribute) and node.func.attr in _PY_SAFE_METHODS:
                continue
            return False
    return True


def _py_module_run_is_readonly(argv) -> bool:
    """`python -m <module> …` for read-only entrypoints only. pip is accepted solely
    with a reading subcommand — `pip install` writes."""
    module = argv[2]
    rest = argv[3:]
    if module in ("pip", "pip3"):
        return bool(rest) and rest[0] in _PIP_READ_SUBCOMMANDS
    return module in _PY_SAFE_MODULE_RUNS


def _is_readonly_py_invocation(binary: str, seg: str) -> bool:
    """True for `pythonX -c <provably read-only payload>` and `pythonX -m <read-only
    module>`. Any other interpreter, extra flags, or an unprovable payload returns False
    (so the caller refuses it). Fail closed on any parse error."""
    if binary not in ("python", "python2", "python3"):
        return False
    try:
        argv = shlex.split(seg)
    except ValueError:
        return False
    if len(argv) == 3 and argv[1] == "-c":               # exactly `python -c <payload>`
        return _py_payload_is_readonly(argv[2])
    if len(argv) >= 3 and argv[1] == "-m":
        return _py_module_run_is_readonly(argv)
    return False


# find primaries that write or block no matter what any -exec inner command is.
_FIND_WRITE_PRIMARIES = re.compile(r"(?:^|\s)-(delete|ok|okdir|fprint|fprintf|fls)\b")


def _find_is_mutating(seg: str, depth: int) -> bool:
    """A find is a read UNLESS it uses a writing/blocking primary, or an -exec/-execdir
    whose inner command is not itself read-only. The inner command is classified with
    the SAME gate (destructive + mutating), so `-exec rm`/`sh -c`/`python -c` are refused
    exactly as they would be standalone."""
    if depth > 3:
        return True                                       # runaway -exec nesting: fail closed
    if _FIND_WRITE_PRIMARIES.search(seg):
        return True
    try:
        argv = shlex.split(seg)
    except ValueError:
        return True                                       # unparseable: fail closed
    saw_exec = False
    i = 0
    while i < len(argv):
        if argv[i] in ("-exec", "-execdir"):
            saw_exec = True
            inner, i = [], i + 1
            while i < len(argv) and argv[i] not in (";", "+"):
                if argv[i] != "{}":
                    inner.append(argv[i])
                i += 1
            if not inner:
                return True                               # -exec with no command
            inner_str = " ".join(inner)
            if any(p.search(inner_str) for p in _DESTRUCTIVE) or _is_mutating_segment(inner_str, depth + 1):
                return True
        i += 1
    return False                                          # pure search, or -exec of a read-only inner


def _env_is_read(tokens) -> bool:
    """`env` is dual-use: bare `env` prints the environment (a read), but
    `env FOO=1 cmd` EXECUTES cmd. Only the printing form is a read."""
    for t in tokens[1:]:
        if t.startswith("-") or "=" in t:
            continue
        return False                                      # a bare word => a command to run
    return True


def _strip_wrapper(binary: str, tokens) -> str:
    """Return the inner command of a wrapper invocation, or "" when there is none.

    Consumes the wrapper's own flags (skipping a separate value for the flags in
    _WRAPPER_VALUE_FLAGS) and then the fixed number of its own positionals, leaving the rest.
    """
    i = 1
    while i < len(tokens) and tokens[i].startswith("-"):
        i += 2 if tokens[i] in _WRAPPER_VALUE_FLAGS else 1
    i += _WRAPPER_BINARIES[binary]
    return " ".join(tokens[i:]) if i < len(tokens) else ""


def _wrapper_is_mutating(binary: str, tokens, depth: int) -> bool:
    """Classify a wrapper invocation by its INNER command, run through this same gate."""
    if depth > 3:
        return True                                       # nested wrappers: fail closed
    if binary == "command" and _COMMAND_LOOKUP.search(" ".join(tokens)):
        return False                                      # `command -v foo` looks foo up, no exec
    inner = _strip_wrapper(binary, tokens)
    if not inner:
        return True                                       # no inner command to judge: fail closed
    return _is_mutating_segment(inner, depth + 1)


def _is_mutating_segment(seg: str, _depth: int = 0) -> bool:
    """True if this ONE command changes the box, executes code, leaves the box, or
    blocks forever. Deny-by-effect — anything else is a read. `_depth` bounds the
    recursion when classifying a find -exec inner command against this same gate."""
    seg = _strip_sudo(seg.strip())
    if not seg:
        return False
    if ">" in _SAFE_REDIR.sub(" ", seg):                  # real-file redirection writes
        return True
    # `2>/dev/null` / `2>&1` / `>/dev/null` change nothing, but they are bare WORDS to a naive
    # tokenizer: `env 2>/dev/null` read as the executing form `env <cmd>` and was refused live.
    # Drop them before any token-position rule looks at the segment.
    seg = _SAFE_REDIR.sub(" ", seg).strip()
    tokens = seg.split()
    if not tokens:
        return False
    raw0 = tokens[0]
    binary = _basename(raw0).lower()
    if _SUBSTITUTION.search(seg):                         # substitution executes, even quoted
        return True
    # running a file on the box: a script by name, or anything by relative path
    if _SCRIPT_SHAPE.search(binary) or raw0.startswith("./") or raw0.startswith("../"):
        return True
    if binary in _WRAPPER_BINARIES:                       # the effect is the INNER command's
        return _wrapper_is_mutating(binary, tokens, _depth)
    if binary in _MUTATING_BINARIES or binary in _EXEC_BINARIES or binary in _NET_BINARIES:
        return True
    ofw = _OUTPUT_FLAG_WRITERS.get(binary)                # reader whose write hides in a flag
    if ofw and ofw.search(seg):
        return True
    if binary in _INTERPRETERS:
        if len(tokens) == 1:
            return True                                   # bare `python` is a REPL — blocks forever
        if all(_VERSION_ONLY.match(t) for t in tokens[1:]):
            return False                                  # `python --version` / `--help` only
        # `-c` payloads are proven read-only structurally (AST); `-m` accepts a fixed set
        # of read-only entrypoints. Anything unproven stays refused.
        return not _is_readonly_py_invocation(binary, seg)
    if binary in ("curl", "wget"):
        return not _http_probe_ok(tokens)                 # loopback probe stays allowed
    if binary == "env":
        return not _env_is_read(tokens)
    if binary == "top":                                   # needs BOTH batch mode and an iteration cap
        return not (re.search(r"(?:^|\s)-\w*b", seg) and re.search(r"-\w*n\s*\d+", seg))
    if binary == "nvidia-smi" and re.search(r"(?:^|\s)(dmon|pmon|-l\b|--loop)", seg):
        return True                                       # continuous monitors never return
    # `-f` means FOLLOW only on log readers; on ps/lsblk/df it means full-format/filesystem,
    # and treating it as streaming wrongly refused plain `ps -f` (seen live).
    if binary in ("tail", "head", "journalctl", "logread", "dmesg") and _FOLLOW.search(seg):
        return True
    # Reads that block forever or stream a raw block device. Scoped to readers that dump raw
    # CONTENT: `cat /dev/sda` streams the whole disk, but `blkid /dev/vdb` / `fdisk -l` /
    # `smartctl -a` only query metadata and are core disk diagnostics (red-team-approved).
    if binary in _RAW_READERS and any(
            _BLOCKING_PATHS.match(_unquote(t)) for t in tokens[1:] if t.startswith("/")):
        return True
    blk = _STREAM_BLOCK.get(binary)
    if blk and blk.search(seg):                           # free -s1 / netstat -c
        return True
    if binary in ("vmstat", "iostat", "mpstat", "pidstat") and not _interval_ok(tokens):
        return True
    if binary == "find":
        return _find_is_mutating(seg, _depth)
    return any(p.search(seg) for p in _MUTATING_FORMS)


# Chaining is ACCEPTED now: each segment is classified independently and every one of
# them must be a read. This removes the single largest source of refusals seen live
# (the model naturally writes `ls /a; ls /b`) without widening what any one command
# may do — and it lets the refusal message stop lying about "this changes the box".
_CHAIN_SPLIT = re.compile(r"\|\||&&|[;|]")


def _split_chain(cmd: str):
    """Split a chain into its individual commands on `;` `|` `||` `&&`.

    Boundaries are located on a QUOTE-MASKED copy, then sliced out of the original, so a
    metachar inside quoted DATA is not a boundary. `grep -E 'nginx|caddy|socat|proxy' f` is ONE
    command whose pattern merely contains `|`; splitting it raw manufactured phantom segments and
    the middle alternative `socat` (a net binary name) refused the whole command — seen live.
    """
    masked = _mask_quoted(cmd)
    parts, last = [], 0
    for m in _CHAIN_SPLIT.finditer(masked):
        # A backslash-escaped metachar (`\;`, `\|`) is a literal argument, not a shell
        # separator: `find … -exec grep x {} \;` is ONE command, and the shell passes the
        # `;` to find. Count the backslashes immediately before the match — an ODD count
        # means it is escaped, so it is not a boundary. (`\\;` = escaped backslash + real
        # separator → even count → still splits.)
        bs = 0
        j = m.start() - 1
        while j >= 0 and masked[j] == "\\":
            bs += 1
            j -= 1
        if bs % 2 == 1:
            continue
        parts.append(cmd[last:m.start()])
        last = m.end()
    parts.append(cmd[last:])
    return [s for s in (x.strip() for x in parts) if s]


def is_form_violation(command: str) -> bool:
    """Refused for SHAPE rather than effect. Now only command substitution, since
    chaining and pipes are accepted. Used solely to word the refusal message."""
    return bool(_SUBSTITUTION.search(command or ""))


_MULTI_SLASH = re.compile(r"/{2,}")


def _normalize_paths(cmd: str) -> str:
    """Rewrite every absolute-path-looking token to its canonical spelling.

    The destructive tier contains PATH rules (`/boot`, `/etc/fstab`, `/var/lib`, the system-path
    `rm`), and a regex over the raw string reads `//etc/fstab` and `/tmp/../etc/fstab` as different
    paths from `/etc/fstab` while the kernel does not. Both spellings were accepted as `mutating` on
    2026-07-30 — one consent card away from a write the tier is supposed to refuse outright.

    This is only ever used to produce a SECOND string to scan, never to replace the first: classify
    ORs the two scans, so normalizing can add matches and can never remove one. That is what makes
    the crude tokenisation here safe — mangling a quoted path with spaces cannot let anything
    through, because the raw string is still scanned as well.
    """
    out = []
    for tok in cmd.split():
        raw = _unquote(tok)
        if raw.startswith("/") and len(raw) > 1:
            norm = posixpath.normpath(_MULTI_SLASH.sub("/", raw))
            out.append(norm)
            continue
        out.append(tok)
    return " ".join(out)


def classify(command: str) -> str:
    """Return 'destructive' | 'read_only' | 'mutating'. Reasoning-blind: command text only."""
    cmd = (command or "").strip()
    if not cmd:
        return "mutating"
    if _HELP.fullmatch(cmd):                              # 0) help/version is always safe
        return "read_only"
    normalized = _normalize_paths(cmd)
    if any(p.search(cmd) for p in _DESTRUCTIVE):          # 1) destructive precedes everything
        return "destructive"
    if normalized != cmd and any(p.search(normalized) for p in _DESTRUCTIVE):
        return "destructive"
    if "\n" in cmd:                                       # 2) never accept a multi-line script
        return "mutating"
    for seg in _split_chain(cmd):                         # 3) every segment must be a read
        if _is_mutating_segment(seg):
            return "mutating"
    return "read_only"


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
