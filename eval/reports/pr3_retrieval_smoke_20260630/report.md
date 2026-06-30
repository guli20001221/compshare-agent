# PR3 retrieval smoke report

## Summary

This gate now has two layers:

- CI-safe retrieval recall tests load the real `deploy/kb/stage2b_w0.jsonl` corpus and run the real BM25 retriever.
- Key-protected live smoke uses the real HTTP engine path with `deepseek-v4-flash`, Qwen3/RRF retrieval, and synthesis enabled.

The live smoke initially exposed two real routing/retrieval gaps:

- Coding Plan delete/cancel phrasing could drift into instance-operation handling and inspect instances.
- Generic resource-capacity semantics like "暂无资源" and `Normal` status could drift into stock tools or retrieve too weakly.

The first attempted fix routed these product-fact questions with engine-local keyword checks and injected API-name hints into the retrieval query. Review rejected that shape because it preserved the keyword-patch pattern.

The final fix is split:

- Real corpus metadata now carries the natural user phrasings, so BM25 raw queries can recall the relevant chunks without engine query stuffing.
- A fallback named `COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD` remains available for the observed flash-router failure mode. The Go-package default remains off, but the production deployment config now explicitly enables it because the guard-off published path did not pass this smoke.

No canned answer was restored, and the engine no longer appends hidden API-name hints to user queries.

## Ablation

I temporarily disabled both the route guard and query expansion on commit `3cf60031` and reran the same smoke set. The run timed out before completing all 25 cases, but the partial replay already showed the route-directive-only path was not sufficient:

- `一直暂无资源 是什么情况` called live inventory / instance tools in several rounds instead of staying in knowledge retrieval.
- One round asked the user to provide GPU/zone, violating the "no GPU or instance prompt" contract.
- `Normal 状态是不是说明一定有库存` had weak/wrong answers in the partial run, including one answer equating `Normal` with current stock.

After corpus metadata was strengthened, I reran the same 25-round smoke with `COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD=0` to verify the actual unguarded path. It still failed the blocking contract:

- Overall: `102/105` blocking assertions passed.
- `pr3_coding_plan_delete_r1` violated `no_instance_or_create` by taking a live-tool path.
- `pr3_stock_normal_semantics_r2` timed out, causing missing retrieval and empty-answer assertions.

Therefore the guard cannot be described as an idle rollback path for this PR. It is now explicitly enabled in `deploy/conf/config.yaml` as the production safety guard for this flash-router jitter class.

## CI-safe retrieval recall

Covered questions:

- 磁盘空间是如何收费的？100GB 原始空间免费吗
- 删除 Coding Plan 包
- 一直暂无资源 是什么情况
- Normal 状态是不是说明一定有库存

Required evidence:

- Disk billing must retrieve the system/data disk billing chunk.
- Coding Plan management must retrieve the package management/refund chunk.
- Stock shortage must retrieve capacity-precheck evidence.
- `Normal` / `SoldOut` status semantics must retrieve the machine-status contract.

Local verification command:

```powershell
go test ./internal/engine -run "RetrievalRecall|ReplayRegression" -count=1
```

Result: passed with raw user queries; no engine query expansion is used.

## Key-protected live smoke

Environment was loaded from local `.env` and `.env.local` without committing or printing secrets.

Runtime:

- Model: `deepseek-v4-flash`
- Retrieval mode: `qwen3_rrf`
- Agentic `SearchKnowledge`: enabled
- Knowledge-QA loop: enabled
- Flash route guard: enabled (`COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD=1`; also enabled in `deploy/conf/config.yaml`)
- Grounded validator / domain guard / disciplined synthesis: enabled

Cases:

- 磁盘空间是如何收费的？100GB 原始空间免费吗
- 删除 Coding Plan 包
- 取消 Coding Plan 套餐能退款吗
- 一直暂无资源 是什么情况
- Normal 状态是不是说明一定有库存

Each case ran 5 rounds, 25 rounds total.

Final command:

```powershell
go test ./cmd -run TestBehavioralGate -count=1 -v -behavioral-gate -behavioral-input eval/reports/pr3_retrieval_smoke_20260630/live_smoke_cases.jsonl -behavioral-contract eval/reports/pr3_retrieval_smoke_20260630/live_smoke_contract.jsonl -behavioral-replay-out eval/reports/pr3_retrieval_smoke_20260630/live_smoke_replay.jsonl -behavioral-min-pass 100 -behavioral-timeout 240s -timeout 90m
```

Final result:

- Overall: `105/105` blocking assertions passed.
- `SearchKnowledge` fired in every round.
- No case inspected instances.
- No case opened a create/deploy confirmation card.
- No case asked the user to choose a GPU or instance.
- `Normal` status semantics no longer refuse; all 5 rounds explain that `Normal` means sellable, not guaranteed stock, and that concrete availability must be checked with `CheckCompShareResourceCapacity`.

Replay output: `live_smoke_replay.jsonl`.
Guard-off replay output: `live_smoke_guard_off_replay.jsonl`.
