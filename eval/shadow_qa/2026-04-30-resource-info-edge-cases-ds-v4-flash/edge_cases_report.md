# Resource Info Edge Cases - deepseek-v4-flash

Date: 2026-04-30
Model endpoint: Modelverse OpenAI-compatible API
Model: deepseek-v4-flash

## Code Verification

Commands:

```powershell
go test ./internal/engine -run 'TestResourceInfoGuard|TestMonitorTimeArgNormalizer|TestMonitorHistoricalNoData|TestMixedMonitorBillingIntent|TestMonitorIntentGuard|TestMonitorHistoryBatchGuard' -count=1
go test ./internal/prompt -run 'TestBuildSystem_ContainsExpiryRenewalGuidance|TestBuildSystem_ContainsMonitorWindowGuidance' -count=1
go test ./... -count=1
```

Result: PASS.

Additional post-review regression:

```powershell
go test ./internal/engine -run 'TestExecuteTool_TreatsEmptyArgumentsAsEmptyObject|TestResourceInfoGuard|TestMonitorTimeArgNormalizer|TestMonitorHistoricalNoData|TestMixedMonitorBillingIntent|TestAccountBillingUnsupported' -count=1
go test ./... -count=1
```

Result: PASS.

## Real CLI Cases

All real cases used:

```powershell
eval\shadow_qa\2026-04-30-resource-info-edge-cases-ds-v4-flash\compshare-agent-shadow.exe cli --config eval\shadow_qa\2026-04-30-resource-info-edge-cases-ds-v4-flash\shadow_qa_agent.yaml
```

### 1. Expiry / Renewal

Prompt:

```text
我的机器什么时候到期，哪些开了自动续费？
```

Log: `expiry_renewal_after_summary.txt`

Observed:

- Forced `DescribeCompShareInstance`.
- Final answer listed all 15 instances, including fixed expiry times for prepaid/dynamic instances.
- Answer used `ExpireTime` and `AutoRenew`; no extra "please query again" follow-up.

### 2. Explicit Historical Range, GPU Model Scope

Prompt:

```text
帮我看下昨天 14:00-15:00 的4090监控
```

Log: `explicit_history_range_after_guard.txt`

Observed:

- First called `DescribeCompShareInstance`.
- Then called `GetCompShareInstanceMonitor` once per 4090 instance.
- Every monitor call used a single `UHostIds` entry.
- Every monitor call used `StartTime=1777442400`, `EndTime=1777446000`, i.e. 2026-04-29 14:00:00 ~ 15:00:00 Beijing time.

### 3. Explicit Historical Range, Single Instance

Prompt:

```text
帮我看下昨天 14:00-15:00 的 qa-shadow-20260417-4090 监控
```

Log: `historical_single_no_data.txt`

Observed:

- Called `DescribeCompShareInstance`, then `GetCompShareInstanceMonitor`.
- Monitor args were normalized to `StartTime=1777442400`, `EndTime=1777446000`.
- The API returned valid historical samples, and the final answer used 2026-04-29 14:00 ~ 15:00.

### 4. Historical No Data Fallback

Prompt:

```text
帮我看下4月1日 14:00-15:00 的 qa-shadow-20260417-4090 监控
```

Log: `historical_old_no_data.txt`

Observed:

- Called `DescribeCompShareInstance`, then `GetCompShareInstanceMonitor`.
- Monitor args were normalized to `StartTime=1775023200`, `EndTime=1775026800`, i.e. 2026-04-01 14:00:00 ~ 15:00:00 Beijing time.
- Final answer was deterministic no-data text:
  "没有返回有效监控数据...不能判断...也不会用其他时间的数据替代."
- It did not invent CPU/GPU/VRAM numbers and did not substitute current realtime data.

### 5. Mixed Monitor + Billing Intent

Prompt:

```text
账号里这些机器哪台监控异常，哪台扣费多？
```

Log: `mixed_monitor_billing.txt`

Observed:

- Tool order:
  1. `DescribeCompShareInstance`
  2. `GetCompShareInstanceMonitor`
  3. `DiagnoseBilling`
- Final answer contained both monitoring abnormality analysis and hourly billing ranking.

## Conclusion

The four requested gaps are handled:

- Expiry/renewal queries now have a hard discovery path and a compact resource summary that survives long-instance-list truncation.
- Explicit historical monitor windows are normalized by the engine, not left to the model.
- Historical no-data responses are guarded deterministically.
- Mixed monitor plus billing intent forces both monitor and billing tools in order.

Post-review note: empty tool-call `arguments` are now treated as `{}` to avoid a transient "unexpected end of JSON input" error when a model emits an argument-less tool call.
