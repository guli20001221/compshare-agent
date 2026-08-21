"""Gate for bounded output clipping that preserves both the head and newest log lines.

Run:  python test_clip.py   ->  exits non-zero on ANY miss.

The tail usually carries the newest timestamp or crash. Clipping must preserve both ends and mark
the omitted middle.
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
