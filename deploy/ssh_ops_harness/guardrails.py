"""Reasoning-blind command guardrails for the SSH-ops lane.

Core safety principle: the decision to run / refuse / scope-authorize a command is driven ONLY
by the user intent + the literal command string, NEVER by anything the instance emitted.
Output read off the box is untrusted data, not instructions — so classification happens
here, before execution, on the command text alone. This is the XPIA / prompt-injection
firewall.

Three tiers (mirror internal/tools/safe_executor.go semantics):
  - read_only  : auto-run, no human prompt        (curated, one-shot diagnostics)
  - mutating   : allowed only inside a trusted task-scoped repair authorization
  - destructive: hard-refused, even with task authorization  (checked FIRST, unconditional)

This is an operations gate for the tenant's own guest, not a general-purpose hostile-code sandbox.
Prefer observable pre-state, precise scope and a recoverable change. Reserve the hard-refused tier
for effects that lose data/recovery access, make the guest unbootable, or cross into another control
plane; a high-impact but reversible guest change belongs inside the authorized task scope.

Redaction is precision-first, classification anchors flags to the relevant binary, and the
transport enforces a per-command timeout.

The auto-run tier is positively proven. A command that matches no curated read shape is
`mutating` and therefore requires the already established task scope; an unknown bare name resolved
through the remote PATH never executes without that authorization. This is intentionally conservative
about unauthorised execution, not about repair: an authorized run can use any reversible guest-local command.

Running a FILE is no longer part of that gap, and the rule for it is deliberately small: a
program named by absolute path auto-runs as a read only from /bin, /sbin, /usr/bin and
/usr/sbin. Everything else — /tmp/x, /root/payload, /opt/app/bin/run — stays in the mutating tier.
The one narrow exception is a path-qualified Python/Conda interpreter whose `-c` or
`-m` payload passes the same structural read-only proof as bare `python`: the path spelling
must not turn an already-proven CUDA/package inspection into a write. This proves the payload's
expressed effect, not the executable's identity; `/tmp/python` and a Conda interpreter are treated
the same. An unsafe payload, relative executable, script, or unknown absolute binary still asks first.

This rule deliberately does NOT try to establish that a path is trustworthy. /root/x/bin/payload
and /root/x/payload carry the same real risk; separating them needs a growing list of exceptions
for bin-shaped directories, temp dirs, symlinks, venvs and toolchain paths, and none of it ever
proves a remote file is safe to execute without authorization. System programs auto-read; user and
application paths require the task-scope repair authorization.

What is not open is describing any of this as deny-by-default, because a reader who believes
that will not look for the fallthrough — which is exactly how a `--help` suffix came to skip
the consent card.
"""
import ast
import posixpath
import re
import shlex
from typing import Iterable
from urllib.parse import urlsplit

# There is deliberately NO help/version fast-path. Do not add one back.
#
# There was one, `^[\w./-]+\s+(--help|--version|help|version)\s*$`, consulted at classify()
# step 0 — ahead of BOTH the destructive scan and the multi-line confirmation tier, so anything it
# accepted executed with no consent card in read-only mode as well as write mode. It was
# wrong in four ways, and each one was a full bypass on its own:
#
#   - `\s` matches a NEWLINE. `reboot\n--help` classified read_only, and the multi-line
#     confirmation tier could not save it because it lives inside the `mutating` branch,
#     which a read_only verdict never reaches. ssh_transport hands the whole string to
#     `bash -c`, which runs `reboot` as line 1.
#   - the name was unconstrained: `./unknown --help`, `/root/payload.sh --help`.
#   - `help` and `version` were accepted WITHOUT the dashes, so `curl version` — which is
#     curl fetching a host named `version`, not a version query — took the fast path
#     straight past the loopback-probe rule that exists to gate egress.
#   - and the repair that suggests itself, an allowlist of trusted binaries, does not work
#     either: the transport runs the command through the REMOTE `bash -c` with no fixed
#     PATH, so a bare `dd` is whatever that box's PATH resolves `dd` to. A name is not an
#     identity here, and an allowlist keyed on one only looks like it establishes trust.
#
# Normal diagnostic version queries are classified by the regular rules below; no separate
# help/version bypass is needed.
# Verbs that write EVERY path they are handed, so a lockout path anywhere in the argv is a write.
# `mv` belongs here rather than below because its SOURCE disappears too: `mv /etc/fstab /tmp/bak`
# leaves the box unbootable just as surely as overwriting it does.
_WRITE_ANY_ARG = r"(?:tee|truncate|chmod|chown|chgrp|dd|rm|mv)"
# Verbs that take a SOURCE first and write only their LAST argument. Reading a lockout path with
# these — `cp /etc/fstab /tmp/fstab.bak`, i.e. backing it up before editing — is the careful thing
# to do, and refusing it is the exact over-strictness this re-tiering exists to remove.
_WRITE_LAST_ARG = r"(?:cp|install|ln)"
# Paths where a write can make the guest unbootable, destroy access, or corrupt live state.
# Ordinary service configuration remains confirmable; these paths are always refused.
_LOCKOUT_BODY = (r"/(?:etc/(?:fstab|crypttab|passwd|shadow|gshadow|group|sudoers|ssh"
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
# Matching is PER COMMAND, not over the whole string: nearly every rule below pairs a write verb
# with a path, and over a whole chain those two halves came from DIFFERENT commands. See
# _scan_destructive for the measurement — and note that anything you add here is therefore
# evaluated against one segment, so a rule must not expect to see the rest of the chain.
# ===========================================================================
# These rules name programs, not effects expressed by an argument or target path. They may be
# skipped only when the WHOLE command is proven to pass arguments solely to existing data-only
# consumers. Every effect/path rule below remains active even for such commands.
_DESTRUCTIVE_PROGRAM_WORD_SRC = {
    r"\bunlink\b", r"\bshred\b", r"\bmkfs\w*\b", r"\bwipefs\b", r"\bblkdiscard\b",
    r"\b(lvremove|vgremove|pvremove|lvreduce|vgreduce)\b",
    r"\bshutdown\b", r"\breboot\b", r"\bhalt\b", r"\bpoweroff\b",
    r"\buserdel\b", r"\bgroupdel\b", r"\bchpasswd\b", r"\busermod\b",
}
_DESTRUCTIVE_SRC = [
    *sorted(_DESTRUCTIVE_PROGRAM_WORD_SRC),
    # ---- deletion -------------------------------------------------------------------------
    # Irreversible or wide deletes are refused; a targeted non-system delete is mutating.
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
    # Only zero-length truncation is confirmable; allocation and lockout/kernel targets refuse.
    r"\btruncate\b(?![^\n]*(?:-s\s*0|--size[=\s]*0)(?:\s|$))",
    r"(?<![\w-])dd\b[^\n]*\s(if=|of=|bs=|count=|conv=)",
    # fdisk/parted stay destructive EXCEPT the pure LIST mode (-l/--list), which only prints the
    # partition table — that form is allowlisted read-only in _STRUCTURED_DIAG (F9). The interactive
    # editor (`fdisk /dev/sda`) and every other invocation still hard-refuse.
    r"\bfdisk\b(?!\s+(?:-l|--list)\b)", r"\bparted\b(?!\s+(?:-l|--list)\b)",
    r"\bsgdisk\b\s+(-Z|--zap)",
    r"\bfind\b[^\n]*\s-delete\b",
    # A shell used by find is not itself irreversible. The literal payload is still scanned by
    # every destructive rule, and any shell shape we cannot prove read-only reaches the exact
    # confirmation card. Keeping `sh|bash` here made harmless inspection scripts a hard refusal,
    # contrary to the lane's recoverability boundary.
    r"\bfind\b[^\n]*-exec\s+\S*\b(rm|mv|cp|tee|dd|chmod|chown|truncate|shred|chattr|mkfs|unlink|kill)\b",
    r"\bzpool\b\s+destroy\b", r"\bzfs\b\s+destroy\b", r"\bbtrfs\b[^\n]*\bdelete\b",
    # power / boot
    r"\binit\s+[06]\b",
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
    # Kernel/boot/device interfaces stay refused for every write verb, not only for `>`. The one
    # exception is a per-process fd path: /proc/<pid>/fd/<n> is a handle on a REGULAR file, and
    # truncating it through that handle is exactly the space-reclaim repair measured above — the
    # only way to free a log whose inode an open process is still pinning.
    rf"\b{_WRITE_ANY_ARG}\b[^\n]*\s/(?:boot|sys|dev)/",
    rf"\b{_WRITE_ANY_ARG}\b[^\n]*\s/proc/(?!\d+/fd/)",
    # cp/install/ln write only their LAST argument, so the destination form is separate — otherwise
    # `cp /proc/net/tcp /tmp/out` and `cp /dev/null /tmp/marker`, both legitimate, would be refused.
    rf"\b{_WRITE_LAST_ARG}\b[^\n]*\s/(?:boot|sys|dev|proc)/\S*\s*$",
    # System service paths are confirmable so the lane can repair software. _LOCKOUT_PATHS above
    # remain refused because a bad write there can destroy boot, access, or live data.
    # Recursive ownership/mode rewrites discard old metadata for an unknown tree. A single chmod
    # (including 777) or chattr change is reversible and therefore stays `mutating`.
    r"\bchmod\b.*\s-R\b", r"\bchown\b.*\s-R\b",
    # firewall / services / management-channel lockout
    r"\biptables\b\s+-F", r"\bufw\b\s+disable",
    # Restarting or reloading SSH/network can destroy the recovery channel when config is broken.
    # Starting an already-down service remains confirmable.
    r"\bsystemctl\b[^\n]*\b(stop|kill|restart|reload|try-restart|force-reload|disable|mask)\b[^\n]*\b\S*(ssh|network)\S*\b",
    r"\bservice\b\s+\S*(ssh|network)\S*\s+(stop|restart|reload|force-reload)\b",
    # process kill of init / critical daemons
    r"\bkill\b\s+(-\w+\s+)*-?1(\s|$)",
    r"\b(pkill|killall)\b[^\n]*\b(sshd|systemd|init|dockerd|containerd)\b",
    # orchestrator / container deletes
    r"\bkubectl\b[^\n]*\bdelete\b",
    r"\bdocker\b[^\n]*\b(system\s+prune|volume\s+rm|rmi|image\s+prune|container\s+prune)\b",
    r"\bhelm\b[^\n]*\b(uninstall|delete)\b",
    # fork bomb / cron
    r":\s*\(\s*\)\s*\{", r"\bcrontab\b\s+-r",
    # credential-bearing commands: never run a command that inlines a secret
    r"\bsshpass\b\s+-p",
    r"\bexport\b\s+\w*(KEY|TOKEN|SECRET|PASSWORD|PASSWD|PWD|DSN|AUTH)\w*\s*=",
]
_DESTRUCTIVE = [re.compile(p, re.IGNORECASE) for p in _DESTRUCTIVE_SRC]

# Recursive deletion is confirmable only for trees regenerable by construction: dot-caches,
# __pycache__, /tmp, /var/tmp and /var/cache. A user-named cache directory is not sufficient.
#
# It suppresses only the recursion rule. Every other destructive pattern still applies. Globs,
# traversal, multiple targets, unknown flags and non-regenerable trees cannot take the exemption.
# System paths remain refused by their independent rule. Literal path checks cannot resolve a
# symlink; the confirmation card therefore shows the exact approved command.
_RM_RECURSION_RULES = {
    r"\brm\b[^\n]*\s-{1,2}[a-zA-Z-]*[rR]",
}
_DESTRUCTIVE_NO_RM_RECURSION = [re.compile(p, re.IGNORECASE)
                                for p in _DESTRUCTIVE_SRC if p not in _RM_RECURSION_RULES]
if len(_DESTRUCTIVE_NO_RM_RECURSION) != len(_DESTRUCTIVE_SRC) - len(_RM_RECURSION_RULES):
    # A renamed or reworded pattern would silently stop being suppressed (harmless) or silently
    # suppress the wrong rule (not harmless). Fail at import rather than at classification time.
    raise RuntimeError("_RM_RECURSION_RULES no longer matches _DESTRUCTIVE_SRC verbatim")

_REGENERABLE_TREE = re.compile(
    r"(?:"
    r"/(?:var/)?tmp/[^/]+(?:/[^/]+)*"          # /tmp/<x>…, /var/tmp/<x>… (never /tmp itself)
    r"|/var/cache/[^/]+(?:/[^/]+)*"            # /var/cache/apt, /var/cache/yum, …
    r"|(?:/[^/]+)*/\.cache(?:/[^/]+)*"         # …/.cache[/…] — pip, huggingface, torch, uv
    r"|(?:/[^/]+)*/__pycache__"
    r")/?"
)
# Only flags whose meaning is exhausted by "recursive" and "force". An unrecognized flag means the
# command does something this function did not reason about, so it keeps the destructive verdict.
_RM_KNOWN_FLAGS = re.compile(
    r"-{1,2}(?:[rRfdv]+|recursive|force|dir|verbose|one-file-system|preserve-root)")


def _is_regenerable_recursive_delete(seg: str) -> bool:
    """True only for `rm -rf <one absolute path under a regenerable tree>`. Fails closed."""
    toks = _strip_sudo(seg).split()
    if not toks or _basename(_unquote(toks[0])) != "rm":
        return False
    paths, end_of_flags = [], False
    for t in toks[1:]:
        if t == "--" and not end_of_flags:
            end_of_flags = True                           # POSIX end-of-options; the rest are paths
            continue
        if t.startswith("-") and not end_of_flags:
            if not _RM_KNOWN_FLAGS.fullmatch(t):
                return False
            continue
        paths.append(_unquote(t))
    if len(paths) != 1:
        return False                                      # the card must name exactly what dies
    target = paths[0]
    if _DANGEROUS_META.search(target):
        return False                                      # glob/brace/quote/`..` — radius unknown
    return bool(_REGENERABLE_TREE.fullmatch(target))

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
# expansion or newlines is banned. Parent traversal is rejected by every path-bearing reader and
# system-program check at the point where a token is actually interpreted as a path; treating every
# literal `..` as path syntax incorrectly cards valid non-path arguments such as journal priorities.
# Balanced quotes are allowed after raw
# command/variable expansion has been rejected; quoted grep patterns and paths are inert argv data.
_HARD_META = re.compile(r"""[;&`$<>(){}\[\]~]|\n""")
# Command substitution — denied on the RAW command even inside single quotes (see F14).
_SUBSTITUTION = re.compile(r"\$\(|`")
_VARIABLE_EXPANSION = re.compile(r"\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[^}]+\}|[0-9!#*@_-])")
# Candidates for stdin->stdout text filters; argument/option checks still apply below.
# Deliberately EXCLUDES awk/sed (system()/-i/w-file), xargs/tee (exec/write), dd.
# _program_words_are_data also consumes this set: adding a program requires reviewing ALL its
# argv-to-execution options there, not merely whether its ordinary stdin-filter form is read-only.
_SAFE_FILTERS = {"grep", "egrep", "fgrep", "head", "tail", "wc", "sort",
                 "uniq", "cut", "tr", "nl", "column", "rev", "cat", "tac"}
_OUTPUT_BUILTINS = {"printf", "echo", "true", "false"}
# Follow/stream flags — scoped to the binaries where -f means "stream" (NOT ps -f / lsblk -f).
_FOLLOW = re.compile(r"(?:^|\s)(-f|-F|--follow)(?:\s|$)")
# Per-binary continuous flags that loop forever (cluster-aware, e.g. `free -hs1`, `netstat -tlnpc`).
_STREAM_BLOCK = {
    "free": re.compile(r"(?:^|\s)(-[a-z]*s|--seconds)"),
    "netstat": re.compile(r"(?:^|\s)(-[a-z]*c|--continuous)"),
}

# nvidia-smi: query/read flags ONLY. NOT -r/-pl/-pm/-e/-l (mutate/stream).
_NVIDIA = re.compile(
    r"nvidia-smi(\s+(-q|-L|-a|--help|-i\s+\d+|-d\s+\w+|--id=\d+|--query[\w.-]*=\S+|--format=\S+|--display=\S+|topo(\s+-m)?))*"
)


def _bounded_nvidia_monitor(tokens) -> bool:
    """True for a dmon/pmon invocation with an explicit positive sample count.

    Both subcommands stream forever by default, but ``-c N`` makes them ordinary bounded
    diagnostics.  The command-shape gate has already rejected substitution and real-file
    redirection; these monitor subcommands do not expose GPU mutation flags.
    """
    if len(tokens) < 3 or tokens[1] not in ("dmon", "pmon"):
        return False
    counts = []
    for i, token in enumerate(tokens[2:]):
        if token == "-c" and i + 3 < len(tokens):
            counts.append(tokens[i + 3])
        elif token.startswith("--count="):
            counts.append(token.partition("=")[2])
    return len(counts) == 1 and counts[0].isdigit() and int(counts[0]) > 0

# Simple flag-only-safe diagnostics (no file CONTENT, no stream/mutate args). fullmatch.
_STRUCTURED_DIAG = [re.compile(p) for p in [
    r"systemctl\s+(status|is-active|is-enabled|is-failed|list-units|list-unit-files|list-dependencies)(\s+\S+)*",
    r"(free|uptime|uname|hostname|whoami|id|pwd|date|lscpu|lsblk|lsmod|lspci|nproc|arch|sensors)(\s+\S+)*",
    r"(pgrep|pidof)(\s+\S+)*",                         # process lookup only; pkill is separate
    r"pwdx\s+\d+",                                     # one process cwd; no write form
    r"pstree(\s+\S+)*",                                # process ancestry only
    r"lsof(\s+\S+)*",                                  # open-file/socket metadata; no write mode
    r"df(\s+\S+)*",
    r"findmnt(\s+\S+)*",
    r"mountpoint(\s+\S+)*",                           # query mount membership/device; no write form
    r"dmesg(\s+(-T|-x|-t|-H|-k|-r|-e|--ctime|--color=\w+|-l\s+\w+|-n\s+\d+|--level=\S+))*",
    r"(ss|netstat)(\s+\S+)*",
    r"ip\s+(-\w+\s+)*(addr|a|link|l|route|r|neigh|n)(\s+(show|list|s|l))?",
    r"getconf(\s+\S+)*",
    # Listing forms of binaries whose other invocations mutate. Anchored to listing flags only:
    # `swapon /swapfile` and
    # `swapon -a` still enable swap, `mount /dev/sda1 /mnt` still mounts, and all three stay
    # mutating because they cannot fullmatch. Bare `swapon` is deliberately NOT here — modern
    # util-linux lists, older versions did not, and the difference is not worth guessing at.
    r"swapon(\s+(-s|--summary|--show(=\S+)?|--noheadings|--raw|--bytes))+",
    r"mount(\s+(-l|--list))*",
    # Real version FLAGS only. A bare `version` is a positional script/source name for several of
    # these programs (`node version`, `cmake version`, compilers) and must not silently execute.
    r"(nvcc|python3?|pip3?|conda|gcc|g\+\+|cc|c\+\+|cmake|java|node|ruff|jupyter)\s+(--version|-V)",
    r"(docker|podman|nerdctl|git|go)\s+(--version|-V|version)",
    r"npm\s+(--version|-v)",
    r"npx\s+(--version|-v)",
    r"pip3?\s+(list|show|freeze)(\s+\S+)*",
    r"pip3?\s+cache\s+dir",
    r"conda\s+(list|info|env\s+list)(\s+\S+)*",
    r"(docker|podman|nerdctl)\s+(ps|images|info|version|stats\s+--no-stream)(\s+\S+)*",
    # POSIX `test` only evaluates file metadata/string/integer predicates and changes no state.
    # Shell expansion/substitution/redirection is rejected before this pattern, so allowing the
    # builtin itself removes confirmation cards from ordinary `test -r FILE && cat FILE` probes
    # without making an arbitrary operand executable.
    r"test(\s+\S+)*",
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
    r"sshd\s+-t",                                       # validate daemon config without applying it
    r"fuser(\s+\S+)*",                                  # kill forms are rejected before this proof
]]

_SYSTEMCTL_READ_VERBS = {
    "status", "is-active", "is-enabled", "is-failed", "is-system-running",
    "list-units", "list-unit-files", "list-dependencies", "list-jobs",
    "list-sockets", "list-timers",
}
_SYSTEMCTL_SAFE_PROPERTIES = {
    "ActiveEnterTimestamp", "ActiveState", "Description", "ExecMainCode", "ExecMainStatus",
    "FragmentPath", "Id", "InactiveEnterTimestamp", "LoadState", "MainPID", "Names",
    "NRestarts", "Restart", "SourcePath", "SubState", "UnitFileState", "Version",
}
_SYSTEMCTL_READ_FLAGS = {
    "--all", "-a", "--failed", "--full", "-l", "--no-ask-password",
    "--no-legend", "--no-pager", "--plain", "--quiet", "-q", "--reverse",
    "--show-types", "--system", "--user", "--value",
}
_SYSTEMCTL_READ_VALUE_FLAGS = {
    "--lines", "-n", "--output", "-o", "--property", "-p", "--state", "--type", "-t",
}
_SYSTEMCTL_READ_VALUE_PREFIXES = (
    "--lines=", "--output=", "--property=", "--state=", "--type=",
)


def _systemctl_is_readonly(tokens) -> bool:
    """Prove reporting-only systemctl forms, including options before the verb.

    `systemctl --no-pager --type=service --state=running` is the ordinary spelling of
    the implicit `list-units` read. The old regex required a verb in argv[1], so this
    live command and `is-system-running` both produced spurious confirmation cards.
    Unknown options and every non-reporting verb still fail closed.
    """
    i, verb, properties = 1, None, []
    while i < len(tokens):
        token = tokens[i]
        if token == "--":
            i += 1
            if i < len(tokens):
                verb = tokens[i]
            break
        if token in _SYSTEMCTL_READ_FLAGS:
            i += 1
            continue
        if token.startswith(_SYSTEMCTL_READ_VALUE_PREFIXES):
            if token.startswith("--property="):
                properties.extend(part for part in token.split("=", 1)[1].split(",") if part)
            i += 1
            continue
        if token in _SYSTEMCTL_READ_VALUE_FLAGS:
            if i + 1 >= len(tokens) or tokens[i + 1].startswith("-"):
                return False
            if token in ("--property", "-p"):
                properties.extend(part for part in tokens[i + 1].split(",") if part)
            i += 2
            continue
        if token.startswith("-"):
            return False
        verb = token
        i += 1
        break
    # No verb means systemctl's default list-units report. Unit operands are accepted only
    # after an explicit reviewed reporting verb; option parsing above has already rejected
    # mutating flags such as --now and --force.
    if verb == "show":
        # An unrestricted `systemctl show` includes Environment= and may expose credentials.
        # A caller-selected set of non-secret lifecycle properties is a bounded observation.
        while i < len(tokens):
            token = tokens[i]
            if token.startswith("--property="):
                properties.extend(part for part in token.split("=", 1)[1].split(",") if part)
                i += 1
                continue
            if token in ("--property", "-p"):
                if i + 1 >= len(tokens) or tokens[i + 1].startswith("-"):
                    return False
                properties.extend(part for part in tokens[i + 1].split(",") if part)
                i += 2
                continue
            if token in _SYSTEMCTL_READ_FLAGS:
                i += 1
                continue
            if token == "--":
                i += 1
                continue
            if token.startswith("-"):
                return False
            i += 1                                      # literal unit name
        return bool(properties) and all(prop in _SYSTEMCTL_SAFE_PROPERTIES for prop in properties)
    return verb is None or verb in _SYSTEMCTL_READ_VERBS


_AWK_BINARIES = {"awk", "gawk", "mawk", "nawk"}
_AWK_UNSAFE_PROGRAM = re.compile(
    r"(?:\bsystem\s*\(|\bgetline\b|\b(?:ARGV|ARGC|ENVIRON)\b|@[a-zA-Z_]"
    r"|\b(?:print|printf)\b[^{};\n]*(?:>{1,2}|(?<!\|)\|(?!\|))|\b(?:while|for|do)\b)",
    re.IGNORECASE,
)


def _awk_is_readonly(tokens, allow_stdin: bool) -> bool:
    """Prove the small awk form used to filter safe diagnostic text.

    Awk is otherwise an execution/write primitive. This accepts one inline program and
    either stdin or explicitly allowlisted files, while rejecting system/getline, dynamic
    ARGV/ENVIRON access, redirection/pipes, program files, assignments and loops. It is a
    structural proof, not an awk-name allowlist.
    """
    i = 1
    while i < len(tokens) and tokens[i].startswith("-"):
        token = tokens[i]
        if token == "-F":
            if i + 1 >= len(tokens):
                return False
            i += 2
            continue
        if token.startswith("-F") and len(token) > 2:
            i += 1
            continue
        return False
    if i >= len(tokens):
        return False
    program = tokens[i]
    if _AWK_UNSAFE_PROGRAM.search(program):
        return False
    paths = tokens[i + 1:]
    if not paths:
        return allow_stdin
    if any("=" in path or path.startswith("-") for path in paths):
        return False
    return all(path.startswith("/") and _safe_path(path) for path in paths)

# Readers that emit raw file CONTENT — strict: every target must be a curated-safe absolute path.
_CONTENT_READERS = {"cat", "nl", "tac", "strings", "od", "xxd", "hexdump",
                    "sort", "base64"}
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
    "/etc/timezone", "/etc/issue", "/etc/resolv.conf",
    "/entrypoint.sh",                                    # F10: container launch script
    # F8: mount/partition config — the core of data-disk "为什么 df 看不到我的盘" diagnosis. A
    # fresh cloud data disk is raw+unmounted, and a WRONG /etc/fstab entry is the classic reason a
    # mount silently fails on boot; both are diagnosed by reading these. fstab/mtab hold device
    # UUIDs + mount options, not user secrets (secret files still tripwire via _DENY_PATH_SUBSTR).
    "/etc/fstab", "/etc/mtab",
    "/etc/ssh/sshd_config",                             # server settings, not keys/credentials
    "/proc/meminfo", "/proc/cpuinfo", "/proc/loadavg", "/proc/uptime",
    "/proc/version", "/proc/stat", "/proc/diskstats", "/proc/mounts", "/proc/swaps",
    "/proc/partitions",                                  # F8: raw block-device table (name/size)
    "/proc/modules", "/proc/devices", "/proc/cmdline",
    # socket tables — the ss/netstat fallback for port-state diagnosis when neither tool is
    # installed (a minimal container). Hex addr/port/state/uid/inode; no user file content, no secret.
    "/proc/net/tcp", "/proc/net/tcp6", "/proc/net/udp", "/proc/net/udp6",
    "/proc/net/dev", "/proc/net/route",
    "/usr/local/cuda/version.txt", "/usr/local/cuda/version.json",
}
# Per-process diagnostic files. `environ` remains excluded and is available only through the
# name-allowlisted structured tool. `cmdline` is admitted because process launch arguments are a
# first-class service/GPU diagnostic and are already visible through the reviewed `ps ... args`
# surface. Output still passes through the same value scrubber before reaching the model.
_PROC_PID_SAFE = re.compile(
    r"^/proc/(?:\d+|self|thread-self)/(?:cgroup|status|mountinfo|limits|comm|maps|cmdline)$")
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
    """Strip balanced surrounding quotes so a quoted path is validated on its real value."""
    return p[1:-1] if len(p) >= 2 and p[0] == p[-1] and p[0] in "'\"" else p


# Metadata-only reads (ls/stat/file/readlink/wc/*sum; `du` is broader, see F5 below) reveal a
# name/size/perms/hash but never file CONTENT, so they are safe across the introspection tree —
# binaries, libs, device nodes, kernel / hardware state — which is far broader than the content-read
# allowlist. Deliberately EXCLUDES /etc, /var, /home, /root (a filename there can itself be
# sensitive; content reads stay narrow).
_META_SAFE_PREFIXES = ("/dev/", "/usr/", "/sys/", "/proc/", "/lib/", "/lib64/",
                       "/bin/", "/sbin/", "/opt/")
# F11: non-home application/data dirs, where these GPU images put the served app (/workspace/ComfyUI,
# /data/..., mounted volumes). Metadata-only, and deliberately NOT /root or /home — see _safe_meta_path.
_META_APP_PREFIXES = ("/workspace", "/data", "/mnt", "/model", "/root", "/home", "/tmp", "/var/tmp")
_META_STATE_PREFIXES = ("/var/lib/docker", "/var/lib/containerd")
_META_DENY_SUBSTR = tuple(d for d in _DENY_PATH_SUBSTR if d != "/root")

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


# Application logs are content-safe only under application directories, never /var/log. Output is
# still secret-scrubbed and secret-file tripwires still apply.
_APP_LOG_PREFIXES = ("/workspace/", "/data/", "/mnt/", "/model/", "/opt/", "/start.d/", "/usr/local/",
                     "/root/", "/home/")
_APP_LOG_SHAPE = re.compile(r"(?:\.(?:log|out|err)|(?:^|/)nohup\.out)$", re.I)


def _app_log_path(p: str) -> bool:
    if ".." in p or p.startswith("/var/log"):
        return False
    if any(d in p.lower() for d in _DU_DENY_SUBSTR):     # secret files still deny
        return False
    return bool(_APP_LOG_SHAPE.search(p)) and any(p.startswith(pre) for pre in _APP_LOG_PREFIXES)


_APP_SOURCE_PREFIXES = ("/workspace/", "/data/", "/mnt/", "/model/", "/opt/")


def _app_source_path(p: str) -> bool:
    """A caller-named Python/shell source file under an application tree.

    Traceback diagnosis needs the few lines around a reported failure, but opening the whole app
    data tree would send arbitrary user data to the model. Keep this to source shapes, reject glob
    expansion and every existing secret-file tripwire, and leave configs/data behind confirmation.
    """
    if ".." in p or re.search(r"[*?\[\]{}]", p):
        return False
    low = p.lower()
    if any(d in low for d in _DENY_PATH_SUBSTR):
        return False
    return (low.endswith((".py", ".sh")) and
            any(p.startswith(prefix) for prefix in _APP_SOURCE_PREFIXES))


def _safe_path(p: str) -> bool:
    p = _unquote(p)
    if ".." in p:
        return False
    if _app_log_path(p):                                 # F13: the service's own log
        return True
    if _app_source_path(p):                              # F15: caller-named operational source
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

    Application trees frequently live below /root or a login user's home on these images. Metadata
    readers reveal names, modes and sizes rather than file content, so those trees are included
    while credential/private-key component tripwires remain in force. Content still uses the
    narrower _safe_path policy apart from explicitly shaped source and log files."""
    p = _unquote(p)
    if _safe_path(p):
        return True
    if ".." in p:
        return False
    if any(d in p.lower() for d in _META_DENY_SUBSTR):
        return False
    if any(p == pre or p.startswith(pre + "/") for pre in _META_APP_PREFIXES):
        return True
    if any(p == pre or p.startswith(pre + "/") for pre in _META_STATE_PREFIXES):
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
_DU_USER_PREFIXES = ("/root", "/home", "/workspace", "/data", "/mnt",
                     "/var/lib/docker", "/var/lib/containerd")
_DU_DENY_SUBSTR = tuple(d for d in _DENY_PATH_SUBSTR if d != "/root")


# F12: a `du` walk rooted at `/` may descend ONE level (top-level dirs + their sizes). Deeper walks
# would enumerate subdirectory names inside home directories, which the policy forbids.
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
    # Deliberately not "any top-level dir": `du -sh /etc` remains refused, and that
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


def _literal_cd_ok(cmd: str) -> bool:
    """A literal ``cd`` changes only this short-lived SSH shell's working directory.

    Each tool call gets a fresh session, so the effect cannot persist on the guest.  Accept an
    optional ``--`` and one literal path (or no path for the remote user's home); substitutions are
    rejected by the outer shape gate before this helper is reached.
    """
    try:
        argv = shlex.split(cmd)
    except ValueError:
        return False
    if not argv or _basename(argv[0]) != "cd":
        return False
    rest = argv[1:]
    if rest[:1] == ["--"]:
        rest = rest[1:]
    if len(rest) > 1:
        return False
    return not rest or ("\x00" not in rest[0] and not rest[0].startswith("-")) or rest[0] == "-"


def _bounded_sleep_ok(cmd: str) -> bool:
    """Permit a bounded observation delay without mislabelling it as a guest mutation.

    The SSH call itself is capped at 25 seconds. Service warm-up probes commonly wait 6-15 seconds;
    asking for approval to spend time (while changing no guest state) is both misleading and noisy.
    Keep a margin for the actual verification command in the same call.
    """
    try:
        argv = shlex.split(cmd)
    except ValueError:
        return False
    if not argv or _basename(argv[0]) != "sleep":
        return False
    rest = argv[1:]
    if rest[:1] == ["--"]:
        rest = rest[1:]
    if len(rest) != 1 or re.fullmatch(r"(?:\d+(?:\.\d*)?|\.\d+)s?", rest[0]) is None:
        return False
    value = rest[0][:-1] if rest[0].endswith("s") else rest[0]
    return float(value) <= 20.0


# --- F10: loopback-only HTTP probe -------------------------------------------
# A loopback HTTP request distinguishes a dead service from one listening only locally.
#
# Scope is deliberately airtight so this can never exfiltrate or mutate:
#   - every non-flag argument MUST be a loopback URL (127.0.0.1 / localhost / ::1 / 0.0.0.0),
#   - flags are a closed allowlist; anything unknown DENIES (deny-by-default preserved),
#   - every flag that sends a body, uploads, overrides the HTTP method, attaches auth/data headers,
#     or writes a real file is banned; one literal Host header is allowed for reverse-proxy/Vite
#     virtual-host diagnosis (`-o` is permitted ONLY to /dev/null).
# So the worst case is a GET against a service already running on this box, whose response is then
# capped + secret-scrubbed like any other command output.
_LOOPBACK_URL = re.compile(
    r"^https?://(?:127(?:\.\d{1,3}){3}|localhost|\[::1\]|0\.0\.0\.0)(?::\d+)?(?:/[^\s]*)?$", re.I)
_PROBE_VALUE_FLAGS = {"-m", "--max-time", "--connect-timeout", "--max-redirs", "-o", "--output",
                      "-D", "--dump-header", "-H", "--header", "--timeout", "--tries", "-t",
                      "-w", "--write-out"}
_PROBE_BOOL_FLAGS = {"-s", "--silent", "-S", "--show-error", "-I", "--head", "-i", "--include",
                     "-L", "--location", "-f", "--fail", "-k", "--insecure", "-4", "-6",
                     "-q", "--quiet", "--spider", "-nv", "--no-verbose", "-O-", "-qO-",
                     "--server-response"}
# Short-flag letters that are boolean (take no value) — used to accept clusters like `-sS`.
_PROBE_BOOL_SHORT = set("sSIiLfk46q")
# Anything that can send data, upload, re-method, authenticate, or name an output file.
# case-SENSITIVE on purpose: CLI flags are, and `-O` (write file) must not be conflated with the
# permitted `-o /dev/null`. `-T` is banned outright — it is --upload-file in curl even though it is
# --timeout in wget, and the safe wget form (`--timeout=N`) is still available.
_PROBE_BANNED = re.compile(
    r"(?:^|\s)(?:-d|--data\S*|-F|--form\S*|-T|--upload-file|-X|--request|-u|--user|"
    r"-b|--cookie|-c|--cookie-jar|-K|--config|-e|--referer|-A|--user-agent|-O|--remote-name|"
    r"--output-document\S*|-P|--directory-prefix)(?:[=\s]|$)")


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
            if base in ("-D", "--dump-header") and val != "-":
                return False                             # response headers may go only to stdout
            if (base in ("-H", "--header") and
                    re.fullmatch(r"Host:\s*[A-Za-z0-9.-]+(?::\d+)?", val, re.I) is None):
                return False                             # only virtual-host selection, never auth/data
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


def _sed_numeric_print_is_read(cmd: str, allow_stdin: bool) -> bool:
    """Prove only ``sed -n 'N[,M]p' [SAFE_FILE...]``.

    GNU sed has writing/execution commands even without ``-i``. A fixed numeric print program has
    neither, so it is safe as a bounded stdin filter or against already-curated content paths.
    """
    try:
        argv = shlex.split(cmd)
    except ValueError:
        return False
    if not argv or _basename(argv[0]) != "sed":
        return False
    quiet, scripts, paths, i = False, [], [], 1
    while i < len(argv):
        token = argv[i]
        if token == "-n":
            quiet = True
        elif token == "-e":
            i += 1
            if i >= len(argv):
                return False
            scripts.append(argv[i])
        elif token.startswith("-"):
            return False
        elif not scripts:
            scripts.append(token)
        else:
            paths.append(token)
        i += 1
    if not quiet or len(scripts) != 1:
        return False
    if re.fullmatch(r"(?:\d+|\$)(?:,(?:\d+|\$))?p", scripts[0]) is None:
        return False
    if not paths:
        return allow_stdin
    return all("/" in path and _safe_path(path) for path in paths)


def _is_read_only(cmd: str) -> bool:
    tokens = cmd.split()
    if not tokens:
        return False
    binary = _basename(tokens[0])
    if binary == "cd":                                  # transient cwd inside this one SSH session
        return _literal_cd_ok(cmd)
    if binary == "sleep":                               # bounded wait before a verification probe
        return _bounded_sleep_ok(cmd)
    if binary == "[":                                   # shell test builtin spelling
        return len(tokens) >= 2 and tokens[-1] == "]"
    if binary == "printenv":                            # runtime-selection facts, not full secret env
        return len(tokens) > 1 and all(t in _DIAGNOSTIC_ENV_NAMES for t in tokens[1:])
    if binary in ("curl", "wget"):                       # F10: loopback-only service probe
        return _http_probe_ok(tokens)
    blk = _STREAM_BLOCK.get(binary)
    if blk and blk.search(cmd):
        return False
    if binary == "ps":
        return _ps_ok(tokens)
    if binary == "journalctl":
        return _journalctl_ok(cmd)
    if binary == "systemctl":
        try:
            return _systemctl_is_readonly(shlex.split(cmd))
        except ValueError:
            return False
    if binary in _AWK_BINARIES:
        try:
            return _awk_is_readonly(shlex.split(cmd), allow_stdin=False)
        except ValueError:
            return False
    if binary in ("tail", "head"):                 # content readers that can stream via -f
        if _FOLLOW.search(cmd):
            return False
        return _safe_content_read(tokens)
    if binary == "top":
        return _top_ok(cmd)
    if binary in ("vmstat", "iostat", "mpstat", "pidstat"):
        return _interval_ok(tokens)
    if binary == "nvidia-smi":
        return _NVIDIA.fullmatch(cmd) is not None or _bounded_nvidia_monitor(tokens)
    # Pure shell output/status builtins are diagnostics, not guest mutations. Real-file
    # redirection and substitution are rejected before this point, so these forms can only write
    # to the command's captured stdout/stderr or return a status code.
    if binary in _OUTPUT_BUILTINS:
        return True
    if binary == "fuser" and _KILL_FLAG_BINARIES["fuser"].search(cmd):
        return False
    if binary == "git":
        try:
            git_tokens = shlex.split(cmd)
            if _git_local_config_read(git_tokens) or _git_repository_read(git_tokens):
                return True
        except ValueError:
            pass
    if binary in ("grep", "egrep", "fgrep"):
        try:
            grep_tokens = shlex.split(cmd)
        except ValueError:
            return False
        return (_safe_grep_file_read(grep_tokens)
                or _recursive_grep_names_only(grep_tokens))
    if binary == "sed":
        return _sed_numeric_print_is_read(cmd, allow_stdin=False)
    # Match the allowlist on the BASENAME-normalized command as well: a venv/conda interpreter is
    # invoked by absolute path (`/usr/local/miniconda3/envs/comfyui/bin/python --version`), which is
    # exactly as read-only as the bare form. This is the ONLY route by which a path-qualified
    # `--version` reads as read_only — there is no help/version fast-path above any more (see
    # the tombstone at the top) — and it also covers forms carrying a redirect (`2>&1`) and the
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


def _safe_grep_file_read(tokens, allow_find_placeholder=False) -> bool:
    """Allow grep to read explicitly safe files, while preserving stdin-only pipeline handling.

    This deliberately supports only option forms that take no following value. More elaborate grep
    invocations remain available behind exact confirmation rather than being guessed safe.
    """
    no_value_options = {
        "-a", "--text", "-c", "--count", "-E", "--extended-regexp", "-F", "--fixed-strings",
        "-H", "--with-filename", "-h", "--no-filename", "-i", "--ignore-case", "-l",
        "--files-with-matches", "-L", "--files-without-match", "-n", "--line-number", "-q",
        "--quiet", "--silent", "-s", "--no-messages", "-v", "--invert-match", "-w",
        "--word-regexp", "-x", "--line-regexp",
    }
    positional = []
    for token in tokens[1:]:
        if token == "--":
            continue
        if token.startswith("-"):
            if token not in no_value_options and not re.fullmatch(r"-[acEFHhilLnoqsvwx]+", token):
                return False
            continue
        positional.append(token)
    if len(positional) < 2:
        return False
    files = positional[1:]
    return all((allow_find_placeholder and path == "{}") or
               ("/" in path and _safe_path(path)) for path in files)


def _grep_stdin_only(tokens) -> bool:
    """Prove that grep consumes its pattern plus stdin, never a second file operand.

    The old ``no token contains '/'`` shortcut confused a slash inside the PATTERN with a file and,
    worse, accepted a relative file operand that contained no slash. Parse the small option subset
    models actually use; one positional is the pattern, while ``-e`` supplies that pattern as an
    option value. Any additional positional is a file and therefore not an stdin-only filter.
    """
    no_value_options = {
        "-a", "--text", "-c", "--count", "-E", "--extended-regexp", "-F", "--fixed-strings",
        "-H", "--with-filename", "-h", "--no-filename", "-i", "--ignore-case", "-I",
        "-l", "--files-with-matches", "-L", "--files-without-match", "-n", "--line-number",
        "-q", "--quiet", "--silent", "-s", "--no-messages", "-v", "--invert-match", "-w",
        "--word-regexp", "-x", "--line-regexp",
    }
    positional, explicit_patterns, i = [], 0, 1
    while i < len(tokens):
        token = tokens[i]
        if token == "--":
            positional.extend(tokens[i + 1:])
            break
        if token in ("-e", "--regexp"):
            i += 1
            if i >= len(tokens):
                return False
            explicit_patterns += 1
        elif token.startswith("--regexp=") or (token.startswith("-e") and token != "-e"):
            explicit_patterns += 1
        elif token.startswith("-"):
            if token not in no_value_options and not re.fullmatch(r"-[acEFHIhilLnoqsvwx]+", token):
                return False
        else:
            positional.append(token)
        i += 1
    if explicit_patterns:
        return not positional
    return len(positional) == 1


def _recursive_grep_names_only(tokens) -> bool:
    """Allow a recursive application-tree search only when grep emits file names, not content."""
    recursive, names_only, positional = False, False, []
    allowed_long = {
        "--recursive", "--dereference-recursive", "--binary-files=without-match", "--text",
        "--files-with-matches", "--files-without-match", "--extended-regexp", "--fixed-strings",
        "--ignore-case", "--no-messages", "--silent", "--word-regexp", "--line-regexp",
    }
    allowed_cluster = set("rRIlLEFainsvwxHh")
    for token in tokens[1:]:
        if token == "--":
            continue
        if token.startswith("--"):
            if token not in allowed_long:
                return False
            recursive = recursive or token in ("--recursive", "--dereference-recursive")
            names_only = names_only or token in ("--files-with-matches", "--files-without-match")
            continue
        if token.startswith("-"):
            if len(token) < 2 or any(ch not in allowed_cluster for ch in token[1:]):
                return False
            recursive = recursive or "r" in token or "R" in token
            names_only = names_only or "l" in token or "L" in token
            continue
        positional.append(token)
    if not recursive or not names_only or len(positional) < 2:
        return False
    roots = positional[1:]
    return all(path.startswith("/") and _safe_meta_path(path) for path in roots)


def _git_local_config_read(tokens) -> bool:
    """Prove the read form used to inspect one repository-local Git config value."""
    i = 1
    if i < len(tokens) and tokens[i] == "-C":
        if i + 1 >= len(tokens) or not tokens[i + 1].startswith("/"):
            return False
        if not _safe_meta_path(tokens[i + 1]):
            return False
        i += 2
    if i >= len(tokens) or tokens[i] != "config":
        return False
    rest = tokens[i + 1:]
    if rest[:1] in (["--local"], ["--worktree"]):
        rest = rest[1:]
    if len(rest) != 2 or rest[0] not in ("--get", "--get-all", "--get-regexp"):
        return False
    return re.fullmatch(r"[A-Za-z0-9_.^$*+?()|\\-]+", rest[1]) is not None


def _git_repository_read(tokens) -> bool:
    """Prove a small metadata-only Git inspection inside an explicit application repository.

    Requiring ``-C /absolute/path`` keeps the target observable to the classifier. Remote URL
    queries, diffs/file content and every worktree-changing verb stay behind confirmation.
    """
    if len(tokens) < 4 or tokens[1] != "-C":
        return False
    repo = tokens[2]
    if not repo.startswith("/") or not _safe_meta_path(repo):
        return False
    verb, rest = tokens[3], tokens[4:]
    if verb == "status":
        allowed = {"-s", "--short", "-b", "--branch", "--porcelain", "--no-renames"}
        return all(token in allowed or token.startswith(("--porcelain=", "--untracked-files="))
                   for token in rest)
    if verb == "rev-parse":
        if not rest:
            return False
        allowed_flags = {"--show-toplevel", "--git-dir", "--is-inside-work-tree", "--is-bare-repository"}
        return all(token in allowed_flags or token == "HEAD" or
                   re.fullmatch(r"--short(?:=\d+)?", token) is not None for token in rest)
    if verb == "log":
        # Commit identity/subject only, bounded to one entry; no -p/--stat/name/content forms.
        bounded = any(token == "-1" or token == "--max-count=1" for token in rest)
        if not bounded:
            return False
        for token in rest:
            if token in {"-1", "--max-count=1", "--oneline", "--no-decorate",
                         "--no-show-signature", "HEAD"}:
                continue
            if token.startswith(("--format=", "--pretty=")):
                value = token.split("=", 1)[1]
                # Permit identity/subject/time placeholders and literal separators only. %b/%B
                # (commit body), %N (notes), signature payloads and unknown future placeholders
                # stay behind confirmation instead of contradicting the metadata-only contract.
                if "%" in re.sub(r"%(?:%|H|h|s|f|ct|cI|an)", "", value):
                    return False
                continue
            return False
        return True
    return False


def _is_safe_filter(seg: str) -> bool:
    """A downstream pipe stage: an allowlisted text filter reading ONLY stdin. No
    file-path args (so `| grep root /etc/shadow` cannot read a file) and no -f stream."""
    toks = seg.split()
    if not toks:
        return False
    if _basename(toks[0]) == "sed":
        return _sed_numeric_print_is_read(seg, allow_stdin=True)
    if _basename(toks[0]) in ("grep", "egrep", "fgrep"):
        try:
            return _grep_stdin_only(shlex.split(seg))
        except ValueError:
            return False
    if _basename(toks[0]) in _AWK_BINARIES:
        try:
            return _awk_is_readonly(shlex.split(seg), allow_stdin=True)
        except ValueError:
            return False
    if _basename(toks[0]) not in _SAFE_FILTERS:
        return False
    if _FOLLOW.search(seg):
        return False
    return not any("/" in t for t in toks[1:])


def _strip_safe_input_redirect(cmd: str):
    """Return a command with one proven read-only ``< ABS_PATH`` source removed.

    Input redirection does not change the guest, but treating every ``<`` as a write made common
    `/proc/<pid>/cmdline` inspection ask for a write confirmation. Keep the accepted shape small:
    one whitespace-delimited redirect on the first pipeline stage, at the end of that stage, from
    the same content-safe path surface used by ordinary file readers. The command consuming it must
    independently be a stdin-only text filter. Here-docs, fd duplication, relative/dynamic paths,
    later-stage redirects and additional redirects all fail closed.
    """
    masked = _mask_quoted(cmd)
    pipe_at = masked.find("|")
    source_end = len(cmd) if pipe_at < 0 else pipe_at
    source, suffix = cmd[:source_end], cmd[source_end:]
    try:
        tokens = shlex.split(source)
    except ValueError:
        return None
    # POSIX shells accept both `< /proc/PID/cmdline` and the equally common compact spelling
    # `</proc/PID/cmdline`.  shlex keeps the latter as one token, so split that token only when the
    # raw unquoted command proves there is exactly one compact absolute-path redirect.  A quoted
    # literal such as `printf '</tmp/x'` is blanked by _mask_quoted and cannot enter this branch.
    compact_redirects = re.findall(r"(?<!\S)<(?=/)", _mask_quoted(source))
    if "<" not in tokens and len(compact_redirects) == 1:
        compact_tokens = [i for i, token in enumerate(tokens) if token.startswith("</")]
        if len(compact_tokens) != 1:
            return None
        i = compact_tokens[0]
        tokens = tokens[:i] + ["<", tokens[i][1:]] + tokens[i + 1:]
    if "<" not in tokens:
        return (cmd, False) if "<" not in masked else None
    if tokens.count("<") != 1:
        return None
    at = tokens.index("<")
    if at == 0 or at + 2 != len(tokens):
        return None
    path = tokens[at + 1]
    if not path.startswith("/") or not _safe_path(path):
        return None
    source_without_redirect = shlex.join(tokens[:at])
    if not _is_safe_filter(source_without_redirect):
        return None
    if "<" in _mask_quoted(suffix):
        return None
    return source_without_redirect + suffix, True


def _outer_group_inner(source: str):
    """Return the body when one balanced ``(...)`` group wraps the entire source stage."""
    value = source.strip()
    masked = _mask_quoted(value)
    if len(masked) < 2 or masked[0] != "(" or masked[-1] != ")":
        return None
    depth = 0
    for i, ch in enumerate(masked):
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth < 0 or (depth == 0 and i != len(masked) - 1):
                return None
    return value[1:-1].strip() if depth == 0 else None


def _split_top_level_pipes(cmd: str):
    """Split real pipelines while keeping ``||`` fallbacks inside one outer group together."""
    masked = _mask_quoted(cmd)
    parts, start, depth, i = [], 0, 0, 0
    while i < len(masked):
        ch = masked[i]
        escaped = i > 0 and masked[i - 1] == "\\"
        if ch == "(" and not escaped:
            depth += 1
        elif ch == ")" and not escaped:
            depth -= 1
            if depth < 0:
                return None
        elif ch == "|" and depth == 0:
            if (i + 1 < len(masked) and masked[i + 1] == "|") or (i > 0 and masked[i - 1] == "|"):
                return None
            parts.append(cmd[start:i].strip())
            start = i + 1
        i += 1
    if depth != 0:
        return None
    parts.append(cmd[start:].strip())
    return parts


def _is_safe_readonly_command(cmd: str) -> bool:
    """True for a curated read-only command, INCLUDING a safe pipeline/glob. Strips
    stderr-to-null/fd redirects, rejects any hard-dangerous metachar, then requires the
    shape `<read-only source> [ | <text filter> ]*`. Globs (`*`/`?`) are allowed because
    the source's path allowlist (_safe_path) still validates the literal string, so a glob
    cannot escape a safe prefix (`/proc/driver/nvidia/*` stays inside nvidia driver info,
    while `/etc/*` or a `..` traversal is still denied). Balanced quotes are permitted after raw
    expansion checks; a `/`-bearing quoted token is still caught by
    the filter's path check, and a quoted secret path (`cat '/etc/shadow'`) still hits _safe_path."""
    stripped = _SAFE_REDIR.sub(" ", cmd)
    input_redirect = _strip_safe_input_redirect(stripped)
    if input_redirect is None:
        return False
    stripped, had_input_redirect = input_redirect
    try:
        shlex.split(stripped)
    except ValueError:                                    # malformed/unbalanced quoting -> fail closed
        return False
    # F14: scan for hard metachars on a copy whose quoted spans are blanked out. Expansion is
    # rejected below, so `grep 'comfy$'` / `grep "a|b"` are inert text — yet the flat
    # scan would refuse them. Masking preserves offsets, so pipe boundaries are located outside
    # quotes too. Path safety
    # is UNAFFECTED: segments are taken from the ORIGINAL text and every path token is still validated,
    # so `cat '/etc/shadow'` remains refused.
    masked = _mask_quoted(stripped)
    segs = _split_top_level_pipes(stripped)
    if segs is None or any(not s for s in segs):
        return False
    grouped_inner_for_scan = _outer_group_inner(segs[0])
    if grouped_inner_for_scan is not None:
        # The balanced outer pair is grouping syntax already proven by _outer_group_inner, not a
        # subshell hidden inside an argv. Blank only those two positions for the metachar scan.
        left = len(segs[0]) - len(segs[0].lstrip())
        right = len(segs[0].rstrip()) - 1
        chars = list(masked)
        chars[left], chars[right] = "x", "x"
        masked = "".join(chars)
    # Backslash-escaped parentheses are literal argv (not a subshell), most commonly find's grouped
    # predicates: `find ... \( -name a -o -name b \) | head`. Preserve string length for the pipe
    # offsets below while removing only those two proven-literal metacharacters from the hard scan.
    masked = re.sub(r"\\[()]", "xx", masked)
    if _HARD_META.search(masked):
        return False
    # Command substitution stays refused even when quoted. Inside single quotes `$(...)` is inert in a
    # correct shell, but this is the one construct where a quoting bug turns into arbitrary execution,
    # so it is also denied on the raw text.
    # A bare `$` anchor (`grep 'comfy$'`) carries no such risk and stays allowed.
    if _SUBSTITUTION.search(stripped):
        return False
    if _VARIABLE_EXPANSION.search(_mask_single_quoted(stripped)):
        return False
    first_tokens = segs[0].split()
    if (first_tokens and first_tokens[0] in _SHELL_KEYWORDS
            and first_tokens[0] not in _READ_CONTROL_KEYWORDS):
        return False                                      # control-flow/loops require confirmation
    source = _strip_sudo(_strip_shell_keywords(segs[0]).strip())
    # `find` needs its own recursive -exec proof rather than the flat allowlist. Preserve that proof
    # through stdin-only filters; splitting `find ... | head` first makes `head` look like a file
    # reader. Do not call the general segment classifier here: its fallback intentionally calls this
    # function and would recurse.
    source_tokens = source.split()
    source_is_find = bool(source_tokens) and _basename(source_tokens[0]) == "find"
    grouped_inner = _outer_group_inner(source)
    source_is_read_group = grouped_inner is not None and classify(grouped_inner) == "read_only"
    source_is_stdin_filter = had_input_redirect and _is_safe_filter(source)
    if (not source_is_stdin_filter and not source_is_read_group and not _is_read_only(source)
            and not (source_is_find and not _find_is_mutating(source, 0))):
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
# This is only used to word the refusal; it never grants execution. Accurate feedback lets the
# model split an unsupported command shape instead of retrying another equivalent wrapper.
# =============================================================================
# Tier 2: mutating — confirmation-gated. Anything not proven read-only lands here.
#
# The boundary is defined by effect rather than a curated command allowlist. Reads are allowed by
# default, and only these classes stay refused:
#   1. writes / state changes on the box
#   2. execution of arbitrary code (a `python -c` is an unbounded write primitive)
#   3. network egress off the box (exfil channel; loopback probes stay allowed)
#   4. commands that stream or block forever (they burn the entire turn budget)
#
# Secret-bearing reads (env, cloud-init logs, /proc/*/environ, `ps auxe`) are
# allowed by product policy: on this platform those are the user's
# own platform keys, and the instance password is already visible in the console,
# so reading them discloses nothing the requesting tenant cannot already see. The
# literal SSH credential is still stripped from output by scrub_output as defense
# in depth, and the destructive tier above is unchanged and still checked first.
# =============================================================================

# Listing-only forms of binaries that are otherwise writers. Checked BEFORE _MUTATING_BINARIES,
# the same shape the interpreters / curl / env carve-outs already use — set membership reads the
# first token only, so without this there is nowhere for "the form that just prints" to be said.
#
# Anchored to listing flags, and swapon requires at least one: `swapon /swapfile`, `swapon -a` and
# `mount /dev/sda1 /mnt` cannot fullmatch and stay mutating. Bare `swapon` is deliberately absent —
# modern util-linux lists, older versions did not, and that difference is not worth guessing at.
_LISTING_ONLY = {
    "swapon": re.compile(r"swapon(\s+(-s|--summary|--show(=\S+)?|--noheadings|--raw|--bytes))+"),
    "mount": re.compile(r"mount(\s+(-l|--list))*"),
}

# Binaries whose invocation changes the box (or holds the session open forever).
_MUTATING_BINARIES = {
    # Targeted deletes are mutating; narrowing the destructive patterns must not auto-run them.
    "rm", "unlink",
    # These write a file on every invocation; there is no read-only form to preserve.
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
# Wrapper prefixes: the command's effect belongs to the inner command, not the wrapper's name.
# The value is how many of the wrapper's OWN positionals precede the inner command once flags are
# consumed (`timeout 5 cmd` has one: the duration). Failing to find an inner command at all fails
# CLOSED, so `ionice -p 1234 -c 3` (which renices a running pid rather than running anything) lands
# in mutating rather than being waved through as an empty read.
_WRAPPER_BINARIES = {
    "nice": 0, "ionice": 0, "nohup": 0, "setsid": 0, "stdbuf": 0, "eatmydata": 0,
    "busybox": 0, "command": 0, "timeout": 1,
}

# A task-scoped repair grant is deliberately broad inside the selected guest, but it must not
# become authority over another host or over a cloud/cluster control plane.  Keep this boundary
# structural: downloads and package/source fetches remain normal guest-local mutations, while
# remote execution/upload, publishing and explicit control-plane clients are refused outright.
_CONTROL_PLANE_CLIENTS = {
    "aws", "az", "gcloud", "ucloud", "aliyun", "openstack",
    "terraform", "tofu", "pulumi", "kubectl", "oc", "helm",
}
_REMOTE_EXEC_OR_COPY = {"ssh", "scp", "sftp", "sshpass"}
_SHELL_CLIENTS = {"sh", "bash", "zsh", "ksh", "dash"}
_HTTP_WRITE_VALUE_FLAGS = {
    "--data", "--data-ascii", "--data-binary", "--data-raw", "--data-urlencode",
    "--form", "--form-string", "--upload-file", "--json",
    "--post-data", "--post-file", "--body-data", "--body-file",
}
_HTTP_READ_METHODS = {"GET", "HEAD"}
# Wrapper flags that consume a SEPARATE value token (`-n 5`, `-o L`, `-s KILL`, `-k 10`). Getting
# this list wrong cannot fail open: a value token mistaken for the inner command is itself
# classified, and a bare `5` or `L` is not a known binary, so it lands in mutating.
_WRAPPER_VALUE_FLAGS = {
    "-n", "-c", "-o", "-e", "-i", "-s", "-k", "-p", "-N", "-m",
    "--adjustment", "--class", "--classdata", "--pid", "--pgid", "--uid",
    "--input", "--output", "--error", "--signal", "--kill-after",
}
# `command -v/-V foo` only LOOKS UP foo (like `which`) rather than running it, and it is a common
# environment probe. Every other `command` form executes.
_COMMAND_LOOKUP = re.compile(r"(?:^|\s)-[vV](?:\s|$)")

# Binaries that read by default but take a destination path, so a write hides in a flag.
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

# A binary can read by default while a flag adds a side effect. fuser kill forms are mutating;
# Anchored to the kill flag itself, so the diagnostic forms this lane genuinely needs — `fuser
# 8188/tcp`, `fuser -v /workspace`, `fuser -n tcp 8188` — stay read_only. Short flags cluster
# (`-km`, `-ki`, `-k -9`), hence the character-class spelling rather than a bare `-k`. `fuser` has
# exactly one lowercase-k option, so this cannot catch a read flag by accident.
_KILL_FLAG_BINARIES = {
    "fuser": re.compile(r"(?:^|\s)(?:--kill\b|-[a-zA-Z]*k[a-zA-Z]*(?=\s|$))"),
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
_PYTHON_BINARY = re.compile(r"^python(?:\d+(?:\.\d+)*)?$")
_VERSION_ONLY = re.compile(r"^(--version|-V|--help)$")
# Executing a file on the box (a script, or anything invoked by relative path) is
# execution regardless of what it is named — this is what keeps an unknown binary from
# becoming an arbitrary write primitive now that the read allowlist is gone.
_SCRIPT_SHAPE = re.compile(r"\.(sh|bash|py|pl|rb|js|php|lua|ksh|zsh|run|bin|out)$", re.I)
# Explicit absolute paths auto-run only from the four system program
# directories; other paths require confirmation. A path-qualified Python probe
# may still run when its payload passes the normal read-only Python analysis.
# This rule judges path shape, not binary identity. Bare names continue through
# the ordinary command classifier.
_SYSTEM_PROGRAM_DIRS = ("/bin", "/sbin", "/usr/bin", "/usr/sbin")


def _is_system_program_path(raw0: str) -> bool:
    """True only for an absolute path whose directory IS one of the four system program dirs."""
    path = _unquote(raw0)
    if not path.startswith("/"):
        return False
    # normpath judges the real target rather than its spelling: `/usr/bin/../../tmp/x` is /tmp/x,
    # so traversal cannot turn a non-system executable into a trusted system-program path.
    return posixpath.dirname(posixpath.normpath(path)) in _SYSTEM_PROGRAM_DIRS
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
    # Cache-cleaning verbs still mutate the guest and therefore require confirmation.
    r"^(apt|apt-get|aptitude|yum|dnf|apk|pacman|zypper)\b.*\b(install|remove|purge|upgrade|update|autoremove|autoclean|clean)\b",
    r"^(pip|pip3|conda|mamba|npm|yarn|pnpm|cargo|gem|poetry|uv)\b.*\b(install|uninstall|remove|update|upgrade|add|sync|clean|purge)\b",
    # Deletes files per its tmpfiles.d configuration — the name reads like a dry-run and is not one.
    r"^systemd-tmpfiles\b.*--(clean|remove)\b",
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
    "importlib.util.find_spec",                          # package presence, no import/installation
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
    "getheader", "getheaders", "geturl", "getcode", "info",
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


def _py_import_call_is_read(call) -> bool:
    """Treat ``__import__('literal.module')`` like the already allowed import statement.

    Models commonly use this spelling inline when printing ``sys.executable``.  Only the
    one-argument literal form is accepted; computed names, fromlists and relative imports remain
    behind confirmation. Calls on the returned module are still checked independently, so
    ``__import__('os').system(...)`` remains mutating.
    """
    if len(call.args) != 1 or call.keywords:
        return False
    module = call.args[0]
    return (isinstance(module, ast.Constant) and isinstance(module.value, str)
            and re.fullmatch(r"[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*", module.value) is not None)


def _py_urlopen_is_local_get(call) -> bool:
    """Allow only urllib's loopback GET equivalent of the existing curl read probe.

    The image may not ship curl (a live platform image did not), while Python is the
    application's only dependable HTTP client. This remains a structural proof rather
    than trusting Python generally: the URL must be a literal loopback HTTP(S) URL,
    there is exactly one positional argument, and the only optional keyword is a
    timeout. A Request object, data/body, external or computed URL, redirect policy,
    TLS context, or any other shape stays behind the write confirmation gate.
    """
    if len(call.args) != 1:
        return False
    if any(kw.arg != "timeout" for kw in call.keywords):
        return False
    value = call.args[0]
    if not isinstance(value, ast.Constant) or not isinstance(value.value, str):
        return False
    try:
        parsed = urlsplit(value.value)
        port = parsed.port
    except ValueError:
        return False
    if parsed.scheme.lower() not in ("http", "https"):
        return False
    if parsed.username is not None or parsed.password is not None:
        return False
    if (parsed.hostname or "").lower() not in ("localhost", "127.0.0.1", "::1"):
        return False
    return port is None or 1 <= port <= 65535


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
            if dotted == "__import__":
                if not _py_import_call_is_read(node):
                    return False
                continue
            if dotted == "urllib.request.urlopen":
                if not _py_urlopen_is_local_get(node):
                    return False
                continue
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
    """True for Python version/help flags, `-c <provably read-only payload>` and
    `-m <read-only module>`. Other flags or an unprovable payload return False
    (so the caller refuses it). Fail closed on any parse error."""
    if not _PYTHON_BINARY.fullmatch(binary):
        return False
    try:
        argv = shlex.split(seg)
    except ValueError:
        return False
    if len(argv) > 1 and all(_VERSION_ONLY.fullmatch(arg) for arg in argv[1:]):
        return True
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
    i = 0
    while i < len(argv):
        if argv[i] in ("-exec", "-execdir"):
            inner, i = [], i + 1
            had_placeholder = False
            while i < len(argv) and argv[i] not in (";", "+"):
                if argv[i] == "{}":
                    had_placeholder = True
                else:
                    inner.append(argv[i])
                i += 1
            if not inner:
                return True                               # -exec with no command
            inner_str = " ".join(inner)
            inner_binary = _basename(_unquote(inner[0])).lower()
            grep_read = (had_placeholder and inner_binary in ("grep", "egrep", "fgrep") and
                         _safe_grep_file_read(inner + ["{}"], allow_find_placeholder=True))
            if any(p.search(inner_str) for p in _DESTRUCTIVE) or (not grep_read and
                                                                  _is_mutating_segment(inner_str, depth + 1)):
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


# Shell reserved words can precede a command and must be stripped before first-token classification.
#
# Unlike the wrapper binaries, these legitimately strip to NOTHING: `fi`, `done` and `esac` are real
# segments that change nothing, so an empty remainder is a READ here, not a fail-closed refusal.
_SHELL_KEYWORDS = {"if", "then", "else", "elif", "fi", "do", "done", "while", "until", "for",
                   "case", "esac", "in", "select", "function", "coproc", "time", "!",
                   "{", "}", "(", ")", "continue", "break"}
# These tokens only direct the current short-lived shell after every adjacent command has passed
# the same classifier. Treating brace groups and finite-loop flow as mutations made ordinary
# batched version/process checks ask for write approval without protecting any guest state.
_READ_CONTROL_KEYWORDS = {"if", "then", "else", "elif", "fi", "!", "{", "}",
                          "continue", "break"}
_DIAGNOSTIC_ENV_NAMES = {
    "CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES", "NVIDIA_DRIVER_CAPABILITIES",
    "CUDA_HOME", "CONDA_DEFAULT_ENV", "VIRTUAL_ENV", "PYTHONHOME", "PYTHONPATH",
    "LD_LIBRARY_PATH", "PATH",
}
_CHILD_DIAGNOSTIC_ENV_NAMES = {
    "CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES", "NVIDIA_DRIVER_CAPABILITIES",
    "OMP_NUM_THREADS", "LOCAL_RANK", "RANK", "WORLD_SIZE",
}
_ENV_ASSIGNMENT = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)=(.*)", re.DOTALL)


def _diagnostic_child_env_inner(seg: str):
    """Return an inner command after a proven child-only diagnostic environment prefix.

    CUDA visibility/rank variables alter only the launched process and cannot load code. PATH,
    PYTHONPATH and LD_LIBRARY_PATH are intentionally absent because they can select an executable,
    Python module or shared object. The returned command still passes through this same classifier.
    """
    try:
        tokens = shlex.split(seg)
    except ValueError:
        return None
    if not tokens:
        return None
    i, found = 0, False
    if _basename(tokens[0]).lower() == "env":
        i = 1
        while i < len(tokens):
            token = tokens[i]
            if token == "--":
                i += 1
                break
            if token in ("-u", "--unset"):
                if i + 1 >= len(tokens) or tokens[i + 1] not in _CHILD_DIAGNOSTIC_ENV_NAMES:
                    return None
                found, i = True, i + 2
                continue
            if token.startswith("--unset="):
                if token.split("=", 1)[1] not in _CHILD_DIAGNOSTIC_ENV_NAMES:
                    return None
                found, i = True, i + 1
                continue
            match = _ENV_ASSIGNMENT.fullmatch(token)
            if match:
                if match.group(1) not in _CHILD_DIAGNOSTIC_ENV_NAMES:
                    return None
                found, i = True, i + 1
                continue
            if token.startswith("-"):
                return None
            break
    else:
        while i < len(tokens):
            match = _ENV_ASSIGNMENT.fullmatch(tokens[i])
            if not match:
                break
            if match.group(1) not in _CHILD_DIAGNOSTIC_ENV_NAMES:
                return None
            found, i = True, i + 1
    if not found or i >= len(tokens):
        return None
    return shlex.join(tokens[i:])


def _strip_shell_keywords(seg: str) -> str:
    toks = seg.split()
    i = 0
    while i < len(toks) and toks[i] in _SHELL_KEYWORDS:
        i += 1
    return " ".join(toks[i:])


def _strip_subshell_edges(seg: str) -> str:
    """Peel grouping punctuation left on segments after quote-aware &&/||/; splitting.

    A grouped fallback such as ``(netstat ... || lsof ... || true)`` is read-only exactly when
    every command inside it is read-only. classify() already checks every resulting segment;
    unmatched group edges should not turn those commands into fake executable names. Command
    substitution is rejected before this helper is reached.
    """
    value = seg.strip()
    while value.startswith("("):
        value = value[1:].lstrip()
    while value.endswith(")"):
        value = value[:-1].rstrip()
    return value


def _strip_wrapper(binary: str, tokens) -> str:
    """Return the inner command of a wrapper invocation, or "" when there is none.

    Consumes the wrapper's own flags (skipping a separate value for the flags in
    _WRAPPER_VALUE_FLAGS) and then the fixed number of its own positionals, leaving the rest.
    """
    i = 1
    while i < len(tokens) and tokens[i].startswith("-"):
        i += 2 if tokens[i] in _WRAPPER_VALUE_FLAGS else 1
    i += _WRAPPER_BINARIES[binary]
    # Re-quote argv when an inner shell receives one command-string argument. Plain joining loses
    # that boundary (`busybox sh -ec 'curl -d ...'` becomes several unrelated argv words) and lets
    # both the effect classifier and the cross-guest scanner inspect a different command.
    return shlex.join(tokens[i:]) if i < len(tokens) else ""


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


def _is_guest_loopback_url(raw: str) -> bool:
    """True only when an explicit HTTP target stays inside the selected guest."""
    try:
        parsed = urlsplit(raw)
        port = parsed.port
    except ValueError:
        return False
    return (parsed.scheme.lower() in ("http", "https")
            and parsed.username is None and parsed.password is None
            and (parsed.hostname or "").lower() in ("localhost", "127.0.0.1", "::1", "0.0.0.0")
            and (port is None or 1 <= port <= 65535))


def _http_crosses_guest_boundary(tokens) -> bool:
    """Refuse an HTTP write unless every explicit target is guest loopback.

    External GET/HEAD downloads are intentionally allowed by the task-scope grant.  A body,
    upload, or unsafe method is different: it sends tenant data or changes another system.  An
    unresolved target fails closed because its destination cannot be proven guest-local.
    """
    binary = _basename(tokens[0]).lower() if tokens else ""
    effect, urls, i = False, [], 1
    while i < len(tokens):
        token = tokens[i]
        flag, has_eq, inline = token.partition("=")
        lower = flag.lower()
        if ((binary == "curl" and flag.startswith("-K"))
                or (binary == "wget" and flag.startswith("-e"))
                or lower in ("--config", "--execute")):
            return True                                  # config can hide target, body and method
        if lower in ("-x", "--request", "--method"):
            method = inline if has_eq else (tokens[i + 1] if i + 1 < len(tokens) else "")
            effect = effect or method.upper() not in _HTTP_READ_METHODS
            i += 1 if has_eq else 2
            continue
        if lower.startswith("-x") and lower != "-x":
            effect = effect or token[2:].upper() not in _HTTP_READ_METHODS
            i += 1
            continue
        value_flag = ((binary == "curl" and flag in ("-d", "-F", "-T"))
                      or lower in _HTTP_WRITE_VALUE_FLAGS
                      or lower.startswith("--data-") or lower.startswith("--form-")
                      or lower.startswith("--post-") or lower.startswith("--body-"))
        if value_flag:
            effect = True
            i += 1 if has_eq else 2
            continue
        if (binary == "curl" and any(token.startswith(prefix) and token != prefix
                                     for prefix in ("-d", "-F", "-T"))):
            effect = True
            i += 1
            continue
        if token.lower().startswith(("http://", "https://")):
            urls.append(token)
        i += 1
    return effect and (not urls or any(not _is_guest_loopback_url(url) for url in urls))


def _git_subcommand(tokens):
    """Return git's first command verb while skipping common global options."""
    i = 1
    value_flags = {"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path"}
    while i < len(tokens):
        token = tokens[i]
        flag = token.split("=", 1)[0]
        if flag in value_flags:
            i += 1 if "=" in token else 2
            continue
        if token.startswith("-"):
            i += 1
            continue
        return token.lower()
    return ""


def _rsync_has_remote(tokens) -> bool:
    for token in tokens[1:]:
        if token.startswith("-"):
            continue
        if token.lower().startswith("rsync://"):
            return True
        if re.match(r"^(?:[^/@:\s]+@)?[^/:\s]+:.+", token):
            return True
    return False


def _segment_crosses_guest_boundary(seg: str, depth: int = 0) -> bool:
    """Detect effects outside the selected guest before task-scope auto-approval."""
    if depth > 4:
        return True
    seg = _strip_subshell_edges(_strip_sudo(_strip_shell_keywords(seg).strip()).strip())
    try:
        tokens = shlex.split(seg)
    except ValueError:
        return False                                      # normal form/effect gates still fail closed
    if not tokens:
        return False
    while tokens and _ENV_ASSIGNMENT.fullmatch(tokens[0]):
        tokens.pop(0)
    if not tokens:
        return False
    binary = _basename(tokens[0]).lower()
    if binary == "env":
        i = 1
        while i < len(tokens):
            token = tokens[i]
            if token == "--":
                i += 1
                break
            if token in ("-S", "--split-string"):
                if i + 1 >= len(tokens):
                    return False
                inner = tokens[i + 1]
                if i + 2 < len(tokens):
                    inner += " " + shlex.join(tokens[i + 2:])
                return _scan_guest_boundary(inner, depth + 1)
            if token.startswith("-S") and token != "-S":
                inner = token[2:]
                if i + 1 < len(tokens):
                    inner += " " + shlex.join(tokens[i + 1:])
                return _scan_guest_boundary(inner, depth + 1)
            if token.startswith("--split-string="):
                inner = token.split("=", 1)[1]
                if i + 1 < len(tokens):
                    inner += " " + shlex.join(tokens[i + 1:])
                return _scan_guest_boundary(inner, depth + 1)
            if token in ("-u", "--unset", "-C", "--chdir", "-a", "--argv0"):
                i += 2
                continue
            if token.startswith(("--unset=", "--chdir=", "--argv0=")):
                i += 1
                continue
            if token in ("-i", "--ignore-environment", "-0", "--null", "--debug"):
                i += 1
                continue
            if token.startswith("-") or _ENV_ASSIGNMENT.fullmatch(token):
                i += 1
                continue
            break
        return i < len(tokens) and _segment_crosses_guest_boundary(shlex.join(tokens[i:]), depth + 1)
    if binary in _WRAPPER_BINARIES:
        inner = _strip_wrapper(binary, tokens)
        # A malformed/no-inner wrapper is not evidence of a boundary crossing; the ordinary
        # classifier still sends it to the mutating tier and therefore fails closed on execution.
        return bool(inner) and _segment_crosses_guest_boundary(inner, depth + 1)
    if binary in _SHELL_CLIENTS:
        for pos, token in enumerate(tokens[1:], 1):
            if token == "-c" or (re.fullmatch(r"-[A-Za-z]+", token) and "c" in token[1:]):
                return pos + 1 >= len(tokens) or _scan_guest_boundary(tokens[pos + 1], depth + 1)
    if binary in _CONTROL_PLANE_CLIENTS or binary in _REMOTE_EXEC_OR_COPY:
        return True
    if binary == "rsync" and _rsync_has_remote(tokens):
        return True
    if binary == "git" and _git_subcommand(tokens) == "push":
        return True
    if binary in ("docker", "podman", "nerdctl") and any(t.lower() == "push" for t in tokens[1:]):
        return True
    if binary in ("npm", "cargo", "twine") and any(t.lower() in ("publish", "upload") for t in tokens[1:]):
        return True
    if binary in ("curl", "wget") and _http_crosses_guest_boundary(tokens):
        return True
    return False


def _scan_guest_boundary(command: str, depth: int = 0) -> bool:
    """True when any command crosses from the selected guest into another authority domain."""
    for line in (command or "").split("\n"):
        for seg in _split_chain(line):
            if _segment_crosses_guest_boundary(seg, depth):
                return True
    return False


def _is_mutating_segment(seg: str, _depth: int = 0) -> bool:
    """True unless this ONE command is positively proven read-only.

    Explicit writers, arbitrary execution, egress and blocking forms are caught first; unknown
    commands then reach the confirmation tier. `_depth` bounds recursive wrapper/find analysis."""
    # Strip everything that can merely PRECEDE the real command, to a fixed point: `then sudo rm ...`
    # needs both strippers, and either one alone leaves the other's prefix in token 0.
    seg, prev = seg.strip(), None
    if _SUBSTITUTION.search(seg):                         # reject before peeling grouping parens
        return True
    seg = _strip_subshell_edges(seg)
    initial_tokens = seg.split()
    if (initial_tokens and initial_tokens[0] in _SHELL_KEYWORDS
            and initial_tokens[0] not in _READ_CONTROL_KEYWORDS):
        return True                                       # do not auto-run shell control-flow/loops
    while prev != seg:
        prev = seg
        seg = _strip_sudo(_strip_shell_keywords(seg).strip())
    if not seg:
        return not (initial_tokens and initial_tokens[0] in _READ_CONTROL_KEYWORDS)
    if ">" in _SAFE_REDIR.sub(" ", seg):                  # real-file redirection writes
        return True
    # `2>/dev/null` / `2>&1` / `>/dev/null` change nothing, but they are bare WORDS to a naive
    # tokenizer: `env 2>/dev/null` read as the executing form `env <cmd>` and was refused live.
    # Drop them before any token-position rule looks at the segment.
    seg = _SAFE_REDIR.sub(" ", seg).strip()
    child_env_inner = _diagnostic_child_env_inner(seg)
    if child_env_inner is not None:
        return (_depth > 3 or
                _is_mutating_segment(child_env_inner, _depth + 1))
    tokens = seg.split()
    if not tokens:
        return False
    raw0 = _unquote(tokens[0])
    binary = _basename(raw0).lower()
    if binary == "[":
        return not (len(tokens) >= 2 and tokens[-1] == "]")  # shell test; no state change
    if binary in _OUTPUT_BUILTINS:
        return False                                      # captured output/status, no real-file redirect
    # running a file on the box: a script by name, or anything by relative path
    if _SCRIPT_SHAPE.search(binary) or raw0.startswith("./") or raw0.startswith("../"):
        return True
    # A path-qualified Python/Conda probe is judged by the SAME structural proof as bare Python.
    # The platform images put their real application interpreter under /opt or /usr/local; making
    # the spelling alone win before this proof forced a confirmation for torch.cuda.is_available()
    # and made read-only runs infer CUDA health from files instead of measuring it. This exception
    # is deliberately Python-only and payload-proven: /tmp/x and /opt/app/bin/run still card below,
    # while os.system/open(..., 'w')/subprocess remain mutating through the AST gate. It does NOT
    # attest the interpreter binary itself; accepting `/tmp/python` and `/opt/venv/bin/python`
    # equally is the explicit capability tradeoff for diagnosis inside the customer's own guest.
    if ("/" in raw0 and _PYTHON_BINARY.fullmatch(binary)
            and _is_readonly_py_invocation(binary, seg)):
        return False
    # Any other non-system absolute program still asks first. Outside the Python exception above,
    # this judges the PATH rather than a claimed basename: naming an arbitrary payload
    # `/tmp/nvidia-smi` does not make it an unattended diagnostic.
    if "/" in raw0 and not _is_system_program_path(raw0):
        return True
    if binary in _WRAPPER_BINARIES:                       # the effect is the INNER command's
        return _wrapper_is_mutating(binary, tokens, _depth)
    listing = _LISTING_ONLY.get(binary)
    if listing and listing.fullmatch(seg):
        return False                                      # the form that only prints — see _LISTING_ONLY
    if binary in _MUTATING_BINARIES or binary in _EXEC_BINARIES or binary in _NET_BINARIES:
        return True
    ofw = _OUTPUT_FLAG_WRITERS.get(binary)                # reader whose write hides in a flag
    if ofw and ofw.search(seg):
        return True
    kfb = _KILL_FLAG_BINARIES.get(binary)                 # reader whose KILL hides in a flag
    if kfb and kfb.search(seg):
        return True
    # Versioned Python names (`python3.12`) must enter the same structural gate. Keeping only the
    # historical python/python2/python3 literals here would make a system-path python3.12 `-m`
    # invocation skip both the non-system-path card above and the module proof below, then fall
    # through as read_only.
    if binary in _INTERPRETERS or _PYTHON_BINARY.fullmatch(binary):
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
        return True                                       # environment values may contain credentials
    if binary == "top":                                   # needs BOTH batch mode and an iteration cap
        return not (re.search(r"(?:^|\s)-\w*b", seg) and re.search(r"-\w*n\s*\d+", seg))
    if binary == "nvidia-smi" and re.search(r"(?:^|\s)(dmon|pmon|-l\b|--loop)", seg):
        return not _bounded_nvidia_monitor(tokens)         # bounded dmon/pmon return; loops do not
    # `-f` means FOLLOW only on log readers; on ps/lsblk/df it means full-format/filesystem,
    # and treating it as streaming wrongly refused plain `ps -f` (seen live).
    if binary in ("tail", "head", "journalctl", "logread", "dmesg") and _FOLLOW.search(seg):
        return True
    # Reads that block forever or stream a raw block device. Scoped to readers that dump raw
    # CONTENT: `cat /dev/sda` streams the whole disk, but `blkid /dev/vdb` / `fdisk -l` /
    # `smartctl -a` only queries metadata and is a core disk diagnostic.
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
    # _MUTATING_FORMS is ^-anchored so a PATH that merely contains the verb (`cat
    # /opt/pip-install.log`) cannot trip it. That anchor is worth keeping, but it also made every
    # one of those rules depend on HOW the binary was spelled: `/usr/bin/pip3 install X` and
    # `/usr/bin/systemctl restart X` missed the anchor and fell through to read_only — no consent
    # card — while the table-driven writers were unaffected, because those are looked up by
    # basename (`/bin/rm`, `/usr/bin/chmod`, `/usr/bin/tee` all classify correctly). The
    # destructive tier was never exposed either: it matches on \b, so `/usr/bin/systemctl restart
    # sshd` is still refused. Same asymmetry as the wrapper-binary and shell-keyword holes, in a
    # fifth spelling — two matching mechanisms in one file, and only the position-dependent one is
    # fragile. Re-spelling token 0 as its basename keeps the anchor's protection and drops its
    # dependence on the invocation. ORed, never substituted, so this can only ADD matches.
    # Normalize path-qualified AND quoted executable spellings. Comparing against `raw0` missed
    # `"npm" --version`: raw0 had already been unquoted, so the code incorrectly kept the quoted
    # segment and failed the reviewed version-only form inside an otherwise proven literal loop.
    respelled = " ".join([binary] + tokens[1:]) if tokens[0] != binary else seg
    if any(p.search(seg) or p.search(respelled) for p in _MUTATING_FORMS):
        return True
    # Auto-run is allowlisted, not inferred from the absence of a known writer.
    # This is the critical boundary for bare commands such as `redis-cli
    # FLUSHALL`, `npm run build`, or a future executable the classifier has
    # never seen: all remain available after an exact confirmation, but none can
    # change the guest without one.
    return not (_is_safe_readonly_command(seg) or _is_safe_readonly_command(respelled))


# Chaining is ACCEPTED now: each segment is classified independently and every one of
# them must be a read. This removes the single largest source of refusals seen live
# (the model naturally writes `ls /a; ls /b`) without widening what any one command
# may do — and it lets the refusal message stop lying about "this changes the box".
_CHAIN_SPLIT = re.compile(r"\|\||&&|[;|\n]")
_CONDITIONAL_SPLIT = re.compile(r"\|\||&&|[;\n]")


def _split_at(cmd: str, pattern):
    """Split on quote-aware shell boundaries selected by ``pattern``.

    Boundaries are located on a QUOTE-MASKED copy, then sliced out of the original, so a
    metachar inside quoted DATA is not a boundary. `grep -E 'nginx|caddy|socat|proxy' f` is ONE
    command whose pattern merely contains `|`; splitting it raw manufactured phantom segments and
    the middle alternative `socat` (a net binary name) refused the whole command — seen live.
    """
    masked = _mask_quoted(cmd)
    parts, last = [], 0
    for m in pattern.finditer(masked):
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


def _split_chain(cmd: str):
    """Split into individual commands on `;` `|` `||` `&&`."""
    return _split_at(cmd, _CHAIN_SPLIT)


def _split_conditionals(cmd: str):
    """Split top-level control flow while preserving pipelines and grouped fallbacks."""
    masked = _mask_quoted(cmd)
    parts, last, depth, i = [], 0, 0, 0
    while i < len(masked):
        ch = masked[i]
        bs, j = 0, i - 1
        while j >= 0 and masked[j] == "\\":
            bs, j = bs + 1, j - 1
        escaped = bs % 2 == 1
        if ch == "(" and not escaped:
            depth += 1
        elif ch == ")" and not escaped:
            depth = max(0, depth - 1)
        if depth == 0 and not escaped:
            width = 2 if masked.startswith(("||", "&&"), i) else (1 if ch in ";\n" else 0)
            if width:
                parts.append(cmd[last:i])
                last, i = i + width, i + width
                continue
        i += 1
    parts.append(cmd[last:])
    return [s for s in (part.strip() for part in parts) if s]


_LITERAL_FOR_LOOP = re.compile(
    r"(?P<prefix>^|[;\n])(?P<space>\s*)for\s+(?P<var>[A-Za-z_][A-Za-z0-9_]*)\s+in\s+"
    r"(?P<values>[^;\n]+?)\s*;\s*do\s+(?P<body>.*?)\s*;\s*done\b", re.DOTALL)
_LITERAL_LOOP_VALUE = re.compile(r"[A-Za-z0-9_./:@+\-]+")


def _expand_loop_var(body: str, variable: str, value: str) -> str:
    """Expand one shell loop variable outside single quotes; values are prevalidated literals."""
    out, quote, i = [], "", 0
    braced, plain = "${" + variable + "}", "$" + variable
    while i < len(body):
        ch = body[i]
        if ch == "'":
            quote = "" if quote == "'" else ("'" if not quote else quote)
            out.append(ch)
            i += 1
            continue
        if ch == '"':
            quote = "" if quote == '"' else ('"' if not quote else quote)
            out.append(ch)
            i += 1
            continue
        if quote != "'" and body.startswith(braced, i):
            out.append(value)
            i += len(braced)
            continue
        if (quote != "'" and body.startswith(plain, i)
                and (i + len(plain) == len(body)
                     or not (body[i + len(plain)].isalnum() or body[i + len(plain)] == "_"))):
            out.append(value)
            i += len(plain)
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def _rewrite_proven_literal_for_loop(command: str):
    """Validate one finite literal for-loop and replace it with ``true`` for outer classification.

    Models naturally batch the same read across a short path/interpreter list. Each literal value is
    substituted and the body is reclassified by this same gate; any write, nested loop, dynamic
    variable or unknown command makes the entire loop require confirmation.
    """
    masked = _mask_quoted(command)
    match = _LITERAL_FOR_LOOP.search(masked)
    if match is None:
        return False, False, command
    values_text = command[match.start("values"):match.end("values")]
    body = command[match.start("body"):match.end("body")]
    try:
        values = shlex.split(values_text)
    except ValueError:
        return True, False, command
    if (not values or len(values) > 32
            or any(_LITERAL_LOOP_VALUE.fullmatch(value) is None for value in values)
            or re.search(r"\b(?:for|while|until|select|case)\b", _mask_quoted(body))):
        return True, False, command
    variable = match.group("var")
    for value in values:
        expanded = _expand_loop_var(body, variable, value)
        if _VARIABLE_EXPANSION.search(expanded) or classify(expanded) != "read_only":
            return True, False, command
    prefix = command[match.start("prefix"):match.end("prefix")]
    rewritten = command[:match.start()] + prefix + " true" + command[match.end():]
    return True, True, rewritten


def is_form_violation(command: str) -> bool:
    """Refused for SHAPE rather than effect. Now only command substitution, since
    chaining and pipes are accepted. Used solely to word the refusal message."""
    return bool(_SUBSTITUTION.search(command or ""))


_MULTI_SLASH = re.compile(r"/{2,}")


def _normalize_paths(cmd: str) -> str:
    """Rewrite every absolute-path-looking token to its canonical spelling.

    The destructive tier contains PATH rules (`/boot`, `/etc/fstab`, `/var/lib`, the system-path
    `rm`), and a regex over the raw string reads `//etc/fstab` and `/tmp/../etc/fstab` as different
    paths from `/etc/fstab` while the kernel does not.

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


def _program_words_are_data(cmd: str) -> bool:
    """Only known data consumers may suppress program-name matches in their arguments.

    This does not grant read-only status or suppress effect/path rules. Unknown execution
    consumers anywhere in the command retain the raw scan, including pipes into a shell.
    """
    # Reuse the existing splitter; do not extend its shell grammar. Quoted data escapes such as
    # printf's '\n' do not affect boundaries. Escaped quotes, comments and unquoted escapes can.
    if (_SUBSTITUTION.search(cmd) or "${" in cmd or "$[" in cmd
            or re.search(r"\\['\"]", cmd)):
        return False
    try:
        shlex.split(cmd)
    except ValueError:
        return False
    masked = _mask_quoted(cmd)
    # Keep real-file writes behind the raw gate; only reuse the existing null/FD redirections.
    if re.search(r"[#&<>(){}\\]", _SAFE_REDIR.sub(" ", masked).replace("&&", "")):
        return False
    match = _LITERAL_FOR_LOOP.fullmatch(masked)
    body = cmd
    if match is not None:
        values = cmd[match.start("values"):match.end("values")]
        if re.search(r"[$|]", values):
            return False
        body = cmd[match.start("body"):match.end("body")]
    segments = _split_chain(body)
    if not segments:
        return False
    for seg in segments:
        try:
            tokens = shlex.split(seg)
        except ValueError:
            return False
        if not tokens:
            return False
        program = tokens[0]
        binary = _basename(program)
        if "/" in program and posixpath.dirname(program) not in _SYSTEM_PROGRAM_DIRS:
            return False
        if binary == "printf":
            args = tokens[1:]
            if args and args[0] == "--":
                args = args[1:]
            # Only literal output conversions; -v, %n and computed formats can assign shell
            # variables whose array subscripts execute arithmetic. Never infer their effects.
            if (not args or args[0].startswith("-") or "$" in args[0]
                    or re.fullmatch(r"(?:[^%]|%[-+ #0-9.*]*[diouxXfFeEgGaAcbsqQ%])*", args[0]) is None):
                return False
            continue
        if not (binary in _OUTPUT_BUILTINS or
                (binary in _SAFE_FILTERS and binary not in _OUTPUT_FLAG_WRITERS) or
                (binary not in {"[", "sort"} and not _PYTHON_BINARY.fullmatch(binary)
                 and _is_read_only(seg))):
            return False
    return True


def _scan_destructive(cmd: str) -> bool:
    """Match the destructive tier PER COMMAND — never across a `;` / `|` / newline boundary.

    Most rules have the shape "write-verb ... sensitive-path". Scanning a whole chain could pair
    the verb from one command with another command's path, while weakening `^`/`$` anchors.
    _split_chain respects quoted separators and newlines. Bare `&` is intentionally not a boundary
    because splitting it would corrupt `2>&1` and `&>` redirections.
    """
    program_words_are_data = _program_words_are_data(cmd)
    for line in cmd.split("\n"):
        for seg in _split_chain(line):
            norm = _normalize_paths(seg)
            # The regenerable-tree exemption is decided on BOTH spellings and only suppresses the
            # recursion rules when both agree, so a path that normalizes into or out of an exempt
            # tree cannot pick up the exemption on one spelling and dodge the rules on the other.
            pats = _DESTRUCTIVE
            if (_is_regenerable_recursive_delete(seg)
                    and (norm == seg or _is_regenerable_recursive_delete(norm))):
                pats = _DESTRUCTIVE_NO_RM_RECURSION
            if program_words_are_data:
                pats = [p for p in pats if p.pattern not in _DESTRUCTIVE_PROGRAM_WORD_SRC]
            if any(p.search(seg) for p in pats):
                return True
            if norm != seg and any(p.search(norm) for p in pats):
                return True
    return False


def classify(command: str) -> str:
    """Return 'destructive' | 'read_only' | 'mutating'. Reasoning-blind: command text only.

    ``destructive`` also carries the product's hard-refused cross-guest/control-plane boundary;
    both classes are effects the task-scoped repair grant can never authorize.
    """
    cmd = (command or "").strip()
    if not cmd:
        return "mutating"
    if cmd in _SHELL_KEYWORDS:                            # bare syntax is not a useful remote probe
        return "mutating"
    if _scan_guest_boundary(cmd) or _scan_destructive(cmd):  # 1) hard refusal precedes everything
        return "destructive"
    loop_found, loop_safe, rewritten = _rewrite_proven_literal_for_loop(cmd)
    if loop_found:
        return "read_only" if loop_safe and classify(rewritten) == "read_only" else "mutating"
    if _is_safe_readonly_command(cmd):                    # 2) preserve reviewed read-only pipelines
        return "read_only"
    for conditional in _split_conditionals(cmd):          # 3) every line/control-flow branch is a read
        # Keep a pipeline intact long enough for _is_safe_readonly_command to prove that its
        # downstream stages read stdin only. Splitting `read | grep || true` all the way down first
        # turns the stdin-only grep into an apparent bare file reader and creates a false write card.
        if _is_safe_readonly_command(conditional):
            continue
        for seg in _split_chain(conditional):
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
