# Create-flow 表单化确认（editable confirm form）— 设计

日期：2026-06-10 · 基线：origin/main `bcd0058` · 状态：定稿。lead 拍板：表单**纯选择式**（无自由文本，v1 去掉 Name 字段）；编辑后走 §7 方案 A（复验→二次确认卡）。

## 0. 目标（lead 原话归纳）

把创建实例的确认环节从「只读卡片 + y/N」升级为**可编辑表单**：

1. plain create 获得 advise-first 等级的体验（deploy_model 已有，plain create 没有）。
2. 表单内含 **2-3 个镜像推荐**（按 GPU 兼容性过滤）。
3. 可编辑字段：GPU 型号、可用区、镜像、计费方式——**全部选择式**（lead 拍板：不要自由输入；改实例名等继续走对话）。
4. **新协议面默认关闭 + 客户端 opt-in**——旧前端字节不变（红线：新协议帧不得 default-on emit）。
5. 前端（console/frame `feature/light-gpu-console-ai-ws`）+ 后端两个 PR，本地 `start-dev.ps1 -Mutating` 联调验收。

## 1. 现状（已核验，file:line 为 origin/main bcd0058）

- **确认帧**：`confirmationEvent{ConfirmationId, Action, Summary map[string]any, TimeoutSeconds}`（`internal/httpapi/handlers_chat.go:70-75`），由 `ChatOptions.ConfirmFunc` 闭包 emit（`handlers_chat.go:382-397`），经 `sanitizeConfirmArgs`（`:510-523`，剥 Password/Token 类键）。
- **回传**：`ConfirmCSAgentAction{SessionId, ConfirmationId, Confirmed bool}`（`internal/httpapi/ws.go:156-170`）→ `ConfirmBroker.Resolve` 把 bool 灌进 channel（`confirm_broker.go:53-69`），`WaitForConfirmation` 60s 超时（`:88-99`）。
- **workflow 确认门**：`StepConfirm` 分支 `if e.confirmFn == nil || !e.confirmFn(def.Name, args) → cancelled`（`internal/workflow/engine.go:89-110`）。`ConfirmFunc = func(action string, args map[string]any) bool`（`workflow/types.go:96`）。
- **确认卡内容**：`stepConfirmCreate().BuildArgs`（`workflow/create_instance.go:516-544`）：workflow/GpuType/Gpu/CPU/Memory/Zone/ChargeType/image/price/FallbackNote。价格由 `confirmPriceText`（`:447-487`，#249）后端格式化。
- **plain create 的弱点**：镜像选择=「精确/包含匹配，否则取列表第一个」（`create_instance.go:658-726`）；无 GPU↔镜像兼容校验（只有 deploy_model 有 `gpuImageCompatible`，`deploy_model.go:435`）；**中途改字段无任何机制**——只能拒绝整单重来。
- **deploy_model 与 plain create 共用同一个 stepConfirmCreate** → 表单化对两条路径同时生效。
- **客户端能力协商：今天不存在**。`SendCSAgentChat` 仅 SessionId/Message/Image/ProjectId/request_uuid；GetMeta 的 Version 是应用版本非线协议版本。
- 本地 wsgateway 是纯字节双向 pump（`cmd/wsgateway/main.go:120-137`），新帧透传，零改动。

## 2. 协议设计（additive，不新增 event 类型）

不加新 event；在现有 `confirmation` 帧上**新增可选字段 `Form`**，在 `ConfirmCSAgentAction` 上**新增可选字段 `Overrides`**。旧客户端 JSON.parse 后只读已知字段，多余字段天然无害；但仍按红线做双闸门（§4）。

### 2.1 出站：confirmation 帧 + Form

```go
type confirmationEvent struct {
    ConfirmationID string         `json:"ConfirmationId"`
    Action         string         `json:"Action"`
    Summary        map[string]any `json:"Summary,omitempty"` // 不变——旧渲染路径
    TimeoutSeconds int            `json:"TimeoutSeconds"`
    Form           *confirmForm   `json:"Form,omitempty"`    // 新增；双闸门通过才出现
}

type confirmForm struct {
    Version int                `json:"Version"` // 1
    Fields  []confirmFormField `json:"Fields"`
}

type confirmFormField struct {
    Key      string              `json:"Key"`      // GpuType|Zone|ImageId|ChargeType
    Label    string              `json:"Label"`    // 后端格式化（#249 教训：raw obj 不许穿透）
    Type     string              `json:"Type"`     // v1 只有 "select"（留枚举位）
    Value    string              `json:"Value"`    // 当前默认值
    Editable bool                `json:"Editable"`
    Options  []confirmFormOption `json:"Options,omitempty"`
}

type confirmFormOption struct {
    Value string `json:"Value"`
    Label string `json:"Label"` // 例 "RTX 4090 · 24G"
    Note  string `json:"Note,omitempty"`
}
```

### 2.2 入站：ConfirmCSAgentAction + Overrides

```json
{"Action":"ConfirmCSAgentAction","SessionId":"…","ConfirmationId":"…",
 "Confirmed":true,"Overrides":{"GpuType":"A800","ImageId":"uimage-xxx"}}
```

**服务端强校验（安全关键）**：Overrides 只在该 ConfirmationId 确实 emit 过 Form 时接受；key 必须 ∈ Form 中 `Editable=true` 的字段；值必须 ∈ 该字段 Options（v1 全字段 select → 校验=纯白名单成员检查）。校验失败 → error 帧，pending **保留**（超时门继续计时），客户端可改后重发。这是防 CreateCompShareInstance 参数注入（Disks/GPU 数量等不在表单白名单内，永远改不了）。

### 2.3 Opt-in 信号

`SendCSAgentChat` 新增可选 `"Features": ["confirm_form_v1"]`（字符串数组，向前可扩展）。`ws.go` 解析后挂到本 turn。GetMeta 增加可选 `"Features":["confirm_form_v1"]` 供前端探测后端能力（additive，旧前端忽略）。

## 3. 引擎/workflow 改造

`ConfirmFunc` 签名**不动**（CLI、旧路径零变化）。新增可选 richer 回调，仅 httpapi 接线：

```go
// engine.ChatOptions 新增（CLI 不设，保持 nil）
ConfirmEditsFunc func(action string, args map[string]any, form *workflow.ConfirmForm) (confirmed bool, overrides map[string]string)
```

`workflow.Step`（StepConfirm 类型）新增可选项：

```go
BuildForm       func(*Context) (*ConfirmForm, error) // nil → 永远走旧 boolean 路径
ApplyOverrides  func(*Context, map[string]string) error // 白名单合并进 wfCtx.Params
RevalidateSteps []string                              // 编辑后需重跑的前置步名
```

确认门新逻辑（`workflow/engine.go` StepConfirm 分支）：

```
if e.confirmEditsFn != nil && step.BuildForm != nil:
    form := BuildForm(wfCtx)
    confirmed, ov := e.confirmEditsFn(def.Name, args, form)
    if !confirmed → cancelled（同今天）
    if len(ov) > 0 → ApplyOverrides(wfCtx, ov)；回跳重跑 RevalidateSteps（检查库存→查询价格）；
                     重新进入本 StepConfirm（刷新后的卡+表单再确认）——见 §7 分叉
    else → 继续（同今天）
else:
    现行 boolean confirmFn 路径，字节不变
```

编辑回环上限 3 轮，超限 → grounded 失败提示重新发起。复跑库存失败 → 现有步骤失败路径（"您改选的 A800 在 cn-wlcb-01 已售罄…"），不静默兜底。

`ConfirmBroker` pending 条目携带本次 Form（合法键/Options 集合）；channel 类型从 `chan bool` 升级为 `chan confirmResolution{Confirmed bool; Overrides map[string]string}`；`Resolve` 做 §2.2 校验。

## 4. 双闸门（default-off）

1. **boot env flag** `COMPSHARE_CONFIRM_FORM`：默认 off；`"1"` 开；未知值 warn+off（仓库惯例）。off 时 `ConfirmEditsFunc` 不接线，全链路与今天字节一致。
2. **客户端 opt-in**：本 turn `Features` 不含 `confirm_form_v1` → 不 emit Form、拒绝 Overrides。

两闸都过才有新行为。CLI（`cliConfirm`）v1 完全不动——表单仅 WS 路径，CLI 后续单独做。

## 5. 表单内容构建（纯确定性，零额外 API 调用，零 LLM）

新函数 `buildCreateConfirmForm(wfCtx)`（`internal/workflow/create_instance.go`），全部取自既有步骤结果：

| 字段 | 来源 | Options 规则 |
|---|---|---|
| GpuType (select) | Step-2 `DescribeAvailableCompShareInstanceTypes` 结果 | 已选 zone 内可购型号 ∩ 镜像 SupportedGpuTypes（若声明）；≤5 项；default=已 resolve 的 GPU。**不对未校验的选项做库存断言**（红线：没查过的不许说有货）——编辑后由复跑的库存步给权威结论 |
| Zone (select) | 同上 | 该 GPU 存在的 zone 列表；default=已 resolve zone |
| ImageId (select) | Step-1 镜像查询结果 | 兼容过滤（SupportedGpuTypes ∋ 默认 GPU 或未声明）→ 用户 ImageName 精确/包含匹配优先 → 取前 3。community 源用 #235 FuzzySearch 精确匹配结果的 top 版本；v1 不跨源混排 |
| ChargeType (select) | 静态 | Postpay/Day/Month（按量/包日/包月），default Postpay（#246） |

v1 **不**可编辑：实例名（走对话）、CPU/Memory 配比（随 GPU 自动 re-resolve）、GPU 数量（=1）、磁盘大小（后续项）。价格不是表单字段——是展示值，编辑后由复跑的价格步刷新（§7 方案 A）。

deploy_model 路径自动获得同一表单（共用 stepConfirmCreate）；其 GPU 选项天然已被镜像 SupportedGpuTypes 约束，用户改 GPU 走同一复验回环，与 R4「尊重用户指定 GPU」一致。

## 6. 可观测性 / 测试 / 验收

- **trace**：确认步 emit 的 args 已入 observability；新增 `form_emitted bool`、`edit_rounds int`、`override_keys []string`（additive；带上游信号，照 fallback-observability 惯例）。
- **测试**：
  - httpapi：双闸任一未开 → confirmation 帧与今天**字节一致**（golden）；双开 → Form 出现；Overrides 合法回传 → 编辑值落地；非法 key/值 → error 帧且 pending 保留；wrong-owner 行为不变（既有测试）。
  - workflow：编辑回环复跑库存+价格并二次确认；复跑售罄 → grounded 失败；回环上限。
  - sanitize：Form Label 全部后端格式化；Password 类键剥离不回归。
- **验收**：`go test ./...` 全绿（含 golden/eval 套件）；mutating-on smoke 断言确认门仍触发、删除类仍拒；前后端联调 `start-dev.ps1 -Mutating`（先停旧 :8080、重建 agent.exe）走通「创建 4090 → 表单改 A800 → 刷新卡二次确认 → 创建成功」。

## 7. 编辑后的确认模式（已拍板：A）

- **A（推荐）：编辑 → 复验 → 二次确认卡**。用户在表单改 GPU/zone/镜像 → 后端复跑库存+价格 → 重新 emit 刷新后的 confirmation（新 ConfirmationId，含权威库存结论和新价格）→ 用户再确认。优点：永远不让用户对没见过的价格/没核过的库存下单（诚实）；表单构建零额外 API 调用；机制上「编辑永远重进同一校验管道」。代价：编辑时多一轮交互；前端需处理「第二张确认卡替换第一张」。
- **B：单轮确认 + 预取每选项价格/库存**。表单构建时对每个 GPU/zone 选项各打一次价格+库存 API（≤8 次额外读调用），用户改完直接 Confirmed 一次成单。优点：交互少一轮。缺点：确认时刻的价格/库存是构建时刻的快照（有过期窗口）；首张卡延迟显著增加；调用预算膨胀。

## 8. PR 切分

- **PR-B1（本仓，default-off）**：§2-§6 全部。flag off 或客户端未 opt-in 时零行为变化（字节级）。含本设计文档。
- **PR-F1（console/frame，基于 `feature/light-gpu-console-ai-ws` @ `7a778fa`）**：`service.js` SendCSAgentChat 带 Features、ConfirmCSAgentAction 带 Overrides；`index.jsx` 渲染 Form（select/text）+ 二次确认卡替换逻辑；`styles.jsx` 配套。
- 联调通过后再议 flag 翻 default-on（独立小 PR，沿用 boot-only 可回滚惯例）。

## 9. 明确不做（v1）

- CLI 表单输入（保持 y/N）。
- 磁盘大小编辑、GPU 数量编辑、跨源镜像混排推荐。
- LLM 参与表单构建/排序（纯代码可答，Rule 5）。
- 对未校验组合的任何库存/价格断言。
