# Plan A: Time injection + account billing short-circuit narrowing

## Background
Resource-info E2E (deepseek-v4-flash, monitor-guard build) passed 10/10, but a follow-up
absolute-time historical-monitor probe revealed that the LLM translates "昨天下午 2 点到 3 点"
to a Unix timestamp anchored on a date one year in the past (1745647200 = 2025-04-26 14:00 BJ).
Root cause: `prompt/builder.go` never injects the current wall-clock time, so the LLM falls
back to its training-cutoff "today" when computing relative dates.

Separately, `engine.isAccountBillingUnsupportedNormalized` previously short-circuited any
message containing both an account-scope keyword and a bill keyword, hijacking
instance-scoped billing questions like "查我账号下哪台实例消费最高" — these should route to
`DiagnoseBilling`, not the canned "go to console" reply.

## Scope
1. Inject current Beijing wall-clock time into `messages[0]` (system message) on every
   `Chat()` call so the LLM has a fresh date anchor for relative-time queries.
2. Narrow `isAccountBillingUnsupportedNormalized` to fire only on unambiguous standalone
   account-level keywords (余额 / 总账单 / 消费流水 / 流水 / balance), and explicitly veto when
   instance-scope keywords (实例 / 机器 / 主机 / 哪台 / 每台 / 这些 / 哪些) co-occur.
3. Add a billing routing rule to `prompt/builder.go` systemTemplate so the LLM (which now
   handles ambiguous mixed-scope cases instead of the short-circuit) gets explicit guidance
   on account-vs-instance billing routing.

## Files changed
- `internal/engine/engine.go`
  - Add `time` import.
  - Add `Engine.systemPromptBase string` and `Engine.nowFn func() time.Time` fields.
  - Default `nowFn = time.Now` in `New()` and `NewWithDeps()`.
  - `Init()` and `InitWithContext()` now store the static prompt body in
    `systemPromptBase` and seed `messages[0]` via `composeSystemContent()`.
  - New helper `composeSystemContent()` returns `当前北京时间：YYYY-MM-DD HH:MM\n\n{base}`.
  - New helper `refreshSystemMessage()` rewrites `messages[0]` in place each call.
  - `Chat()` calls `refreshSystemMessage()` immediately after `e.userTurn++`.
  - `isAccountBillingUnsupportedNormalized` rewritten:
    - Vetoes short-circuit when `accountInstanceScopeKeywords` (实例 / 机器 / 主机 / 哪台 /
      每台 / 这些 / 哪些) match.
    - Removed the "accountScopeKeywords && accountBillKeywords" combination branch.
    - Removed the "monthlyBillKeywords && accountBillKeywords" combination branch.
    - Kept the standalone unambiguous-keyword branch (余额 / 总账单 / 消费流水 / 流水 / balance).
  - New package-level `accountInstanceScopeKeywords` slice.

- `internal/prompt/builder.go`
  - Added a "## 计费问题口径" section to `systemTemplate` describing account-vs-instance
    routing, immediately after the "## 实时查询规则" section.

- `internal/engine/engine_test.go`
  - New `TestSystemMessageTimePrefix_InjectedOnInitAndRefreshedEachChat` — mocks `nowFn`
    with a 2-element clock, verifies Init seeds the prefix and Chat refreshes it in place
    while preserving the static prompt body and the system role.
  - New `TestAccountBillingUnsupported_HijackByInstanceScopeFallsThrough` — table-driven
    over 3 hijack-style messages, asserts the LLM is called (i.e. NO short-circuit) when
    the message mentions both account and instance scope.

## Behavior preservation
- `TestAccountBillingUnsupported_ReturnsWithoutLLMOrTools` still passes: input
  "查一下我这个账号本月总账单、余额和消费流水明细" contains 余额 / 总账单 / 消费流水
  (standalone branch) and lacks instance-scope keywords, so short-circuit still fires.
- `TestShouldForceMonitorRefresh.negative_account_billing_boundary` still passes: the
  monitor guard uses an independent `accountBillingKeywords` list and is unaffected.
- All Billing*Guard tests still pass; the diagnosis-tracking path is untouched.

## Verification
- `go test ./...` PASS (full repo, 11 packages).
- `go build ./...` clean.
- Real-account E2E rerun deferred to post-review per the agreed flow.

## Post-review correction (2026-04-30)
User flagged two real bugs in the first cut. Both addressed:

1. **Monthly summary fall-through was unsafe**: prompt-only soft guidance is empirically
   violated by deepseek-v4-flash, which calls DiagnoseBilling on "本月费用". Restored
   monthly summary as a hard-block via a new `monthlyAccountCostKeywords` set (费用 /
   花了 / 花费 / 消费 / 账单 / 扣费 / 多少钱). Branch fires when monthly word AND cost
   word both present AND no instance scope co-occurs.
2. **Instance-scope veto was too broad**: previously veto'd the standalone-keyword
   branch too, allowing "这些机器导致账号余额还剩多少" to fall through. Fixed by
   limiting the instance-scope veto to the monthly-summary branch only. The standalone
   account-only-data branch (余额 / 总账单 / 消费流水 / 流水 / balance) now hard-blocks
   ALWAYS regardless of instance words — those data points live exclusively in the
   billing center.

New keyword sets in `engine.go`:
- `accountOnlyDataKeywords` — unconditional hard-block triggers
- `monthlyAccountCostKeywords` — paired with monthlyBillKeywords for branch 2
- `accountInstanceScopeKeywords` — vetoes branch 2 only

New tests:
- `TestAccountBillingUnsupported_MonthlySummaryHardBlocks` — 4 monthly-summary cases,
  asserts no LLM/tool call + canned reply
- `TestAccountBillingUnsupported_AccountOnlyDataIgnoresInstanceWords` — 4 hijack cases
  with both account data words and instance words, asserts hard-block holds

The hijack-fall-through test (`TestAccountBillingUnsupported_HijackByInstanceScopeFallsThrough`)
still passes — its cases contain neither account-only-data words nor monthly+cost
combinations, so they correctly fall through to the LLM for DiagnoseBilling routing.

## Known limitations / explicit non-goals
- `composeSystemContent` always uses Beijing time (CST, +08:00). Other timezones not
  supported; the agent is operated by a CN team for a CN cloud platform.
- Time refresh updates `messages[0]` in place each turn. This will invalidate any
  provider-side prompt cache keyed on the system message, which is acceptable because
  Modelverse (the current upstream) does not use prompt caching.
- "这些 / 哪些" without an explicit instance noun (e.g. "账号余额这些信息") would
  inappropriately veto the monthly-summary branch — but the standalone branch still
  catches 余额, so the hard-block holds for the listed concrete cases. Real-world
  frequency assumed low; defer narrowing.
