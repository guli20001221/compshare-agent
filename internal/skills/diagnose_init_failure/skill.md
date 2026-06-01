---
name: diagnose_init_failure
description: Use this skill when user reports CompShare instance stuck in Install / Install Fail state, or instance not starting after creation, and needs read-only platform-side triage
triggers:
  - "实例没启动"
  - "实例初始化失败"
  - "Install Fail"
  - "实例创建后没起来"
  - "实例卡在 Install"
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

# Diagnose: Instance Init Failure

> 来源:从原 `InitFailureChain` Go 代码 SOP 提炼(2026-05-29),未经真机故障验证。

## 排查步骤

**Mode A — 用户指定了 UHostId**

1. **查单实例状态**(call `DescribeCompShareInstance` with UHostIds=[id])
   - `State == "Install Fail"` → 初始化失败,建议删除重建;如反复出现联系客服
   - `State == "Install"` → 初始化中(2-3 分钟 normal),等待
   - `State == "Starting"` → 启动中(1-2 分钟),等待
   - `State == "Running"` → 实际已就绪,可能用户感知有滞后,引导验证
   - 其他 state → 状态异常,引导控制台

**Mode B — 用户未指定 UHostId(说"我的实例没启动")**

1. **扫描所有实例**(call `DescribeCompShareInstance` with Limit=100)
2. 按 State 分组:
   - `Install Fail` 列表 → 高优先级输出,逐个列(含镜像名)
   - `Install` 列表 → 中优先级,提示正在初始化中
   - `Starting` 列表 → 低优先级,提示启动中
   - 其他 Running 状态 → 计数即可,不展开
3. 总结:"X 个实例初始化失败、Y 个初始化中、Z 个启动中、其余正常"

**辅助:可选引导用户自查 kernel 日志**(平台侧没有 dmesg 工具,**不要尝试自行调用 dmesg**;仅当用户指定 UHostId 且实例 Running 时,提示用户在实例内 JupyterLab 终端只读运行 `dmesg --since='10 minutes ago'` 并回报)
- 用户回报含 `Out of memory:` / `Killed process` → 内存爆,引导用户升配或检查应用占用
- 用户回报含 `EXT4-fs error` / `I/O error` → 磁盘问题,引导联系客服

## Pitfalls

- **Install vs Install Fail 区分**: `Install` 是正常过渡状态(等待即可),`Install Fail` 是错误状态(必须删除重建);UI 文案不要混
- **多实例扫描必须列出所有异常实例**:不能只展示前 N 个(memory `transcript-head-truncation` 教训:头截断丢实例)
- **社区镜像 vs 官方镜像 init 失败原因不同**:社区镜像更易因脚本错误失败 → 建议换官方镜像;官方镜像失败更可能平台层问题 → 联系客服
- **Limit=100 兜底**:API default Limit=20,大客户可能有几十个实例,主动设 100 防止漏

## 兜底建议

无法确定初始化状态时:引导用户控制台查实例详情;reproducible 失败建议联系客服并提供实例 ID + 镜像名。**不主动建议重装系统**(用户数据可能未备份)。
