"""Offline evidence that the reasoning-blind guardrails tier commands correctly.
Run: python test_guardrails.py   (no network / no SSH needed)
"""
from guardrails import classify, redact

CASES = [
    ("nvidia-smi", "read_only"),
    ("nvidia-smi -q", "read_only"),
    ("nvidia-smi --query-gpu=memory.used --format=csv", "read_only"),
    ("df -h /", "read_only"),
    ("free -h", "read_only"),
    ("uptime", "read_only"),
    ("systemctl status ssh", "read_only"),
    ("journalctl -u docker --no-pager -n 100", "read_only"),
    ("cat /proc/meminfo", "read_only"),
    # deny-first: chaining / pipes / redirection disqualify auto-run
    ("ps aux | grep python", "mutating"),
    ("echo hi > /tmp/x", "mutating"),
    ("nvidia-smi; rm -rf /", "destructive"),     # destructive precedes everything
    # genuine mutating -> T3 confirm
    ("systemctl restart vllm", "mutating"),
    ("pip install torch", "mutating"),
    ("mkdir /data/models", "mutating"),
    # destructive -> hard refuse
    ("rm -rf /tmp/x", "destructive"),
    ("dd if=/dev/zero of=/dev/sda", "destructive"),
    ("mkfs.ext4 /dev/vdb", "destructive"),
    ("reboot", "destructive"),
    ("ufw disable", "destructive"),
    ("chmod -R 777 /", "destructive"),
]


def main():
    ok = 0
    for cmd, want in CASES:
        got = classify(cmd)
        mark = "OK " if got == want else "XX "
        ok += got == want
        print(f"{mark} classify({cmd!r}) = {got}  (want {want})")
    print(f"\n{ok}/{len(CASES)} passed")

    print("\n--- redaction (box output is untrusted; secrets scrubbed) ---")
    for s in ["OPENAI_API_KEY=sk-abcdef1234567890",
              "Authorization: Bearer abcdef0123456",
              "db password=hunter2 here"]:
        print(f"  {s!r}\n   -> {redact(s)!r}")

    assert ok == len(CASES), "guardrail classification regressions present"


if __name__ == "__main__":
    main()
