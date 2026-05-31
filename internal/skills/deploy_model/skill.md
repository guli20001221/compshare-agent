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

- **用 deploy_model**:用户说"部署/跑/搭 + 某个模型/框架/应用"(部署 Qwen2.5-32B / 跑数字人 / 搭一个能跑 ComfyUI 的环境),想要一台合适的实例来承载这个工作负载。由 agent 选镜像 + 定 GPU。
- **不要用**:对**已有实例**的操作(关机/启动/重启/变配/加盘)走 `operation_lifecycle`;用户直接指定硬件规格的"创建一个 4090 单卡实例"(spec-first)也走 `operation_lifecycle`,不是 deploy_model。

## 编排步骤(arm 已实现,非手动)

1. **选型(TierAgent,grounded)**:活查平台镜像 `DescribeCompShareImages`(框架底座:PyTorch/CUDA/ComfyUI/Ubuntu)+ 社区镜像 `DescribeCommunityImages`(开箱即用应用:数字人/视频生成,`ExcludeReadme` 省 token)→ 强模型从**真实候选清单**里选 `image_source`(platform/community)+ `image_name`(禁止编造,落选回退平台框架镜像)。
2. **定 GPU(确定性)**:`RecommendGPUType` = 有模型名走 `GetModelVRAMRequirement`(参数量×字节×1.2 buffer → 最小可承载单卡,放不下给多卡),无模型名走 `GetGPURecommendation`(场景关键词)。→ `GpuType`。
3. **建实例(orchestrator saga)**:复用 `CreateInstanceDef` 经 `RunAgentSaga` 传 `{GpuType, ImageSource, ImageName}`:查配比 → 检查库存(售罄即停)→ 查价 → **确认(StepConfirm 是唯一 HITL 门)** → `CreateCompShareInstance` → 单次 describe。
4. **轮询(handler 内有界循环)**:`DescribeCompShareInstance` 读 `UHostSet[0].State` 直到 `"Running"`,有界 N 轮短读(**轮询耗尽≠失败**,实例已建,慢=还在起)。
5. **回报**:实例 ID + `GpuType` + 镜像 + `SshLoginCommand`(含 IP/端口)。**永不回报 `Password`**(base64 密钥)。

## 关键字段(已核上游)

- 列表镜像无 `Readme`;平台单查(传 `CompShareImageId`)才返 `Readme` 且 base 镜像常空;社区列表默认带 `Readme`。
- 平台镜像 `Softwares.Applications` 被客户面剥离 → 平台只能按 `Framework` 粒度匹配,应用级靠社区镜像。
- 社区响应 = `CompshareImageGroup[].{ImageName, Data[].CompShareImageId}`;**无 `Data[]` 时镜像 ID 解析为空**,create 步骤须 fail-loud(已加 guard)。
- 实例:`UHostSet[].{UHostId, State, GpuType, SshLoginCommand, Password, IPSet[]}`;`Memory` 单位 MB;`CreateCompShareInstance` 返回 `UHostIds []`。

## Pitfalls

- **写操作默认关**:`CreateCompShareInstance` 是 L1 mutating,shipped 默认只读 → arm 前置 `mutatingToolsEnabled` 闸,关时直接友好拒绝,不浪费选型/查询。
- **不可逆永不自动**:`TerminateCompShareInstance` 是 L2,saga 永不自动执行(建失败不计费、建成功是用户资源);清理由用户在控制台决定(ADR-006 §决策2 Amendment)。
- **State 大小写敏感**:`"Running"`/`"Install Fail"` 精确匹配;含 "fail" 视为初始化失败(终止轮询),`Install`/`Starting` 是过渡态(继续轮询)。
- **GPU 选最小可承载卡**:可能选到当前售罄的卡 → saga 在库存检查停并报"创建未完成",由用户换规格重试(非 arm 自动改卡)。
