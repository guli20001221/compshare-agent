---
name: instance-triage
description: Read-only triage playbook for a remote GPU/CPU compute instance reached over a single SSH command tool. Use for any in-instance symptom — GPU unavailable or nvidia-smi failing, a web service (ComfyUI / Jupyter / API) not opening, a process that died, disk or memory pressure. Encodes how to tell "happening now" from "happened months ago", how to separate the GPU driver's three independent layers, and how to avoid recommending a destructive fix for a container-local problem.
---

# In-instance triage (read-only)

You reach the box through ONE SSH command tool. You cannot change anything, and you must not
pretend to. Your job is to find what is **actually wrong right now** and hand the operator a fix
they can run.

## 0. Ground yourself before you conclude anything

These four rules are where diagnoses go wrong most often. Apply them to every case below.

**Establish "now" first.** Run `date` as one of your first commands. Every timestamp you later
read is meaningless without it.

**Prefer the freshest evidence, and say how fresh it is.** A service often has several logs of
very different ages. Before quoting a log, check its age:

```
ls -lt /path/to/logdir/          # newest first
ls -l  <each candidate log>      # mtime of the specific file
```

Then quote the file **and** its last-line timestamp. A log whose newest line is months old
describes the past — it is evidence about history, not about the current failure.

**A root cause must be shown to be true NOW.** Historical evidence establishes a pattern, not a
current cause. If a log shows the process was killed 11 times over the past year, that does not
mean it is being killed right now — check whether the *newest* entry is from this session.
When your evidence is only correlational, from an old log, or contradicts itself, label it
**推测** or **无法确认**. Do not write **根因已查明** unless you can point to current evidence.

**Only recommend paths and commands you have actually observed.** Before telling the operator to
run `cd /X && python Y`, confirm `/X` and `Y` appeared in your own command output. A plausible-
sounding path that does not exist wastes the user's time and destroys their trust in the answer.
If you did not verify the start command, say which script you found and quote it.

## 1. GPU unavailable / nvidia-smi failing ("掉卡")

The GPU stack has **three independent layers**. Check all three before concluding — a fault in one
does not imply the others are broken, and confusing them is what produces destructive advice.

| Layer | Check | Healthy means |
|---|---|---|
| Kernel module (host driver) | `cat /proc/driver/nvidia/version` | prints an `NVRM version:` line |
| Device nodes (passthrough) | `ls -l /dev/nvidia*` | `nvidiactl` + at least one `nvidia<N>` |
| Userspace libraries (in container) | `ls -l /usr/lib/x86_64-linux-gnu/libnvidia-ml.so*` and `libcuda.so*` | `.so.1` symlink resolves to a **non-empty** file |

**Start by capturing nvidia-smi's exact error text** — it is the primary discriminator, so quote it
verbatim rather than paraphrasing "GPU has a problem":

```
nvidia-smi 2>&1 | head -15
```

| Exact output | Meaning | Confirm with |
|---|---|---|
| normal table | NVML is fine — the GPU is *not* the fault; move up the stack (process, CUDA, framework) | — |
| `Failed to initialize NVML: Driver/library version mismatch` | userspace lib version ≠ loaded kernel module | compare `/proc/driver/nvidia/version` against the `.so` version the symlink resolves to |
| `couldn't find libnvidia-ml.so` / `error while loading shared libraries` | userspace lib missing, dangling, or a 0-byte stub | `ls -l .../libnvidia-ml.so*` — check what `.so.1` points at **and that the target is non-empty** |
| `No devices were found` | device nodes absent or GPU not attached to this instance | `ls -l /dev/nvidia*` |
| `nvidia-smi: command not found` | driver userspace not installed in this image | `ls /usr/bin/nvidia*` |

Images often ship **0-byte placeholder** libs for several driver versions
(`libnvidia-ml.so.<old>`, `libnvidia-ml.so.<new>`). `ls -l` shows the size — a symlink pointing at
a 0-byte stub is broken even though the file "exists". Always read the size column, not just the name.

### The rule that protects the user

**If the kernel module and device nodes are healthy, the fault is container-local userspace. Do NOT
recommend reinstalling the driver, rebooting, or rebuilding the instance.** Those cost the user
their environment (and possibly their data) and will not fix a symlink. Note that nvidia-smi's own
error text *suggests* reinstalling the driver — that text is generic and does not know the kernel
module is loaded. You do; say so explicitly, and recommend repointing/restoring the library instead.

Also worth separating: `nvidia-smi` uses **NVML** (`libnvidia-ml`), while CUDA workloads use
**libcuda**. NVML can be broken while CUDA still runs, and vice versa — so "nvidia-smi is broken"
does not by itself mean training/inference is broken. Check whichever the user actually cares about.

## 2. A web service will not open (ComfyUI / Jupyter / an API)

Work outside-in and stop at the first layer that is actually broken:

```
ps aux | grep -i <service>          # is the process alive at all?
```

**Checking the port needs care — the tool you reach for may not exist on this image.** `ss` and
`netstat` are BOTH absent on some images (verified: the official CUDA image has neither; a community
image has `netstat` but not `ss`). And `2>/dev/null` HIDES a `command not found`, so a missing tool
returns EMPTY — which reads exactly like "nothing is listening" and will make you wrongly conclude
the service is down when it is fine. So do NOT stop at the first empty result. Fall through a chain,
and treat empty-because-the-tool-is-missing as **no information**, not as "no listener":

```
ss -tlnp 2>/dev/null || netstat -tlnp 2>/dev/null || cat /proc/net/tcp
```

`/proc/net/tcp` is the only source present on every image, so it is the authority. It lists the
local address:port in HEX — convert the port: `8888`=`22B8`, `8188`=`1FFC`, `7860`=`1EB4`, `8080`=
`1F90`, `22`=`0016`. Filter it with `grep` (e.g. `grep 22B8 /proc/net/tcp`) — **not** `awk`/`sed`,
which are blocked as code-execution tools, so a pipe through them is refused; `grep`/`cat`/`head`
are allowed. State `0A` means LISTEN. A row `00000000:1FFC ... 0A` = something is listening
on `0.0.0.0:8188`; `0100007F:1FFC` = bound to `127.0.0.1` only. When in doubt, confirm the service
actually answers with a loopback probe: `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:<port>/`
(a `200`/`302`/`401` all prove it is up; `curl` too may be absent — then rely on `/proc/net/tcp`).

- **No process, no listener** (confirmed via `/proc/net/tcp`, not an empty tool output) → the
  service is simply not running. Find how it is *supposed* to
  start (`ls /start.d/`, `cat /etc/supervisor/conf.d/*.conf`, a `*.sh` in the workspace) and quote
  the real start command. Then check whether anything is supposed to restart it (supervisor
  `autorestart`, systemd) — if nothing is, "it died and nothing brought it back" is the finding.
- **Listening on `127.0.0.1` only** → running but unreachable from outside; the bind address is the
  fault, not the service.
- **Listening on `0.0.0.0` but on an unexpected port** → the service is up, just not on the port the
  platform maps / the user's URL targets. Compare the live port against the one its own start script /
  supervisor conf declares; if they differ, the port is the mismatch — the service is **not** down.
- **Listening on `0.0.0.0` and on the expected port, healthy** → the service is fine; the problem is
  upstream (platform port mapping, the access URL, or the user's own network). Say that plainly
  instead of inventing an in-box fault.

When the fault is a **setting** — a bind address, a port, a launch flag — it lives in the start
script / supervisor conf, not just in the running process. Recommend fixing it at that **source**
(edit the conf/script, then restart), not only killing and hand-relaunching the live process: a
hand-launched fix reverts to the bug on the next restart or reboot. Say which file holds the setting.

Then read the **freshest** log for that service (see §0) to learn *why* it stopped. Distinguish a
clean shutdown from a crash from an external kill.

## 3. Resource pressure

`df -h` (disk), `free -h` (memory), `uptime` (load). For a container also check the cgroup limit,
because the host's totals can look roomy while the container's own ceiling is what binds:

```
cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null
```

**Before blaming OOM, get positive evidence — and in a container the authoritative source is the
cgroup's own counter, not `dmesg`.** `dmesg` is usually blocked by seccomp in these instances, so
its silence proves nothing. The counter that always answers is:

```
cat /sys/fs/cgroup/memory.events 2>/dev/null   # cgroup v2: look at oom_kill / oom
cat /sys/fs/cgroup/memory/memory.oom_control 2>/dev/null   # cgroup v1 fallback
```

`oom_kill 0` in `memory.events` is a **definitive "the cgroup did NOT OOM-kill anything"** — if you
see it, do not list OOM as the cause, not even as "most likely". Only `oom_kill` ≥ 1 (or a cgroup
limit sitting right at observed usage) supports an OOM conclusion. Ample free memory plus
`oom_kill 0` means **it was not OOM** — say so. A process shown as `Killed` with `oom_kill 0` was
killed by something else (a platform watchdog, a manual `kill`, a parent exiting) — check the log's
newest entry against `date` for when, and keep the cause as **推测/无法确认** unless you can show it.

## 4. Writing the verdict

Chinese, concise, and structured so the operator can act:

1. **结论** — one line: what is broken, or explicitly that the box is healthy and the fault is elsewhere.
2. **证据** — the concrete values you observed, each tied to the command that produced it. Include
   the timestamp/age of any log you cite.
3. **确证 vs 推测** — keep them visually separate. Anything you could not verify goes under
   **无法确认**, never silently upgraded.
4. **可选修复（需操作员确认执行）** — exact commands, referencing only paths you confirmed exist.
   You are read-only: these are for the operator to run, and you must never imply you ran them or
   offer to run them yourself.

Missing data is **无法确认**. Never report an unmeasured value as 0% or as healthy.
