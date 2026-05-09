# Phase 1 GT Renderer Smoke (resource + monitor)

Date: 2026-05-09

## Scope

This smoke verifies the Phase 1 resource/monitor renderers against
ground-truth observations from the logged-in CompShare console and real
CompShare API calls.

No raw trace, raw transcript, raw API response, account key, IP address,
instance ID, or customer secret is committed. Raw files stayed under a local
temp directory.

## Ground Truth Inputs

- `qa-shadow-20260417-4090`
  - Console state: running
  - GPU: RTX 4090 class, 1 GPU
  - CPU: 16
  - Memory: 64 GB
  - Billing shown in console: hourly / pay-as-you-go, 1.58 yuan/hour
- `cqc-前端改配更改-预付费`
  - Console state: stopped
  - GPU: P40, 1 GPU
  - CPU: 8
  - Memory: 64 GB
  - Billing shown in console: daily/prepaid, 8.52 yuan/day
  - Stopped prepaid/day instances still keep billing until expiry.

## Verification

### Unit and package tests

- `go test ./internal/intent -run "TestResourceSummaryRenderer|TestMonitorSummaryRenderer" -count=1`
- `go test ./internal/intent ./internal/entity ./internal/engine -count=1`
- `go test ./... -count=1`

Result: all passed.

### Real API renderer check

Method:

- Load `eval/shadow_qa/.env.local`.
- Use the production CompShare API through `internal/tools.ExternalExecutor`.
- Discover `qa-shadow-20260417-4090` from `DescribeCompShareInstance`.
- Call `GetCompShareInstanceMonitor` for that real instance.
- Feed the real monitor payload into `intent.RenderMonitorSummary`.

Sanitized renderer output:

```text
CPU 使用率: 0%
内存使用率(Memory): 1%
GPU 使用率: 0%
显存使用率(VRAM): 1%
```

Assertions:

- Contains CPU, memory, GPU, and VRAM user-facing metrics.
- Does not contain `Data.List`, `MetricKey`, `TagMap`, `gpu_bus_id`, or
  `Results[` internal API paths.

### Real CLI smoke

Runtime:

- Model: `deepseek-v4-flash`
- Trace: `COMPSHARE_TRACE_ENABLED=1`
- Cutover: `USE_INTENT_PLANNER=shadow`, `USE_INTENT_PLANNER_FOR=resource,monitor`
- Config source: temporary copy of `deploy/conf/agent.yaml.example`

Inputs:

1. `qa-shadow-20260417-4090 是什么卡，多少 CPU 和内存，现在运行吗，怎么计费？`
2. `qa-shadow-20260417-4090 当前 CPU、内存、GPU、显存使用率是多少？`
3. `cqc-前端改配更改-预付费 这台现在是什么状态，关机后是否还在计费？计费方式是什么？`
4. `所有实例分别怎么计费？哪些停机还会计费？`
5. `查一下我这个账号本月总账单、余额和消费流水明细`

Observed sanitized results:

- Turn 1 fell back to main ReAct because the planner classified it as
  `mixed_billing_kb`; it answered the GT resource fields correctly and included
  hourly billing price.
- Turn 2 fell back to main ReAct because the planner result was schema-invalid;
  it answered current CPU/memory/GPU/VRAM usage correctly.
- Turn 3 dispatched the deterministic `resource_info` handler and rendered
  stopped state, P40, 8 CPU, 64 GB memory, and daily billing type in a readable
  form.
- Turn 4 hit a Modelverse 403 pre-deduct balance error after
  `DescribeCompShareInstance`; this smoke cannot use it as a billing-quality
  verdict.
- Turn 5 returned the expected account-level hard-block canned reply and did
  not call tools.

## Interpretation

The regression that exposed raw monitor API paths is fixed in the renderer and
verified with a real monitor API payload. The natural CLI monitor question did
not exercise the deterministic monitor handler in this run because the planner
fell back to ReAct; that is a planner stability issue, not a renderer failure.

Instance-level billing remains outside this renderer PR. It needs a separate
GT-verifiable billing smoke when LLM quota allows a complete response.
