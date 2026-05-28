# BillingAnomaly Chain → Fast Tier Capability Migration

**Date**: 2026-05-29
**Driver**: ADR-005 Revised — BillingAnomaly 是 API 查询型(非诊断),需归 fast tier 让 ADR-001 tier 边界纯净
**Audience**: Codex(实施)+ Claude(reviewer)
**Estimated effort**: 4-6 hours

## Goal

把 `BillingAnomalyChain`(`internal/diagnosis/billing_anomaly.go`)从 diagnosis 包迁出,作为 fast tier capability(`IntentBillingInstance`)落到 `internal/intent/capability_billing.go`,对照 `capability_pricing.go` 形态。迁移后:
- Engine 走 Phase-1 cutover 直接 dispatch capability handler,不再经 ReAct + `executeDiagnosis("DiagnoseBilling", ...)` 路径
- BillingFactsSummary 等 struct 迁到 intent 包(或 `internal/entity/`)
- `internal/diagnosis/billing_anomaly.go` 删除,`diagnosis/registry.go` 移除 `DiagnoseBilling` entry
- 现有 production 行为 byte-stable(用户文案 + SSE 输出 shape 不变)

## Acceptance(必须全部通过)

- [ ] `IntentBillingInstance` 走 Phase-1 cutover dispatch,通过 trace `task_tier=fast`(ADR-001 实施后)or `intent_route=cutover`(过渡期)体现
- [ ] 用户文案 byte-stable:对比 6 个典型 case 输出,跟迁移前一致(详见 §4 verification cases)
- [ ] `engine.executeDiagnosis("DiagnoseBilling", ...)` 调用路径删除;调用方改走 capability dispatch
- [ ] `internal/diagnosis/billing_anomaly.go` + `billing_facts_test.go` 删除
- [ ] `diagnosis/registry.go` 的 `chainRegistry` 移除 `DiagnoseBilling` 项
- [ ] 全套 Go 测试通过:`go test ./... -count=1`
- [ ] 配额行为保留:list + price-detail 两次 Describe 调用各扣 1 read-expensive 配额(对应 `engine_test.go:1399 TestDiagnoseBillingConsumesMultipleReadExpensiveQuotaUnits`,迁移后改名 `TestBillingInstanceConsumesMultipleReadExpensiveQuotaUnits`)

## 影响范围(7 个 cross-reference 点)

| 文件 | 当前 | 迁移后 |
|---|---|---|
| `internal/diagnosis/billing_anomaly.go` | 274 行 Chain 实现 + Struct 定义 | **删除** |
| `internal/diagnosis/billing_facts_test.go` | BillingFactsSummary 单元测试 | **迁到** `internal/intent/capability_billing_test.go`(struct + Build 函数迁移后) |
| `internal/diagnosis/registry.go:7` | `"DiagnoseBilling": BillingAnomalyChain` | 删除该 entry;若 5 个未验证 chain 也删则整个 file 可能变空 |
| `internal/diagnosis/engine.go` / `engine_test.go:337` | `IsDiagnosisTool("DiagnoseBilling")` 返回 true | 返回 false(更新测试期望) |
| `internal/engine/engine.go:3736` | comment 提 DiagnoseBilling | 更新 comment 改为引用 capability dispatch |
| `internal/engine/engine_test.go:1399` | `executeDiagnosis("DiagnoseBilling", ...)` | 改走 `DispatchCapability(IntentBillingInstance, ...)`;重命名测试 |
| `internal/engine/scenario_test.go` (482 / 541 / 1029 / 1145) | ReAct force tool `DiagnoseBilling` | 改走 Phase-1 cutover capability dispatch |

## 实施步骤

### Step 1 — 新建 `internal/intent/capability_billing.go`

对照 `capability_pricing.go` 形态:

```go
package intent

import (
    "context"
    "fmt"
)

// Billing capability migrated from internal/diagnosis/billing_anomaly.go
// (2026-05-29, ADR-005 Revised). Two-stage handler:
//
//   stage 1: DescribeCompShareInstance with no UHostIds (or all UHostIds when
//            user did not specify) — collects UHostId list.
//   stage 2: re-call DescribeCompShareInstance with explicit UHostIds — API
//            returns prices only when UHostIds is provided (platform-side
//            performance optimization, not changeable).
//
// Each stage consumes 1 read-expensive quota unit (preserve quota behavior
// from prior diagnosis flow, see TestBillingInstanceConsumesMultipleRead
// ExpensiveQuotaUnits).

func handleBillingInstance(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
    const action = "DescribeCompShareInstance"

    // Stage 1: list (or single instance if UHostId provided)
    args1 := buildStage1Args(req.Plan.Slots)  // UHostId in slot 时直接查单实例;否则 Limit=100
    listResult, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, action, args1)
    if fb != nil {
        return *fb
    }
    hosts := mapSliceAt(listResult, "UHostSet")
    if len(hosts) == 0 {
        // 原 chain Verdict: 未找到任何实例
        return makeBillingNoInstanceResult(action, args1)
    }

    // 如果用户指定 UHostId(stage 1 已含 price),直接渲染
    if hasUHostIdSlot(req.Plan.Slots) {
        return buildBillingSummary(hosts, action, args1)
    }

    // Stage 2: 拿所有 UHostId,re-query for prices
    ids := extractUHostIds(hosts)
    args2 := map[string]any{"UHostIds": toAnySlice(ids)}
    priceResult, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, action, args2)
    if fb != nil {
        return *fb
    }
    pricedHosts := mapSliceAt(priceResult, "UHostSet")
    if len(pricedHosts) == 0 {
        return makeBillingNoPriceResult(action, args2)
    }
    return buildBillingSummary(pricedHosts, action, args2)
}

// 以下函数从 billing_anomaly.go 直接迁移(修改 package + 必要的内部依赖):
//   - buildBillingSummary       → 同名,新 package(保持原名,Rule 3 surgical changes)
//   - BillingFactsSummary       → 同名,新 package
//   - BillingInstanceFact       → 同名,新 package
//   - BuildBillingFacts         → 同名,新 package
//   - billingInstanceFact       → 同名,新 package
//   - HasStoppedImageCost       → 同名
//   - retainedStoppedCharge / actualInstanceCost / formatInstanceCost
//   - formatInstanceFactCost / chargeTypeLabel / billingPeriod
```

**Struct 迁移目的地**:
- BillingFactsSummary / BillingInstanceFact 类:**留在 `internal/intent/capability_billing.go`**(不迁 entity 包,因为只有 billing capability 用)
- 若未来还有其他 billing-related 业务,再 promote 到 `internal/entity/billing.go`(YAGNI)

### Step 2 — 注册 capability_registry entry

`internal/intent/capability_registry.go` line 41-48 加一项:

```go
var capabilityRegistry = []capabilityEntry{
    // ... existing 6 entries ...
    {intent: IntentBillingInstance, skillGroup: "billing", requiredTool: "DescribeCompShareInstance", handler: handleBillingInstance},
}
```

注意 `skillGroup` 取 `"billing"`(新分类,跟 catalog/pricing 平级)。

### Step 3 — Engine 路由调整

- `engine.go:3736` 附近注释:更新对 DiagnoseBilling 的引用 → 提 `IntentBillingInstance` capability cutover
- 确认 `IntentBillingInstance` 已在默认 `USE_INTENT_PLANNER_FOR` 列表里(目前不在 — CLAUDE.md 注释默认是 `resource,monitor,gpu_specs,stock,pricing,platform_image,custom_image,community_image`)。**需要加 `billing_instance` 到默认列表**:`cmd/trace.go` planner cutover 默认 intent set 扩展

### Step 4 — 测试迁移

| 旧测试 | 迁移目的地 |
|---|---|
| `diagnosis/billing_facts_test.go` 4 个 case | `intent/capability_billing_test.go` 同套 4 个 case(只改 import path + package) |
| `engine_test.go:1399 TestDiagnoseBillingConsumesMultipleReadExpensiveQuotaUnits` | 重命名 `TestBillingInstanceConsumesMultipleReadExpensiveQuotaUnits`,改走 capability dispatch 路径 |
| `engine_test.go:337 IsDiagnosisTool("DiagnoseBilling") == true` | 改为 `false`(BillingAnomaly 不再是 diagnosis tool)+ assert `IntentBillingInstance` capability registered |
| `scenario_test.go` Scenario 11/12/相关 | force tool 改为 capability cutover 触发(对照 Scenario 5 等现有 capability cutover scenarios 的 shape) |

### Step 5 — 删除旧代码

确认 Step 1-4 全部通过 + Acceptance 全绿后:

```bash
rm internal/diagnosis/billing_anomaly.go
rm internal/diagnosis/billing_facts_test.go
```

并清 `diagnosis/registry.go` 里 `DiagnoseBilling` entry。如果同时执行 ADR-005 的 5 个 unverified chain 删除,registry.go 可能整个文件移除(届时 chainRegistry 为空)。

## Verification cases(byte-stable 验证)

跑 6 个典型 case,迁移前后输出对比 diff 必须为空(除时间戳类字段):

| Case | 输入 | 期望关键文案 |
|---|---|---|
| C1 | "为什么扣了这么多钱"(无 UHostId,user 有 3 个 Running 实例) | "您当前有 3 个实例,费用明细如下" + 逐项列出 |
| C2 | "uhost-abc 多少钱"(指定 UHostId) | 单实例费用详情 + 实际计费(Stopped Postpay = ¥0 提示) |
| C3 | 关机 Stopped 实例查计费 | "实例费 ¥0(已关机停计) + 磁盘费 ¥X.XX" |
| C4 | 用户无任何实例 | "未找到任何实例。如果您仍在被扣费,可能存在未释放的资源(如云盘),请到控制台检查。" |
| C5 | 包月实例 + 关机 | "包月/包日实例按预付费计费,具体金额以订单为准。" |
| C6 | 混合(包月 + 按量 + 关机) | 总和分项渲染 + StoppedRetainedTotal 提示 |

`eval/golden_test.go` 若有 billing 相关 golden,同步检查保 pass。

## Risks

- **Planner cutover intent set 扩展**: 加 `billing_instance` 到默认集后,所有走 ReAct billing path 的 fallback 行为消失;如有 edge case 走 ReAct,迁移后 break。**缓解**: scenario_test 的 4 个 billing 场景覆盖 force-cutover + cutover-fallback 路径,跑通后 OK
- **`executeDiagnosis` 调用方更广**: grep 显示 `engine_test.go` 还有 2580 + 2512 等行涉及 DiagnoseBilling mock data。这些 mock 可能在其他测试里被复用 → 迁移时全文件 grep `DiagnoseBilling` 替换/删除
- **Quota 行为**: 原 chain 是 2 步 chain → 2 个 read-expensive 单位;新 handler 走 `executeCapabilityAction` × 2 → 等价 2 单位。verify 测试不变

## Out of scope

- 5 个 unverified diagnosis chain 删除(`ssh_failure.go` 等):另一个独立任务,按 ADR-005 Acceptance 同步推进,本 spec 不覆盖
- ADR-001 三 tier 实施(B1 batch):本 spec 提到 `task_tier=fast` 是预期未来行为,实际迁移阶段仍走当前 cutover 形态
- DescribeCompShareInstance API schema 变化:本 spec 假设 schema 稳定

## Sign-off

- [ ] Claude(reviewer): Spec consistent with ADR-005 + ADR-001 + 现有 cutover 形态
- [ ] Codex(implementer): 实施完成 + Acceptance 全绿
- [ ] User: byte-stable 6 cases 通过 + 生产 smoke 验证
