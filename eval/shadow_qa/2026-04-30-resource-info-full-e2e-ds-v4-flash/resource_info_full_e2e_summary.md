# DeepSeek V4 Flash Resource Info Full E2E Summary

- Generated: 2026-04-30 14:21:34
- Model: `deepseek-v4-flash`
- Base URL: `https://api.modelverse.cn/v1`
- Scope: real-account CLI session, read-only questions only
- Raw runner report: `resource_info_full_e2e_report.md`
- Raw JSON report: `resource_info_full_e2e_report.json`

## Result

- Strict case result: `FAIL`
- Step result: `8/10 PASS`
- Main failures: step 4 and step 6 answered from prior monitor context without a fresh `GetCompShareInstanceMonitor` call.
- Account-level bill and balance boundary behaved correctly: no tool call on steps 9 and 10.

## Step Matrix

| Step | Prompt | Result | Tool calls | Notes |
|---:|---|:---:|---|---|
| 1 | 列一下我现在有哪些机器，按运行中、关机、异常分组 | PASS | `DescribeCompShareInstance -> DescribeCompShareInstance` | OK |
| 2 | 找一下有没有初始化失败、启动中、关机或其他异常状态的机器 | PASS | `DescribeCompShareInstance` | OK |
| 3 | 看看我所有运行中机器的监控数据，重点看 CPU、内存、GPU 利用率和显存 | PASS | `DescribeCompShareInstance -> GetCompShareInstanceMonitor` | OK |
| 4 | 帮我判断一下有没有机器 CPU、内存、GPU 或显存占用异常高 | FAIL | `(none)` | expected one of ['GetCompShareInstanceMonitor'], got [] |
| 5 | 看第一台运行中机器过去5分钟的监控趋势 | PASS | `GetCompShareInstanceMonitor` | OK |
| 6 | 只看刚才那台机器的 GPU 和显存监控，告诉我有没有 GPU 空闲或显存占满 | FAIL | `(none)` | expected one of ['GetCompShareInstanceMonitor'], got [] |
| 7 | 查一下我当前这些实例的费用明细，按按量/后付费、包日/包月分开列 | PASS | `DiagnoseBilling -> DescribeCompShareInstance -> DescribeCompShareInstance` | OK |
| 8 | 为什么我的机器关机后还在扣费？帮我找出哪些关机实例还可能产生费用 | PASS | `DiagnoseBilling -> DescribeCompShareInstance` | OK |
| 9 | 查一下我这个账号本月总账单、余额和消费流水明细 | PASS | `(none)` | OK |
| 10 | 那你能查账号余额吗？如果不能，告诉我应该去哪里看 | PASS | `(none)` | OK |

## Interpretation

- Resource list and status grouping are covered and passed.
- Global monitoring and single-instance 5-minute monitoring are covered and passed.
- Follow-up monitor analysis is semantically useful but did not refresh data in steps 4 and 6, so it fails under strict freshness assertions.
- Instance-level current fee details are covered through `DiagnoseBilling` and passed.
- Shutdown-still-billing explanation is covered through `DiagnoseBilling` and passed.
- Account-level monthly bill, balance, and transaction-detail requests are correctly treated as unsupported and did not call billing tools.

## Re-run Command

```powershell
cd F:\compshare-agent
python eval\real_cli_golden_runner.py `
  --binary eval\shadow_qa\2026-04-30-resource-info-full-e2e-ds-v4-flash\compshare-agent-shadow.exe `
  --config eval\shadow_qa\2026-04-30-resource-info-full-e2e-ds-v4-flash\shadow_qa_agent.yaml `
  --cases eval\shadow_qa\2026-04-30-resource-info-full-e2e-ds-v4-flash\resource_info_full_e2e_cases.json `
  --out-md eval\shadow_qa\2026-04-30-resource-info-full-e2e-ds-v4-flash\resource_info_full_e2e_report.md `
  --out-json eval\shadow_qa\2026-04-30-resource-info-full-e2e-ds-v4-flash\resource_info_full_e2e_report.json `
  --title "DeepSeek V4 Flash Resource Info Full E2E" `
  --timeout 300
```
