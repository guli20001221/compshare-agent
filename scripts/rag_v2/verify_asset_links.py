#!/usr/bin/env python3
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import json
from pathlib import Path
import time
from typing import Any
from urllib.request import Request, urlopen


def check(url: str, timeout: int) -> dict[str, Any]:
    request = Request(
        url,
        headers={"User-Agent": "Mozilla/5.0 RAG-V2-Link-Check", "Range": "bytes=0-1023"},
    )
    last_error: Exception | None = None
    for attempt in range(3):
        try:
            with urlopen(request, timeout=timeout) as response:
                response.read(1024)
                return {"url": url, "ok": 200 <= response.status < 400, "status": response.status, "content_type": response.headers.get("Content-Type")}
        except Exception as exc:  # noqa: BLE001 - retried then reported exactly
            last_error = exc
            if attempt < 2:
                time.sleep(attempt + 1)
    assert last_error is not None
    return {"url": url, "ok": False, "error": f"{type(last_error).__name__}: {last_error}"}


def main() -> int:
    parser = argparse.ArgumentParser(description="Verify every unique clickable asset URL in one or more corpora.")
    parser.add_argument("--chunks", type=Path, action="append", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--rewrite-prefix", nargs=2, metavar=("FROM", "TO"))
    parser.add_argument("--only-prefix", action="append", default=[])
    parser.add_argument("--skip-prefix", action="append", default=[])
    parser.add_argument("--workers", type=int, default=24)
    parser.add_argument("--timeout", type=int, default=20)
    args = parser.parse_args()

    urls: set[str] = set()
    for path in args.chunks:
        for line in path.read_text(encoding="utf-8").splitlines():
            row = json.loads(line)
            urls.update(str(ref["url"]) for ref in (row.get("asset_refs") or []))
    if args.rewrite_prefix:
        source, target = args.rewrite_prefix
        urls = {target + url[len(source):] if url.startswith(source) else url for url in urls}
    if args.only_prefix:
        urls = {url for url in urls if any(url.startswith(prefix) for prefix in args.only_prefix)}
    urls = {url for url in urls if not any(url.startswith(prefix) for prefix in args.skip_prefix)}

    results: list[dict[str, Any]] = []
    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        pending = {pool.submit(check, url, args.timeout): url for url in sorted(urls)}
        for future in as_completed(pending):
            results.append(future.result())
    results.sort(key=lambda item: item["url"])
    failed = [item for item in results if not item["ok"]]
    report = {"checked": len(results), "passed": len(results) - len(failed), "failed": len(failed), "failures": failed, "results": results}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"checked={len(results)} passed={len(results)-len(failed)} failed={len(failed)}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
