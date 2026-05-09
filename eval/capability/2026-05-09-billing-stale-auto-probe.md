# Billing Stale Follow-up Auto Probe

Date: 2026-05-09

Model: `deepseek-v4-flash`

Purpose: verify the decision made after PR #10 / PR #11: after removing the
old `shouldForceBillingDiagnosis` object-tool-choice guard, instance-billing
follow-ups should rely on model auto-routing rather than a force-tool patch.

## Runtime

- Config source: `deploy/conf/agent.yaml.example`, copied to a temp path with
  model set to `deepseek-v4-flash`.
- Secrets: loaded from `eval/shadow_qa/.env.local` via `scripts/load_env.ps1`.
- Trace: enabled with `COMPSHARE_TRACE_ENABLED=1`.
- Intent planner: enabled in shadow/cutover mode for `resource,monitor` only.
  Billing stayed on the old ReAct/diagnosis path.
- Raw trace, stdout, stderr, temp config, and binary were kept in local temp
  directories and are not committed.

## Probe Inputs

The accepted routing probe used Chinese user text; raw user text is not copied
into this artifact. Sanitized semantic shapes:

1. `<instance billing question: why am I being charged>`
2. `<adjacent billing follow-up: why is it still charging>`

Account hard-block control was also exercised in a separate run:

3. `<account-level monthly bill / balance / transaction-flow question>`

Probe scope is intentionally minimal compared to PR #12's three-case monitor
stale-reuse probe. Expand this matrix if a real user report shows stale billing
reuse or if billing enters a future planner-handler cutover slice.

## Trace Summary

| Probe | Turn | User intent shape | Tool routing result | Notes |
| --- | --- | --- | --- | --- |
| minimal adjacent follow-up | 1 | instance billing | `main_react:DiagnoseBilling` + two `diagnosis_internal:DescribeCompShareInstance:success` calls | auto routed to billing diagnosis without force-tool |
| minimal adjacent follow-up | 2 | billing follow-up | `main_react:DiagnoseBilling` + two `diagnosis_internal:DescribeCompShareInstance:success` calls | no stale tool-reuse observed at trace level |
| account control | 3 | account-level balance / monthly bill / transaction flow | no tool call; `engine_hard_block.hit=true`, `category=account_billing_unsupported` | permanent account hard-block still owns account billing boundary |

## Safety Checks

- Routing observation is derived from one accepted adjacent-follow-up run and
  one accepted account-control run. Two earlier attempts hit final-rendering
  stream/network errors after successful diagnosis tool calls and are excluded
  from the routing claim.
- Raw UHostId pattern in trace: false.
- Raw IPv4 pattern in trace: false.
- Sensitive marker scan in trace: false.
- Stderr bytes: 0 in the accepted runs.

## Observations

- Auto-routing was sufficient to call `DiagnoseBilling` on an adjacent billing
  follow-up. This supports keeping `shouldForceBillingDiagnosis` deleted rather
  than reintroducing an object `tool_choice` guard that is incompatible with
  `deepseek-v4-flash` (`SupportsObjectToolChoice=false`, so a reintroduced
  force-tool guard would be a no-op on the primary baseline).
- The billing turns produced large diagnosis outputs. Two probe attempts hit
  stream/network errors during final LLM rendering after the diagnosis tool path
  had already run (`EOF` or TLS handshake timeout). Those errors are not stale
  routing failures, but they are a separate stability signal for billing
  rendering under long tool results.
- Because billing is not part of the current Phase 1 demo cutover slice, this
  probe does not add a new engine guard. A future billing handler or renderer
  pass should use this artifact as evidence that auto-routing can reach the
  diagnosis path, while long-result rendering may need separate hardening.
- Decision: `shouldForceBillingDiagnosis` deletion stands based on the routing
  observation. Rendering stability under long billing tool results is a separate
  Phase 1 follow-up.
