---
name: diagnose_ssh
description: Use this skill when user reports SSH connection failure / timeout / Permission denied on a CompShare instance and needs read-only platform-side triage
triggers:
  - "SSH 连不上"
  - "SSH 连接超时"
  - "Permission denied"
  - "SSH 连接失败"
  - "无法登录实例"
applicable_tiers: [agent]
required_tools:
  - DescribeCompShareInstance
  - GetCompShareInstanceMonitor
related_skills:
  - safety_warning
body_cap_lines: 100
verification_status: unverified
field_refs_verified: false
---

# Diagnose: SSH Connection Failure

> 来源:从原 `SSHFailureChain` Go 代码 SOP 提炼(2026-05-29),未经真机故障验证。

## 排查步骤

1. **查实例状态 + SSH 入口**(call `DescribeCompShareInstance` with UHostId)
   - State 走 GPU not-detected 同套 branching(Stopped / Install / Install Fail / Starting / Stopping / Rebooting → 直接 conclude)
   - `State == "Running"` 后还要检查两件事:
     - `OsType == "Windows"` → **Windows 实例不适用 SSH**,引导用户用 RDP / mstsc
     - `SshLoginCommand` 字段为空 → 云侧未配置 SSH 入口,引导用户控制台核对登录入口和公网 IP;若用户有 JupyterLab 入口,可让用户在终端跑 `systemctl status ssh --no-pager` + `ss -lntp \| grep ':22'`
     - 上述都通过 → 进步骤 2

2. **查资源使用**(call `GetCompShareInstanceMonitor` with UHostId)
   - 看 `uhost_cpu_used` + `cloudwatch_memory_usage` 最新值
   - 阈值 **90%**(不是 95%,90-94% 区间已经会导致 SSH 超时,memory 教训)
   - CPU 或 Memory ≥ 90% → 资源耗尽,引导用户控制台重启实例释放资源,或建议升配
   - CPU 跟 Memory 监控均无数据 → 输出"无法确认资源状态"(不能 fall through 到 healthy),引导用户 JupyterLab 终端 `free -h` / `uptime` / `top -b -n 1 \| head`
   - 监控数据完整但都正常 → fallback verdict

## Pitfalls

- **SSH 入口来源**:`SshLoginCommand` 是 `DescribeCompShareInstance` 的字段,**不是** `DescribeCompShareSoftwarePort`(后者只返回镜像应用端口,不返回 SSH);memory `pr2_5_联调_2026_05_28` line 470 错路由教训
- **Windows 实例**:必须先检查 `OsType`,Linux SSH 路径不适用 Windows
- **阈值 90% 不是 95%**:90-94% 已经能导致 SSH timeout(原 Chain 注释明确记录),不能放松到 95%
- **监控数据空 ≠ healthy**:memory `pr2_5_联调_2026_05_28` 0%/healthy 教训

## 兜底建议

云侧未发现明确问题时:引导用户先用控制台 SSH 入口重试;若可 JupyterLab 进入终端,引导只读自查;仍异常联系技术支持并提供实例 ID。**不主动建议重装 SSH 服务**(超出 read-only 边界,只能作为"可选修复"提示)。
