"""Gate for output clipping (regression: head-only truncation hid the newest log lines).

Run:  python test_clip.py   ->  exits non-zero on ANY miss.

A head-only clip made `cat <big service log>` present a window whose newest visible line was months
old; live runs then reported a months-stale "last run" — accurate for what they were shown, but
wrong about the box. The clip must keep the TAIL (where a log's crash and latest timestamp live)
as well as the head, and must say material was elided.
"""
import ssh_transport as t


def main():
    misses = []

    def check(name, cond):
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

    print(f"clip: {6 - len(misses)}/6 passed")
    if misses:
        raise SystemExit(f"FAIL: {len(misses)} miss(es)")
    print("\nALL GREEN")


if __name__ == "__main__":
    main()
