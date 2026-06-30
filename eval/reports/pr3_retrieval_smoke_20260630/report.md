# PR3 retrieval smoke report

## Summary

The CI-safe part of the gate is now implemented as real-corpus retrieval recall tests. These tests load the production platform corpus and run the real BM25 retriever, so they can catch corpus or retrieval drift without any model key.

The key-protected live smoke was not executed in this local shell because no model/API environment variables were present. This report intentionally records that as blocked, not passed.

## CI-safe retrieval recall

Covered questions:

- 磁盘空间是如何收费的？100GB 原始空间免费吗
- 删除 Coding Plan 包
- 一直暂无资源 是什么情况

Required evidence:

- Disk billing must retrieve the system/data disk billing chunk.
- Coding Plan management must retrieve the package management/refund chunk.
- Stock shortage must retrieve capacity precheck and Normal/SoldOut status evidence.

Local verification command:

```powershell
go test ./internal/engine -run "RetrievalRecall|ReplayWiring" -count=1
```

Result: passed locally after adding retrieval-query expansion for stock shortage and Coding Plan cancellation/delete phrasing.

## Key-protected live smoke

Status: blocked locally.

Reason: this shell had no model/API environment variables, so the Flash router + Qwen3 RRF + synthesis path could not be executed honestly.

Required cases for the next key-enabled run:

- 磁盘空间是如何收费的？100GB 原始空间免费吗
- 删除 Coding Plan 包
- 取消 Coding Plan 套餐能退款吗
- 一直暂无资源 是什么情况
- Normal 状态是不是说明一定有库存

Acceptance:

- No instance lookup.
- No create confirmation card.
- No GPU-selection prompt.
- Trace records real knowledge retrieval hits.
- Any refusal or wrong answer is fixed by corpus/retrieval/synthesis changes, not by restoring canned replies.

