"""Gate for bounded output clipping that preserves both the head and newest log lines.

Run:  python test_clip.py   ->  exits non-zero on ANY miss.

The tail usually carries the newest timestamp or crash. Clipping must preserve both ends and mark
the omitted middle.
"""
import ssh_transport as t
from unittest.mock import patch


def _capture_text(raw):
    capture = t._BoundedOutput()
    capture.append(raw)
    return capture.text()


def main():
    misses = []
    checks = []

    def check(name, cond):
        checks.append(name)
        if not cond:
            misses.append(name)
            print(f"XX  {name}")

    # small output passes through untouched
    check("small output unchanged", t._clip("hello") == "hello")
    exact = "x" * t._MAX_OUTPUT
    check("exactly-at-cap unchanged", t._clip(exact) == exact)

    # oversized: both ends survive, and the elision is announced
    log = "\n".join(f"[2025-10-09] old line {i}" for i in range(2000))
    log += "\n" + "\n".join(f"[2026-07-23] NEW line {i}" for i in range(2000))
    out = t._clip(log)
    check("clip is bounded", len(out) <= t._MAX_OUTPUT + 200)
    check("head retained", "old line 0" in out)
    check("TAIL retained (the regression)", "NEW line 1999" in out)
    check("newest timestamp visible", "[2026-07-23]" in out)
    check("elision is stated", "elided" in out)

    # Large log reads drain in full, retaining bounded evidence from both ends.
    capture = t._BoundedOutput()
    secret = "split-output-" + "credential-7654321"
    raw = ("first log line\npassword=" + secret + "\n" +
           "diagnostic line\n" * 300000 + "final crash line\n").encode()
    max_retained = 0
    for start in range(0, len(raw), 13 * 1024):
        capture.append(raw[start:start + 13 * 1024])
        max_retained = max(max_retained, len(capture.head) + len(capture.tail))
    check("capture stays bounded", max_retained <= t._MAX_CAPTURE_BYTES)
    visible = capture.text((secret,))
    check("received bytes counted", capture.total == len(raw) and capture.truncated)
    check("bounded capture retains first and last evidence",
          "first log line" in visible and "final crash line" in visible and "elided" in visible)
    check("bounded capture remains scrubbed", secret not in visible and "[REDACTED]" in visible)
    clipped_head, clipped_tail = t.guardrails.scrub_output_fragments(
        "initial evidence\npassword=" + secret[:10], secret[10:] + "\nfinal evidence\n", (secret,))
    check("cut credential lines do not become standalone output",
          secret[:10] not in clipped_head and secret[10:] not in clipped_tail and
          "initial evidence" in clipped_head and "final evidence" in clipped_tail)
    check("single oversized line is visibly omitted", "omitted" in _capture_text(b"x" * (t._MAX_CAPTURE_BYTES + 1)))
    unicode_bytes = ("诊断结果" * 17000).encode()
    unicode_capture = t._BoundedOutput()
    unicode_capture.append(unicode_bytes[:10000])
    unicode_capture.append(unicode_bytes[10000:])
    check("uncut UTF-8 split decodes once", "\ufffd" not in unicode_capture.text())

    # A continuously ready stdout used to hide the deadline and starve stderr.
    class BusyChannel:
        closed = False
        stdout_reads = 0
        stderr_reads = 0

        def recv_ready(self): return True
        def recv_stderr_ready(self): return True
        def exit_status_ready(self): return False
        def close(self): self.closed = True
        def recv(self, size):
            self.stdout_reads += 1
            return b"running\n" * (size // 8)
        def recv_stderr(self, size):
            self.stderr_reads += 1
            return b"warning\n"

    channel = BusyChannel()
    with patch("time.monotonic", side_effect=range(100)):
        stdout, stderr, timed_out = t._pump(channel, deadline_s=4)
    check("busy output respects deadline", timed_out and channel.closed and channel.stdout_reads == 3)
    check("stderr cannot starve", channel.stderr_reads == channel.stdout_reads and "warning" in stderr.text())

    class StatusBeforeEOF:
        closed = False
        eof_received = False
        reads = 0
        late_arrived = False

        def recv_ready(self): return self.reads == 0 or (self.reads == 1 and self.late_arrived)
        def recv_stderr_ready(self): return False
        def exit_status_ready(self): return True
        def close(self): self.closed = True
        def recv(self, size):
            self.reads += 1
            if self.reads == 1:
                return b"first log line\n"
            self.eof_received = True
            return b"last failure after exit-status\n"

    delayed = StatusBeforeEOF()
    with patch("time.sleep", side_effect=lambda _: setattr(delayed, "late_arrived", True)):
        stdout, _, timed_out = t._pump(delayed, deadline_s=1)
    check("exit status does not discard late data before EOF",
          not timed_out and delayed.eof_received and delayed.reads == 2
          and stdout.text().endswith("last failure after exit-status\n"))

    print(f"clip: {len(checks) - len(misses)}/{len(checks)} passed")
    if misses:
        raise SystemExit(f"FAIL: {len(misses)} miss(es)")
    print("\nALL GREEN")


if __name__ == "__main__":
    main()
