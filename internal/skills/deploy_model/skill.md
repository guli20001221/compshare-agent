---
name: deploy_model
description: Use this skill when the user wants to RUN or DEPLOY a model, framework, or application on CompShare (部署/跑/搭 + a workload name) and needs a suitable GPU instance created for it — the agent picks the image and sizes the GPU
triggers:
  - "部署 Qwen / Llama / 模型"
  - "我想跑数字人"
  - "搭一个能跑 ComfyUI 的环境"
  - "部署模型做推理服务"
  - "帮我起一个跑 SD 的实例"
applicable_tiers: [agent]
required_tools:
  - DescribeCompShareImages
  - DescribeCommunityImages
  - GetModelVRAMRequirement
  - GetGPURecommendation
  - CheckCompShareResourceCapacity
  - GetCompShareInstanceUserPrice
  - CreateCompShareInstance
  - DescribeCompShareInstance
related_skills:
  - safety_warning
body_cap_lines: 100
verification_status: unverified
field_refs_verified: true
provenance: human_authored
---

# Deploy: 按工作负载选镜像建实例

> 来源:B8.3 实现(2026-05-31)的 `internal/engine/deploy_model.go` arm + `workflow.CreateInstanceDef`。字段名已对上游 `uhost-compshare-api-master` 源码核实;**端到端未经真机验证**(B8.4 跑通后升级为 spike_validated)。

## 何时用 / 何时不用

- **用 deploy_model**:用户说"部署/跑/搭 + 某个模型/框架/应用"(部署 Qwen2.5-32B / 跑数字人 / 搭一个能跑 ComfyUI 的环境),或问"跑 X 用哪个卡合适"——都先**出推荐**,写操作开时再经确认卡片建实例。由 agent 选镜像 + 定 GPU + 选可用区。
- **不要用**:对**已有实例**的操作(关机/启动/重启/变配/加盘)走 `operation_lifecycle`;用户直接指定硬件规格的"创建一个 4090 单卡实例"(spec-first)也走 `operation_lifecycle`,不是 deploy_model。

## 编排步骤(arm 已实现,非手动)

1. **选型(TierAgent,grounded,两步 LLM)**:① 先让强模型从需求里抽一个社区检索关键词(理解模糊措辞,lead Q1);② 活查平台镜像 `DescribeCompShareImages`(`Limit=100` 取全量 ~68;**平台既有框架底座也有 App 类镜像** ComfyUI/vLLM/Ollama/SGLang)+ 社区镜像 `DescribeCommunityImages`(~743 组,按关键词 `FuzzySearch` 取相关 shortlist,`ExcludeReadme` 省 token)→ 强模型从**真实候选清单**按 Name/Framework/Description 选 `image_source`+`image_name`(禁止编造,落选回退平台框架镜像)。
2. **定 GPU + 选区(确定性,镜像/可用区感知 `selectDeployZoneAndGPU`)**:**zone-preference-first**——按 `deployPreferredZones`(cn-wlcb-01→cn-sh2-02;用户指定 zone 则严格只用该 zone)逐区:`ParseAvailableGPUs(avail, zone)` 取该区 live 卡 → `RecommendGPUTypeLive`(显存算术×1.2 buffer，∩ 镜像 `SupportedGpuTypes`,M2)→ 该卡在该区**真实有货**(`CheckCompShareResourceCapacity` 找 `Gpu==1 & ResourceEnough`)才选定;主区售罄**自动 fallback** 下一区(create 前预选,非 ADR-006 禁的 retry),并记 `FallbackNote`。用户指定 zone 无货/无合适卡 → 报错不静默改。全区无确认库存 → 用主区 sizing 交 saga 兜底;avail 空 → 静态表兜底。
3. **建议 or 建实例**:**只读模式**(写操作关)→ `buildAdviseReply` 只回推荐(GPU/镜像/可用区/选型说明),**不建实例**;**写操作开**→ 复用 `CreateInstanceDef` 经 `RunAgentSaga` 传 `{GpuType, ImageSource, ImageName, CompShareImageId, Zone, FallbackNote}`(**回传已解析镜像 ID + 选定 zone**,保证 saga 建的就是选型同源):查配比 → 检查库存(售罄即停)→ 查价 → **确认卡片(StepConfirm 唯一 HITL 门;args 带 GpuType/Gpu/CPU/Memory/Zone/image/price/FallbackNote,CLI 渲染成卡片、HTTP 经 confirmationEvent.Summary 给前端)** → `CreateCompShareInstance` → 单次 describe。
4. **轮询(handler 内有界循环)**:`DescribeCompShareInstance` 读 `UHostSet[0].State` 直到 `"Running"`,有界 N 轮短读(**轮询耗尽≠失败**,实例已建,慢=还在起)。
5. **取用法(成功后只读一次,按 id 回查选中镜像)**:`fetchImageUsage` 取 `SoftwarePorts`(app↔端口)+`FirewallPorts`(额外端口,如 vLLM API :8000)+`AutoStart`+`Readme`(仅社区有)。访问地址由 **镜像端口 + 实例公网 IP 自行拼** `http://ip:port`,**不直接回显实例 `Softwares[].URL`**(可能带 Jupyter `?token=` 密钥)。社区 `Readme` 去 HTML/iframe/图片 markdown 后截断节选(不喂回 LLM,非注入面)。
6. **回报**:实例 ID + `GpuType` + 镜像 + `SshLoginCommand` + **访问地址/使用说明**。**永不回报 `Password`/`FileBrowserPassword`**(base64 密钥)。

## 关键字段(已核上游)

- **`Readme` 来源(2026-05-31 真机核实)**:平台镜像 `Readme` **恒空**(列表 0/68,单查 by id 也空)→ 平台用法看 `SoftwarePorts`;社区 `Readme` **有内容**(~1-2K markdown+HTML,列表/keyed-by-id 都返,仅 `ExcludeReadme=true` 时省略)→ matcher shortlist 设 `ExcludeReadme=true` 省 token,成功后按 id 单查(不设 `ExcludeReadme`)拿全文。
- **用法字段**:`SoftwarePorts[].{Software,Port}`(app↔端口,平台 App 50/68 有,如 ComfyUI:8188/SD-WebUI:7860;社区也有)+ `FirewallPorts[]`(额外开放 TCP,vLLM API:8000/SGLang:30000)+ `AutoStart`(社区常 true)。实例 `UHostSet[].Softwares[].{Name,URL}` 给 app→完整 URL(含公网 IP),**但 URL 可能含 Jupyter `?token=` 密钥→不直接回显**,改用 端口+IP 自拼。
- **平台镜像也含 App 类镜像**(2026-05-31 真机 recon:`ImageType` 分布 App:49/Other:10/System:9;ComfyUI/vLLM/Ollama/SGLang 都是平台 App 镜像 by Name)。`Softwares.Applications` 字段被客户面剥离,但**应用身份在 `Name`/`Description`** 上 matcher 看得到 → **不存在“平台只有框架、社区只有应用”的二分法**,按真实 Name/Framework 匹配。
- **`SupportedGpuTypes` 在列表响应里就有**(裸 GPU key 如 `4090`/`V100S`,与 gpuSpecs key 同形;可空/可单卡如 vLLM-5090);用于 M2 的 GPU∩镜像交集。
- **平台 server-side `Name` 模糊过滤对 CJK 规范名失效**(`comfyui`→0 命中)→ 平台取全量不做关键词过滤;社区 `FuzzySearch`(名+作者)有效 → 社区走关键词。
- 社区响应 = `CompshareImageGroup[].{ImageName, Data[].{CompShareImageId, SupportedGpuTypes}}`;**无 `Data[]` 时镜像 ID 解析为空**,create 步骤须 fail-loud(已加 guard)。
- 实例:`UHostSet[].{UHostId, State, GpuType, SshLoginCommand, Password, FileBrowserPassword, IPSet[], Softwares[]}`;`Password`/`FileBrowserPassword` 是 base64 密钥,**永不回显**;`Memory` 单位 MB;`CreateCompShareInstance` 返回 `UHostIds []`。

## Pitfalls

- **写操作默认关**:`CreateCompShareInstance` 是 L1 mutating,shipped 默认只读 → arm 前置 `mutatingToolsEnabled` 闸,关时直接友好拒绝,不浪费选型/查询。
- **不可逆永不自动**:`TerminateCompShareInstance` 是 L2,saga 永不自动执行(建失败不计费、建成功是用户资源);清理由用户在控制台决定(ADR-006 §决策2 Amendment)。
- **State 大小写敏感**:`"Running"`/`"Install Fail"` 精确匹配;含 "fail" 视为初始化失败(终止轮询),`Install`/`Starting` 是过渡态(继续轮询)。
- **GPU 选最小可承载卡**:可能选到当前售罄的卡 → saga 在库存检查停并报"创建未完成",由用户换规格重试(非 arm 自动改卡)。
