"""Reasoning-blind command guardrails for the SSH-ops spike.

Core safety principle (from the design discussion): the decision to run / refuse /
confirm a command is driven ONLY by the user intent + the literal command string,
NEVER by anything the instance emitted. Output read off the box is untrusted data,
not instructions — so classification happens here, before execution, on the command
text alone. This is the XPIA / prompt-injection firewall.

Three tiers (mirrors internal/tools/safe_executor.go semantics):
  - read_only  : auto-run, no human prompt        (T1 diagnostics allowlist)
  - mutating   : requires explicit human confirm  (T3 gate)
  - destructive: hard-refused, even with confirm  (L2 invariant, checked first)
Anything not matched is treated as `mutating` (deny-first: unknown => needs confirm).
"""
import re

# Commands that change nothing on the box. Matched against the WHOLE command.
# Conservative: any shell metacharacter enabling mutation/exec/redirection
# disqualifies auto-run (the model can still issue separate tool calls).
_READ_ONLY = [
    # nvidia-smi: query/read flags ONLY (NOT -r/--gpu-reset/-pl/-pm/-e which mutate)
    r"nvidia-smi(\s+(-q|-L|-a|-i\s+\d+|--query[\w.-]*=\S+|--format=\S+|--display=\S+|dmon|pmon|topo(\s+-m)?))*",
    # file/log/disk/mem/proc readers (chaining/redirection already blocked upstream)
    r"(cat|head|tail|less|more|wc|stat|file|readlink|du|df|ls|lsof)(\s+\S+)*",
    r"journalctl(\s+\S+)*",
    r"systemctl\s+(status|is-active|is-enabled|is-failed|list-units|list-unit-files|show|cat)(\s+\S+)*",
    r"(free|uptime|uname|hostname|whoami|id|pwd|date|lscpu|lsblk|lsmod|lspci|dmesg|nproc|arch)(\s+\S+)*",
    r"(ps|top|vmstat|iostat|mpstat)(\s+\S+)*",
    r"(ss|netstat)(\s+\S+)*",
    # ip: read subcommands only (NOT set/add/del/flush which mutate)
    r"ip\s+(-\w+\s+)*(addr|a|link|l|route|r|neigh|n)(\s+(show|list|s|l))?",
    r"env",
    r"printenv(\s+\S+)*",
    r"getconf(\s+\S+)*",
    r"(nvcc|python3?|pip3?|conda|docker|git|gcc|cmake)\s+(--version|version|-V)",
    r"pip3?\s+(list|show|freeze)(\s+\S+)*",
    r"conda\s+(list|info|env\s+list)(\s+\S+)*",
    r"docker\s+(ps|images|info|version|stats\s+--no-stream)(\s+\S+)*",
]

# Always-refused, irreversible / high-blast-radius. Checked FIRST.
_DESTRUCTIVE = [
    r"\brm\b", r"\brmdir\b", r"\bunlink\b", r"\bshred\b", r"\btruncate\b",
    r"\bmkfs\w*\b", r"\bdd\b", r"\bfdisk\b", r"\bparted\b", r"\bwipefs\b",
    r"\bshutdown\b", r"\breboot\b", r"\bhalt\b", r"\bpoweroff\b", r"\binit\s+[06]\b",
    r"\buserdel\b", r"\bgroupdel\b", r"\bpasswd\b", r"\bchpasswd\b",
    r">\s*/dev/[sn]d", r"\bof=/dev/",
    r"\bchmod\b.*\s-R\b", r"\bchown\b.*\s-R\b", r"\bchmod\b.*\b777\b",
    r"iptables\s+-F", r"ufw\s+disable", r"systemctl\s+(disable|mask)\b",
    r":\s*\(\s*\)\s*\{", r"/etc/(passwd|shadow|sudoers)\b", r">\s*/etc/",
    r"\bcrontab\b\s+-r",
]

_DANGEROUS_META = re.compile(r"[;|&`]|\$\(|>>|>|<|\|\||&&|\n")


def _search_any(cmd, patterns):
    return any(re.search(p, cmd) for p in patterns)


def _fullmatch_any(cmd, patterns):
    return any(re.fullmatch(p, cmd) for p in patterns)


def classify(command: str) -> str:
    """Return 'destructive' | 'read_only' | 'mutating'. Reasoning-blind: command text only."""
    cmd = command.strip()
    if _search_any(cmd, _DESTRUCTIVE):              # 1) destructive precedes everything
        return "destructive"
    if not _DANGEROUS_META.search(cmd) and _fullmatch_any(cmd, _READ_ONLY):
        return "read_only"                          # 2) single clean read-only command
    return "mutating"                               # 3) deny-first: unknown => confirm


# --- output redaction (centralized; the box's output is untrusted; never leak secrets) ---
_REDACTORS = [
    (re.compile(r"(?i)\bsk-[a-z0-9]{8,}\b"), "sk-***REDACTED***"),
    (re.compile(r"(?i)(bearer\s+)[a-z0-9._\-]{12,}"), r"\1***REDACTED***"),
    (re.compile(r"(?i)\b(api[_-]?key|token|secret|password|passwd|pwd)(\s*[=:]\s*)\S+"),
     r"\1\2***REDACTED***"),
    (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----",
                re.S), "***REDACTED PRIVATE KEY***"),
    (re.compile(r"\b(LTAI|AKIA|AKID)[A-Za-z0-9]{8,}\b"), "***REDACTED-AK***"),
]


def redact(text: str) -> str:
    out = text
    for pat, repl in _REDACTORS:
        out = pat.sub(repl, out)
    return out
