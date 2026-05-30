---
name: diagnose_image_issue
description: Use this skill when user suspects a CompShare instance image issue (image fails to install, community image misbehaving, image incompatible with application) and needs read-only platform-side triage
triggers:
  - "镜像问题"
  - "镜像无法启动"
  - "社区镜像有问题"
  - "怀疑镜像 bug"
  - "镜像装不上"
applicable_tiers: [agent]
required_tools:
  - DescribeCompShareInstance
related_skills:
  - safety_warning
body_cap_lines: 100
verification_status: unverified
field_refs_verified: false
provenance: human_authored
---

# Diagnose: Image Issue

> 来源:从原 `ImageIssueChain` Go 代码 SOP 提炼(2026-05-29),未经真机故障验证。

## 排查步骤

1. **查实例状态 + 镜像类型**(call `DescribeCompShareInstance` with UHostId)
   - 取字段:`State` / `CompShareImageName` / `CompShareImageType`
   - State branching:
     - `State == "Install Fail"` → 初始化失败,**镜像可能是 root cause**;输出镜像名,建议换官方镜像重建;社区镜像建议联系作者
     - `State == "Install"` → 镜像正在安装中(2-3 分钟),等待;>5 分钟仍未完成联系客服
     - `State == "Starting"` → 启动中(此状态不产生费用),等待 1-2 分钟
     - `State == "Running"` 且 `CompShareImageType == "Community"` → 实例已运行但用户报问题:**社区镜像由第三方维护,可能有兼容性问题**;建议联系镜像作者反馈或换官方镜像
     - `State == "Running"` 且 `CompShareImageType` 为官方类型 → 云侧实例正常但用户报应用问题:**这不证明镜像内应用 / 环境完全正常**;明确告诉用户云侧不能保证镜像内部,引导用户根据具体应用现象继续诊断,实例内只读自查版本/服务状态
     - 其他 state → 状态异常,引导控制台

## Pitfalls

- **Running 不代表镜像没问题**:`State == "Running"` 仅说明云侧实例进程在跑,**不证明镜像内应用层正常**(应用启动失败 / 环境变量错 / 版本不匹配等都可能 Running);verdict 文案必须显式 disclosure
- **社区 vs 官方镜像责任不同**:
  - 社区镜像问题 → 镜像作者负责,平台无法保证;引导联系作者或换官方镜像
  - 官方镜像问题 → 平台层责任,引导联系客服
- **Install Fail 不一定都是镜像锅**:可能是 GPU/CPU 配额不足、网络初始化失败等;镜像名只是 evidence 之一,不要 jump to conclusion
- **不主动建议重建实例**:重建会丢用户数据,必须作为"可选修复"标注 + 提醒备份

## 兜底建议

无法确定镜像问题时:建议用户尝试换官方系统镜像重建实例(标注"会丢实例内数据,请先备份")。**对官方镜像 Running 状态报应用问题的 case,不要轻易归因镜像**,引导用户提供具体应用错误信息(应用名 / 错误码 / 日志片段)。
