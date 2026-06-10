# Realism eval + knowledge_qa forced-tool_choice P0 (2026-06-10)

## What this is

A reusable **realism eval** whose input questions are paraphrased from real
after-sales chat (`聊天记录.md`, WeCom support group) — colloquial, no instance
IDs, symptom-first, policy-driven. It measures the shipped CLI end-to-end on
`main` (deepseek-v4-flash, agentic-RAG default-on): routing, cite-or-disclaim,
0-fabrication, and false-refusal.

- `realism_questions.jsonl` — 30 questions, each labeled with acceptable
  intent(s) + a faithfulness reference (the fact the answer must not contradict)
  + a corpus-presence guess. The chat log is used ONLY as test questions + a
  gap-map, never as corpus content.
- `run_realism_eval.ps1` — one fresh CLI session per question (clean routing, no
  PriorText bleed); records reply + trace intent + tool calls + cite markers.
  Run with cwd = a worktree that has `deploy/kb/` (platform + external corpus).
- `run_one_write.ps1` — single mutating-flow driver (confirm-gate / destructive
  / RetCode) for the write-op leg.

## P0 found (and fixed in this PR)

The first full run (N=1, 30 Qs, `start-server.ps1` key) surfaced a **shipped
P0**: every `knowledge_qa` turn (17/30) hard-errored:

```
错误: LLM 调用失败: status 400 InvalidParameter:
The tool_choice parameter does not support being set to required or object in thinking mode
```

### Root cause (per-KEY, not a stale flag)

The knowledge_qa agent-loop forces `SearchKnowledge` as the first hop via object
/ `"required"` `tool_choice`. A direct probe (same model, same request, only the
API key differs) showed the capability is **per-key**:

| | object | required | auto |
|---|---|---|---|
| key A (`start-server.ps1`) | **400** | **400** | 200 (calls SearchKnowledge) |
| key B (`.smoke_env.ps1`) | 200 | 200 | 200 |

PR #250 flipped `deepseek-v4-flash` `SupportsObjectToolChoice` to `true` from a
reprobe that used a key-B-class key (→ 200). The deployed config uses key A,
where forced tool_choice 400s in thinking mode. The forced-hop gated only on the
capability flags, never on the actual per-call result. `diagnosis` turns use the
same `SearchKnowledge` tool via **auto** and worked — proving only the *forced*
path was broken.

### Fix (key-agnostic graceful degradation)

- `internal/llm/client.go` — on a forced-tool_choice thinking-mode 400, retry
  once with `auto` instead of failing the turn (scoped to that exact message so
  an absent-tool 400 does not trigger it).
- `internal/engine/engine.go` — inject the SearchKnowledge advisory note
  **unconditionally** when forcing, so the auto-fallback reliably calls it first.
- Capability flag left as-is (correct for capable keys); runtime now degrades for
  incapable keys regardless of which key ops configures.

### Before / after (same key A that broke it)

| Q | before | after |
|---|---|---|
| retention | 400 | grounded (7-day grace, Agent vs 按量) |
| image billing | 400 | grounded (30G free, 0.008元/GB/日) |
| package image | 400 | grounded + honest disclaim |
| 无卡 cards | 400 | honest disclaim (KB lists supported only) |
| model storage | 400 | grounded (外挂 /model, 不占100G) |
| port | 400 | grounded (DescribeCompShareSoftwarePort) |

6/7 knowledge_qa subset recovered to grounded answers; SearchKnowledge fires via
the fallback. Unit tests: `TestClientChatFallsBackToAutoWhenForcedToolChoiceUnsupported`,
`TestForcedToolChoiceClassifiers`.

## Secondary findings (NOT fixed here — separate follow-ups)

- `account_billing_unsupported` keyword preblock over-fires on "关机后还在扣费"
  (deflects a data-disk storage-fee question). Ties to the preblock audit.
- `resource_shortage_226604` keyword preblock is broad: 6 distinct phrasings get
  one canned reply, losing per-question nuance (different-host, will-hit-shortage).
- One knowledge_qa turn hit `token_budget_exceeded` after forced retrieval +
  synthesis — worth checking the per-turn budget headroom on the heavy path.
