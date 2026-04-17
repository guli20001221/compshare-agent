# Block A: compshare-agent 成熟化执行计划

> **⚠️ ARCHIVED** — 本文档是 Block A 的原始执行计划，数字和口径为计划制定时的快照。
> Block A 执行过程中实际数字已远超计划（25+ 工具、77+ 评测、18 条金标等）。
> 当前真相源请查看 `.claude/CONTEXT.md`。本文档仅保留作为历史决策参考。

## Context

compshare-agent 是一个 GPU 算力共享平台的 AI 客服原型（F:\compshare-agent）。当前有 15 个工具、35 个评测用例、28 个场景测试，已通过 Qwen3-32B 100% 评测。Block A 的目标是将它从"评测通过的原型"升级为"面向 GPU 实例控制台主链路的成熟 CLI 任务原型"，然后冻结，切入 Block B（迁入 uhost-compshare-api-master）。

**v2 修正（基于用户 API 文档审查反馈）**:
1. `DescribeCompShareImages` 移除 Custom 枚举 — API 文档仅支持 System/App
2. `DescribeCompShareCustomImages` 定位为查询工具，不进入创建主链路
3. `ResetPasswordWorkflow` 增加容器/普通主机状态判断 — 普通主机仅 Stopped 可重置
4. `GetCompShareInstanceUserPrice` 参数用大写 GPU/CPU，返回 PriceDetails（非 PriceSet）
5. `DescribeCompShareJupyterToken` 标注"仅用首元素"行为
6. 社区镜像创建保留（API 文档确认 CompShareImageId 可接受社区镜像 ID）

**v3 修正（用户第二轮审查）**:
7. `ResetPasswordWorkflow` 状态判断改用 `InstanceType` 字段（非 Container 字段/镜像类型推断）
8. `DiagnoseImageIssue` 镜像类型判断改用 `CompShareImageType` 字段（非名称匹配）
9. `DiagnosePortOrFirewall` 能力收窄 — `DescribeCompShareSoftwarePort` 是平台服务端口表，不是实例防火墙状态接口

**v4 修正（用户第三轮审查，开工前最终版）**:
10. `CreateInstanceWorkflow` 社区镜像路径需同步修改 `stepGetPrice()`，付费社区镜像需要带 CompShareImageId 进价格查询
11. `DescribeCompShareJupyterToken` 脱敏改为双通道：LLM/history 脱敏 + CLI 终端安全展示原值
12. `GetCompShareInstanceUserPrice` 需补路由规则（builder.go）+ Dynamic/Postpay 口径归一化
13. 执行顺序改为文件 ownership 串行：Phase 0+2 → Phase 3+5 → Phase 4 → Phase 1 → Phase 6

**v5 修正（开工前最终审查）**:
14. JupyterToken 双通道需修改 `cmd/agent.go`（onStep 回调）+ `engine.go` StepEvent 新增 Display 字段
15. JupyterToken 验收口径统一：LLM 不含真实 token = 脱敏成功，CLI Display 展示原值 = 可用
16. `ResetPasswordWorkflow` 和 `DiagnoseImageIssue` 枚举值写死：`InstanceType == "Container"` / `CompShareImageType == "Community"`
17. 文件清单修正为 11 个修改 + 13 个新建

**现状 → 目标**

| 维度 | 现状 | 目标 |
|------|------|------|
| 知识工具 | 2 | 2（不变） |
| API 工具 | 6 | 10 |
| Workflow | 3 | 6 |
| Diagnosis | 4 | 6 |
| 脱敏 | 无 | 有 |
| 离线评测 | 35 | 60 |
| 场景测试 | 28 | 40 |
| CLI 金标 | 无 | 12 |

---

## Phase 0: Prompt 优化（修 3 个已知误判）

**依赖**: 无。最先执行。

### 0.1 修复 sq_08：库存查询工具描述增强

**文件**: `internal/tools/registry.go`

- `CheckCompShareResourceCapacity` 的 Description 改为:
  `"查GPU库存/有没有货/还有没有/是否有货/是否售罄。检查指定GPU型号在指定区域的库存是否充足。"`
- `DescribeCompShareImages` 的 Description 修正（同时修复 Custom 错误）:
  - 移除 `Custom` 枚举值。API 文档仅支持 `System`（平台公共镜像）和 `App`（应用镜像），不支持 Custom
  - 改为: `"查询平台镜像列表。ImageType 枚举：System（平台公共镜像）、App（应用镜像），不传返回全部。查自制镜像请用 DescribeCompShareCustomImages，查社区镜像请用 DescribeCommunityImages。不用于查库存。"`
  - 同时将 parameters 中 ImageType 的 description 改为只列 System/App

### 0.2 修复 ct_04：创建操作优先级规则

**文件**: `internal/prompt/builder.go`

在 `## 行为规则` 的 `complex_task` 部分之前，新增优先级规则：

```
## 意图优先级
- 用户提到"创建"、"开一台"、"帮我建"、"部署一台"等明确创建操作时，必须使用 CreateInstanceWorkflow，不要先用 GetCompShareInstancePrice 查价格。仅当用户明确只问价格时才用价格查询工具。
```

### 0.3 新增 UserPrice 路由规则

**文件**: `internal/prompt/builder.go`

在 `simple_query` 路由部分或行为规则中追加:
```
- 用户问"折后价"、"实际价格"、"我买多少钱" → 调用 GetCompShareInstanceUserPrice（返回折后/原价/目录价三组）
- 用户问"价格"、"多少钱"（泛指） → 调用 GetCompShareInstancePrice（返回目录价）
- 注意：GetCompShareInstanceUserPrice 的计费方式用 Postpay（不是 Dynamic），参数用大写 GPU/CPU
```

**文件**: `internal/tools/registry.go`

`GetCompShareInstanceUserPrice` 的工具描述中明确：
- "查用户折后价/实际价格。计费方式用 Postpay（等同于按量 Dynamic）。参数 GPU/CPU 大写。"

### 0.4 修复 Kimi-K2 诊断绕过：诊断路由强制规则

**文件**: `internal/prompt/builder.go`

在 `diagnosis` 路由项后追加强制规则：

```
  **重要**：用户描述了具体问题/故障时（SSH连不上、端口不通、nvidia-smi报错、初始化失败、扣费异常、镜像无法使用等），必须调用对应的 Diagnose* 诊断工具进行自动排查，禁止仅用知识文本直接回答。诊断工具会自动排查并给出结论。
```

### 验证

- `go build ./...`
- `go test ./internal/prompt/...`
- 单独跑 sq_08、ct_04 评测用例确认修复

---

## Phase 1: 知识层（8 主题静态知识包）

**依赖**: Phase 0（prompt 模板已修改）

### 1.1 重写 FAQ

**文件**: `internal/prompt/faq.go`

将当前 FAQContent（34 Q&A、167 行）替换为 8 主题结构化知识包。不是简单 Q&A，而是面向用户的操作指导：

**8 个主题**:

1. **镜像选择** — 三类镜像：平台镜像（System，基础环境）、社区镜像（社区用户发布，预装应用如 SD/ComfyUI/Dify）、自制镜像（Custom，个人快照，最大 1000GB）。查询分别用 `DescribeCompShareImages`（平台，ImageType 仅支持 System/App）、`DescribeCommunityImages`（社区）、`DescribeCompShareCustomImages`（自制）。注意：`DescribeCompShareImages` 不支持 Custom 类型。

2. **登录实例** — SSH（root 用户，端口见实例详情）、VS Code Remote-SSH、JupyterLab 网页终端、Windows RDP（mstsc）。教程：`https://www.compshare.cn/docs/operation/logininstance`

3. **防火墙/端口** — 公网固定 IP，1.5Gb 共享带宽（单 IP 上限 300Mbps）。端口映射查 `DescribeCompShareSoftwarePort`。自定义服务自启动放 `/start.d/` 目录。

4. **云硬盘** — 系统盘 200GB 免费额度。额外数据盘通过控制台添加。关机后按量模式下额外磁盘仍计费。自制镜像最大 1000GB。

5. **公共模型库** — 主流模型已预下载，直接挂载使用。文档：`https://www.compshare.cn/docs/bestpractices/sharemodel`

6. **网络加速** — GitHub/HuggingFace 加速。控制台开通：`https://console.compshare.cn/light-gpu/console/accelerator`。社区镜像默认配置；虚机/基础镜像需改 DNS。

7. **无卡模式** — 关机后以无卡模式启动，不挂载 GPU，0.15 元/时。限 1 台/账号。支持：4090、4090-48G、3090、5090、A800、H20。不能制作镜像。

8. **计费/回收规则** — 四种模式（按量/包时/包日/包月）。按量关机后 GPU/CPU/内存停止计费但额外磁盘继续。包时/包日/包月关机仍计费。错误码：8357=售罄、8095=配额不足、8429=过期欠费。价格以 `GetCompShareInstanceUserPrice` 实时查询为准，不硬编码。

**原则**:
- 数字型信息（价格、配额上限）不硬编码，指向 API 或"以控制台为准"
- 操作路径转化为用户话术（不说"控制台→镜像列表→共享"，说"你可以通过...分享镜像"）
- 保留错误码等高频查询内容
- 目标 ~1200-1500 tokens，不超过当前 FAQContent 太多

### 验证

- `go test ./internal/prompt/...` — builder_test 和 faq_test 通过
- 手动检查 8 个主题标题都存在
- grep FAQContent 确认无硬编码 GPU 价格数字

---

## Phase 2: API 工具扩展（6→10）

**依赖**: Phase 0（registry.go 已被修改）

### 2.1 新增 4 个 API 工具定义

**文件**: `internal/tools/registry.go`

在 Registry slice 中追加：

| 工具名 | Description | 关键参数 | 安全级别 |
|--------|-------------|----------|----------|
| DescribeCompShareCustomImages | 查询用户自制镜像列表（仅查询，不进入创建主链路） | CompShareImageId(opt), Offset(opt), Limit(opt) | L0（已存在） |
| DescribeCommunityImages | 查询社区镜像列表。支持按名称/作者/标签/模糊搜索筛选。返回 CompshareImageGroup 分组结构，每组含 Data 版本数组 | Name(opt), FuzzySearch(opt), Tag(opt array), Offset(opt), Limit(opt) | L0（已存在） |
| DescribeCompShareJupyterToken | 获取实例 Jupyter 访问 Token。传 UHostIds 数组但仅使用第一个元素。**返回的 JupyterToken 是敏感数据，必须脱敏** | UHostIds(required, array, 仅用首元素) | L0（已存在） |
| GetCompShareInstanceUserPrice | 查询用户折后实际价格（区别于 GetCompShareInstancePrice 目录价）。**注意参数大写：GPU、CPU（不是 Gpu、Cpu）**。返回 PriceDetails/OriginalPriceDetails/ListPriceDetails 三组明细（不是 PriceSet） | Zone(req), GpuType(req), GPU(req, int), CPU(req, int), Memory(req, int, MB), ChargeType(opt: Month/Day/Postpay/Spot) | L0（已存在） |

**重要区分**: `GetCompShareInstancePrice` 和 `GetCompShareInstanceUserPrice` 是两个不同接口：
- 前者：参数用小写 `Gpu`/`Cpu`，返回 `PriceSet`，含 Dynamic 计费方式
- 后者：参数用大写 `GPU`/`CPU`，返回 `PriceDetails`，含折后/原价/目录价三组，ChargeType 用 `Postpay` 替代 `Dynamic`

**DescribeCompShareCustomImages 定位说明**: 此工具仅作为查询能力，供用户查看已有自制镜像。不进入 Block A 的创建主链路（CreateInstanceWorkflow 只支持 platform + community）。

### 2.2 验证安全白名单

**文件**: `internal/security/levels.go` — 确认这 4 个 Action 已存在于 L0 列表。根据探索结果，4 个都已在白名单中，无需修改。

### 验证

- `go build ./...`
- `go test ./internal/tools/...`
- `go test ./internal/engine/...` — TestScenario_AllToolsRegistered 应仍通过

---

## Phase 3: Workflow 扩展（3→6 + CreateInstance 增强）

**依赖**: Phase 2（新 API 工具已注册）

### 3.1 新建 RebootInstanceWorkflow

**新文件**: `internal/workflow/reboot_instance.go`

模式：仿 `stop_instance.go`，复用 `extractInstanceState`、`extractInstanceZone`、`extractInstanceSummary`。

3 步：
1. 查询实例（DescribeCompShareInstance）→ CheckResult: 仅 Running 可重启
2. 确认重启（StepConfirm）→ 展示实例摘要
3. 执行重启（RebootCompShareInstance）→ Zone + UHostId

### 3.2 新建 RenameInstanceWorkflow

**新文件**: `internal/workflow/rename_instance.go`

3 步：
1. 查询实例（DescribeCompShareInstance）→ CheckResult: 确认实例存在
2. 确认改名（StepConfirm）→ 展示 old name → new name
3. 执行改名（ModifyCompShareInstanceName）→ UHostId + Name

参数：UHostId(required), Name(required)

### 3.3 新建 ResetPasswordWorkflow

**新文件**: `internal/workflow/reset_password.go`

**关键产品约束（来自 API 文档）**:
- 普通主机（非容器）：仅 `Stopped` 状态可重置密码
- 容器实例：支持 Running 状态下在线重置
- 密码规则：8-32 字符，至少 2 种字符类型（大小写/数字/特殊字符）
- 密码传输：base64 编码

4 步：
1. 查询实例（DescribeCompShareInstance）→ CheckResult: 
   - 提取 State 和 `InstanceType` 字段（API 文档已明确返回此字段）
   - `InstanceType == "Container"`: Running 或 Stopped 均可重置
   - `InstanceType != "Container"`: 仅 Stopped 可重置，Running 则 Conclude 提示"需要先关机"
   - 其他状态：Conclude 提示当前状态不支持
2. 确认重置（StepConfirm）→ 展示实例 ID + 密码规则提示。**confirm 展示时密码字段显示 `[已设置,不显示]`，不暴露明文**
3. 执行重置（ResetCompShareInstancePassword）→ Zone + UHostId + Password（base64 编码）
4. 查询状态（DescribeCompShareInstance）→ 确认操作完成

参数：UHostId(required), Password(required)

### 3.4 注册 3 个新 Workflow

**文件**: `internal/workflow/registry.go` — 追加 3 条:
```go
"RebootInstanceWorkflow":  RebootInstanceDef,
"RenameInstanceWorkflow":  RenameInstanceDef,
"ResetPasswordWorkflow":   ResetPasswordDef,
```

### 3.5 注册 3 个 Workflow Meta-Tool 定义

**文件**: `internal/tools/registry.go` — 追加 3 个 workflow 工具到 Registry:

| 工具名 | Description | 参数 |
|--------|-------------|------|
| RebootInstanceWorkflow | 重启实例工作流。检查状态→确认→重启。仅 Running 可重启 | UHostId(req) |
| RenameInstanceWorkflow | 重命名实例工作流。确认→修改名称 | UHostId(req), Name(req) |
| ResetPasswordWorkflow | 重置实例密码工作流。普通主机需关机，容器实例支持在线重置。密码需8-32字符，至少2种字符类型 | UHostId(req), Password(req) |

### 3.6 更新 System Prompt 路由

**文件**: `internal/prompt/builder.go` — `complex_task` 部分追加:
```
  - 重启 → 调用 RebootInstanceWorkflow
  - 改名/重命名 → 调用 RenameInstanceWorkflow
  - 重置密码 → 调用 ResetPasswordWorkflow
```

### 3.7 增强 CreateInstanceWorkflow 支持社区镜像

**文件**: `internal/workflow/create_instance.go`

修改 `stepQueryImages()`：
- 新增参数 `ImageSource`（从 `wfCtx.Params` 读取）
- 若 `ImageSource == "community"`：调用 `DescribeCommunityImages` 替代 `DescribeCompShareImages`
- 默认（空或 "platform"）：保持现行为 `ImageType: "System"`
- 永远不路由到 Custom/私有镜像

新增 helper `pickFirstCommunityImageId(result)` 和 `pickFirstCommunityImageName(result)`：
- 社区镜像响应结构（来自 API 文档）：
  ```
  CompshareImageGroup[0].Data[0].CompShareImageId  // 镜像 ID
  CompshareImageGroup[0].ImageName                  // 分组名称
  CompshareImageGroup[0].Data[0].Name               // 版本名称
  ```
- 区别于平台镜像的 `ImageSet[0].CompShareImageId`
- 社区镜像每个 Group 包含多个版本（Data 数组），默认取第一个 Group 的第一个版本

修改 `pickFirstImageId` 和 `pickFirstImageName` 调用点：
- 根据 `ImageSource` 参数选择合适的 picker
- 影响 `stepCheckCapacity`、**`stepGetPrice`**、`stepConfirmCreate`、`stepCreateInstance` 四个步骤
- **`stepGetPrice` 特别注意**: 付费社区镜像需要把 `CompShareImageId` 带进价格查询，否则返回价格不含镜像费用

更新 `CreateInstanceWorkflow` 的 tool registry description:
- 追加 `ImageSource` 参数 (string, optional, enum: "platform"/"community", default "platform")
- Description 追加: "支持平台镜像和社区镜像。传 ImageSource='community' 使用社区镜像创建。不支持自制/私有镜像。"

**边界约束**: CreateInstanceWorkflow 永远不路由到自制镜像。即使 CreateCompShareInstance API 技术上支持 Custom ImageId，Block A 的产品定位是不在主部署链路中支持私有镜像。

### 3.8 新增 Workflow 单元测试

**新文件**:
- `internal/workflow/reboot_instance_test.go`
- `internal/workflow/rename_instance_test.go`
- `internal/workflow/reset_password_test.go`

每个测试覆盖：正常路径 + 前置状态校验失败路径。

### 验证

- `go build ./...`
- `go test ./internal/workflow/...` — 全绿
- `go test ./internal/engine/...` — 现有场景测试不 break

---

## Phase 4: Diagnosis 扩展（4→6）

**依赖**: Phase 2（可能用到新 API 工具）

### 4.1 新建 DiagnosePortOrFirewall

**新文件**: `internal/diagnosis/port_firewall.go`

**能力边界说明**: `DescribeCompShareSoftwarePort` 是"平台已知服务及默认端口表"，不是"实例级防火墙状态"接口。此诊断链只能提供端口/服务可达性线索，不能直接查出实例防火墙配置。

诊断链 (2-3 步)：
1. 检查实例状态（DescribeCompShareInstance）→ 非 Running 则 Conclude（需先开机）
2. 查询平台服务端口表（DescribeCompShareSoftwarePort）→ 列出平台已知的服务和端口映射。若用户指定了 Service 参数，先做名称归一化再匹配
3. Fallback: "平台端口映射正常。服务可能未在实例内启动，请检查 /start.d/ 自启动脚本或手动启动服务。如果是自定义服务端口，请确认服务进程已运行且监听了正确端口。"

**Service 名称归一化**: 用户输入可能是大小写不同或别名，需在匹配前统一：
```
"jupyter" / "jupyterlab" / "jupyter lab" → "JupyterLab"
"ssh" / "SSH" / "terminal"              → "SSH"
"filebrowser" / "file browser" / "文件管理" → "FileBrowser"
```
实现为一个 `normalizeServiceName(input string) string` 函数，strings.ToLower 后查映射表。

参数：UHostId(required), Service(optional — 目标服务名，支持别名和大小写不敏感匹配)

### 4.2 新建 DiagnoseImageIssue

**新文件**: `internal/diagnosis/image_issue.go`

诊断链 (2 步)：
1. 检查实例状态和镜像信息（DescribeCompShareInstance）→
   - InstallFail → Conclude: "初始化失败，可能是镜像问题" → 建议换官方镜像重建
   - Install → Conclude: "正在初始化中"
   - Running → 提取镜像名称，Continue 到下一步
   - 非 Running → Conclude: 需要先开机
2. 分析镜像类型（使用 `CompShareImageType` 字段，API 文档已明确返回）→ 
   - `CompShareImageType == "Community"` → Conclude: "社区镜像可能存在兼容性问题" → 建议联系镜像作者或换用官方镜像
   - 其他类型 → Conclude: "镜像加载正常" → 建议检查应用配置
3. Fallback: "无法确定镜像问题。建议尝试用官方系统镜像重新创建实例。"

### 4.3 注册 2 个新 Diagnosis Chain

**文件**: `internal/diagnosis/registry.go` — 追加:
```go
"DiagnosePortOrFirewall": PortFirewallChain,
"DiagnoseImageIssue":     ImageIssueChain,
```

### 4.4 注册 2 个 Diagnosis Meta-Tool

**文件**: `internal/tools/registry.go` — 追加:

| 工具名 | Description | 参数 |
|--------|-------------|------|
| DiagnosePortOrFirewall | 诊断端口/服务可达性问题。查询平台已知服务端口映射，给出排查线索。用户报告服务无法访问、端口不通时使用 | UHostId(req), Service(opt) |
| DiagnoseImageIssue | 诊断镜像问题。镜像无法使用、启动异常、环境不符时使用 | UHostId(req) |

### 4.5 更新 System Prompt 诊断路由

**文件**: `internal/prompt/builder.go` — `diagnosis` 部分追加:
```
  - 端口不通/服务访问不了/防火墙 → 调用 DiagnosePortOrFirewall
  - 镜像无法使用/镜像问题/环境不对 → 调用 DiagnoseImageIssue
```

### 4.6 新增 Diagnosis 单元测试

**新文件**:
- `internal/diagnosis/port_firewall_test.go`
- `internal/diagnosis/image_issue_test.go`

### 验证

- `go build ./...`
- `go test ./internal/diagnosis/...` — 全绿

---

## Phase 5: 安全与脱敏

**依赖**: Phase 2（JupyterToken 工具）、Phase 3（ResetPassword workflow）

### 5.1 新建 sanitizer 包

**新文件**: `internal/sanitizer/sanitizer.go`

```go
package sanitizer

// Sanitize 对 tool result 进行脱敏，在进入 LLM 上下文前调用
func Sanitize(action string, result map[string]any) map[string]any

// SanitizeArgs 对 workflow/diagnosis args 脱敏，防止密码等敏感信息进入事件回调
func SanitizeArgs(action string, args map[string]any) map[string]any
```

**脱敏规则**:
- 按 action 名匹配已知敏感字段:
  - `DescribeCompShareJupyterToken`: `JupyterToken` → `"[已获取,请通过安全通道查看]"`（进 LLM 的版本）
  - `ResetCompShareInstancePassword`: `Password` → `"[已设置]"`
- 通用规则: 扫描所有 key，匹配 `Password`、`PrivateKey`、`SecretKey`、`Token`、`AccessKey` 等模式 → 替换为 `"[REDACTED]"`
- 返回深拷贝，不修改原 map

**双通道设计（JupyterToken 专项）**:
- `Sanitize()` 返回脱敏后的 result → 进入 LLM history 和日志
- 原始 result 中的 JupyterToken → 通过 `onStep` 回调安全输出到 CLI 终端
- 效果：LLM 上下文中不含真实 token，但用户在终端能看到并使用

**实现涉及 3 个文件**:

1. **`internal/engine/engine.go`**: `StepEvent` 结构体新增 `Display string` 字段，用于承载需要终端展示但不进 LLM 的内容。在 `executeTool` 中，对 `DescribeCompShareJupyterToken` 的 result：先提取原始 JupyterToken → 写入 `StepEvent.Display` → 再调 `sanitizer.Sanitize()` 脱敏 → 脱敏后的 result 进 LLM history。

2. **`cmd/agent.go`**: 修改 `onStep` 回调。当前 `StepToolResult` 只打印 `"✅ {action} 调用成功"`，需要增加：若 `ev.Display != ""` 则额外输出 `ev.Display` 到终端（如 `"🔑 Jupyter Token: xxx"`）。

3. **`internal/sanitizer/sanitizer.go`**: `Sanitize` 函数对 JupyterToken 替换为占位符（进 LLM 的版本）。

**验收口径统一**: LLM 上下文中 token 被脱敏（不含真实值），CLI 终端通过 Display 字段安全展示原值。评测/金标脚本验证的是"LLM 不泄露 token"，不是"CLI 不展示 token"。

### 5.2 在 engine 中插入脱敏

**文件**: `internal/engine/engine.go`

3 个插入点：

1. **外部 API 工具结果**（`executeTool` 方法，约 L219）:
   ```go
   result = sanitizer.Sanitize(action, result)
   formatted := prompt.FormatToolResult(result)
   ```

2. **Workflow 事件回调**（`executeWorkflow` 方法，StepEvent 回调中）:
   ```go
   ev.Args = sanitizer.SanitizeArgs(ev.Tool, ev.Args)
   ```

3. **Diagnosis 事件回调**（`executeDiagnosis` 方法，DiagEvent 回调中）:
   ```go
   ev.Args = sanitizer.SanitizeArgs(ev.Tool, ev.Args)
   ```

### 5.3 ResetPassword 确认步骤脱敏

**文件**: `internal/workflow/reset_password.go`

在 `stepConfirmReset` 的 `BuildArgs` 中，返回的 args map 里 Password 字段写 `"[已设置,不显示]"`。

### 5.4 新增安全级别（如有缺失）

**文件**: `internal/security/levels.go` — 确认所有 Phase 2-4 新增的 API Action（被 workflow/diagnosis 内部调用的底层 Action）都有正确级别。需检查:
- `RebootCompShareInstance` → L1 ✓ (已存在)
- `ModifyCompShareInstanceName` → L1 ✓ (已存在)
- `ResetCompShareInstancePassword` → L1 ✓ (已存在)

### 5.5 新增 sanitizer 测试

**新文件**: `internal/sanitizer/sanitizer_test.go`

测试:
- JupyterToken 被替换
- Password 被替换
- 非敏感字段不变
- 通用模式匹配（含 "PrivateKey" 的字段被捕获）
- 深拷贝验证（原 map 不被修改）

### 验证

- `go build ./...`
- `go test ./internal/sanitizer/...`
- `go test ./internal/engine/...` — 确认现有测试不 break
- 手动 CLI 测试: 请求 Jupyter Token，确认 LLM 回复中不含真实 token（脱敏），同时 CLI 终端 Display 行展示了原始 token

---

## Phase 6: 评测扩展与全量验证

**依赖**: Phase 0-5 全部完成

### 6.1 扩展离线评测用例 (35→60)

**文件**: `eval/cases.json`

分布:
| 类别 | 现有 | 新增 | 目标 | 新增覆盖内容 |
|------|------|------|------|-------------|
| simple_query | 8 | +6 | 14 | 自制镜像查询、社区镜像查询、JupyterToken、用户折后价、DescribeCompShareSoftwarePort 端口查询、DescribeCompShareCustomImages 自制镜像 |
| knowledge_qa | 8 | +8 | 16 | 8 个知识主题各 2 题（镜像选择、登录、防火墙/端口、云硬盘、公共模型库、网络加速、无卡模式、计费/回收） |
| complex_task | 7 | +5 | 12 | RebootWorkflow、RenameWorkflow、ResetPasswordWorkflow、社区镜像创建、指定计费方式创建 |
| diagnosis | 7 | +5 | 12 | DiagnosePortOrFirewall ×2、DiagnoseImageIssue ×2、额外 SSH/GPU 变体 |
| recommendation | 5 | +1 | 6 | 预算受限大模型推荐 |

### 6.2 更新 toolToIntent 映射

**文件**: `eval/evaluate_test.go`

新增映射:
- `RebootInstanceWorkflow`, `RenameInstanceWorkflow`, `ResetPasswordWorkflow` → complex_task
- `DiagnosePortOrFirewall`, `DiagnoseImageIssue` → diagnosis
- `DescribeCompShareCustomImages`, `DescribeCommunityImages`, `DescribeCompShareJupyterToken`, `GetCompShareInstanceUserPrice` → simple_query

### 6.3 扩展场景测试 (28→40)

**文件**: `internal/engine/scenario_test.go`

新增 12 个场景:

| # | 场景名 | 覆盖 |
|---|--------|------|
| 29 | TestScenario_RebootInstance_Success | Reboot workflow 正常路径 |
| 30 | TestScenario_RebootInstance_NotRunning | 非 Running 不可重启 |
| 31 | TestScenario_RenameInstance | Rename workflow |
| 32 | TestScenario_ResetPassword | ResetPassword workflow（含脱敏验证） |
| 33 | TestScenario_CreateInstance_CommunityImage | 社区镜像创建 |
| 34 | TestScenario_PortFirewall_ServiceNotFound | 端口诊断 — 服务不在列表 |
| 35 | TestScenario_PortFirewall_NotRunning | 端口诊断 — 实例未运行 |
| 36 | TestScenario_ImageIssue_InstallFail | 镜像诊断 — 初始化失败 |
| 37 | TestScenario_ImageIssue_CommunityImage | 镜像诊断 — 社区镜像问题 |
| 38 | TestScenario_SecurityBlock_L2 | L2 操作拒绝 (TerminateCompShareInstance) |
| 39 | TestScenario_Sanitize_JupyterToken | Jupyter Token 脱敏验证 |
| 40 | TestScenario_Sanitize_Password | 密码脱敏验证 |

### 6.4 新建 12 条 CLI 金标对话脚本

**新文件**: `eval/golden_scripts.md`

| # | 脚本 | 输入 | 预期行为 |
|---|------|------|----------|
| 1 | 创建实例 | "帮我开一台4090" | 触发 CreateInstanceWorkflow |
| 2 | 开机 | "把 uhost-xxx 开机" | 触发 StartInstanceWorkflow |
| 3 | 关机 | "关机" | 触发 StopInstanceWorkflow + 磁盘费用提醒 |
| 4 | 重启 | "重启实例" | 触发 RebootInstanceWorkflow |
| 5 | Jupyter Token | "查一下jupyter token" | 调用 DescribeCompShareJupyterToken + LLM 回复不含真实 token + CLI Display 行展示原始 token |
| 6 | 重置密码 | "重置密码" | 触发 ResetPasswordWorkflow + 密码脱敏 |
| 7 | SSH 诊断 | "SSH连不上" | 触发 DiagnoseSSH |
| 8 | 端口诊断 | "JupyterLab打不开" | 触发 DiagnosePortOrFirewall |
| 9 | 知识: 无卡模式 | "什么是无卡模式" | 从知识包回答，包含 0.15 元/时、限 1 台 |
| 10 | 知识: 网络加速 | "怎么加速github" | 从知识包回答，包含加速链接 |
| 11 | 安全拒绝 | "帮我删除这台实例" | L2 拒绝，引导到控制台 |
| 12 | 脱敏验证 | "获取 jupyter token" 后检查输出 | LLM assistant 回复不含真实 token 值，CLI Display 行有原始 token |

### 6.5 全量验证

执行顺序:

```bash
# 1. 编译
go build ./...

# 2. 全部单元测试
go test ./...

# 3. 离线评测（主模型）
go test ./eval/ -run TestEval -model Qwen3-32B -timeout 20m

# 4. 离线评测（备选模型）
go test ./eval/ -run TestEval -model Qwen3-Max -timeout 20m

# 5. 手动跑 12 条金标脚本
```

### 通过标准

| 指标 | 阈值 |
|------|------|
| intent accuracy | >= 95% (57/60) |
| tool/workflow routing accuracy | >= 95% |
| critical P0 cases | 100% |
| 12 条 CLI 金标 | 12/12 通过 |
| `go test ./...` | 全绿 |

---

## 实际执行顺序（串行，避免热点文件冲突）

逻辑上部分 Phase 可并行，但 `registry.go`、`builder.go`、`scenario_test.go`、`cases.json` 是多个 Phase 共享的热点文件。按文件 ownership 串行推进更稳。

```
步骤 1: Phase 0 + Phase 2  → 打通路由 + 工具骨架
         热点文件: registry.go, builder.go
         
步骤 2: Phase 3 + Phase 5  → Workflow + 脱敏
         热点文件: registry.go (workflow tools), engine.go, 新建 workflow 文件 + sanitizer
         
步骤 3: Phase 4            → Diagnosis
         热点文件: registry.go (diagnosis tools), 新建 diagnosis 文件
         
步骤 4: Phase 1            → 知识层
         热点文件: faq.go (独占)
         放在最后补知识，避免中途改动影响测试基线
         
步骤 5: Phase 6            → 统一评测
         热点文件: cases.json, evaluate_test.go, scenario_test.go, golden_scripts.md
         所有功能就绪后一次性扩评测 + 全量验证
```

**原则**: 先打通工具+路由骨架 → 再补 workflow+脱敏 → 再诊断 → 再知识层 → 最后统一评测。不反复改测试基线。

## Block A 退出硬条件

同时满足以下 4 条才能冻结 Block A，切入 Block B：

1. 知识包覆盖 8 个主题并完成冲突清洗
2. 工具数达到 2 + 10 + 6 + 6 = 24
3. 主模型评测 intent >= 95%、tool >= 95%、critical P0 = 100%
4. 12 条 CLI 金标对话全通过

---

## 涉及的所有文件清单

### 修改 (11 个文件)
- `internal/tools/registry.go` — 工具描述修复 + 4 API + 3 workflow + 2 diagnosis 工具定义
- `internal/prompt/builder.go` — 路由规则 + UserPrice 路由 + 新 workflow/diagnosis 路由
- `internal/prompt/faq.go` — 8 主题知识包替换
- `internal/workflow/registry.go` — 注册 3 新 workflow
- `internal/workflow/create_instance.go` — 社区镜像支持（含 stepGetPrice 修改）
- `internal/diagnosis/registry.go` — 注册 2 新 diagnosis
- `internal/engine/engine.go` — 插入 sanitizer 调用 + StepEvent 新增 Display 字段 + JupyterToken 双通道
- `cmd/agent.go` — onStep 回调支持 Display 字段输出
- `eval/cases.json` — 35→60 用例
- `eval/evaluate_test.go` — toolToIntent 映射更新
- `internal/engine/scenario_test.go` — 28→40 场景

### 新建 (13 个文件)
- `internal/workflow/reboot_instance.go`
- `internal/workflow/rename_instance.go`
- `internal/workflow/reset_password.go`
- `internal/workflow/reboot_instance_test.go`
- `internal/workflow/rename_instance_test.go`
- `internal/workflow/reset_password_test.go`
- `internal/diagnosis/port_firewall.go`
- `internal/diagnosis/image_issue.go`
- `internal/diagnosis/port_firewall_test.go`
- `internal/diagnosis/image_issue_test.go`
- `internal/sanitizer/sanitizer.go`
- `internal/sanitizer/sanitizer_test.go`
- `eval/golden_scripts.md`
