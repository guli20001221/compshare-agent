---
name: diagnose_gpu_not_detected
description: Use this skill when user reports nvidia-smi error / GPU not detected / CUDA unavailable on a CompShare instance and needs read-only platform-side triage
triggers:
  - "nvidia-smi 报错"
  - "GPU 不识别"
  - "CUDA 不可用"
  - "GPU 检测不到"
  - "nvidia-smi 找不到设备"
applicable_tiers: [agent]
required_tools:
  - DescribeCompShareInstance
  - GetCompShareInstanceMonitor
related_skills:
  - safety_warning
body_cap_lines: 100
verification_status: unverified
field_refs_verified: false
provenance: human_authored
---

# Diagnose: GPU Not Detected

> 来源:从原 `GPUNotDetectedChain` Go 代码 SOP 提炼(2026-05-29),未经真机故障验证。

## 排查步骤

1. **查实例状态 + GPU 配置**(call `DescribeCompShareInstance` with UHostId)
   - `State == "Stopped"` → 关机状态无法检测 GPU,引导用户控制台开机
   - `State == "Install"` → 实例初始化中(2-3 分钟),等待
   - `State == "Install Fail"` → 初始化失败,建议删除重建
   - `State == "Starting" / "Stopping" / "Rebooting"` → 过渡状态(1-2 分钟),等待
   - `State == "Running"` 且 `GPU == 0` → **无卡模式**:实例启动时未分配 GPU,nvidia-smi 必然无法识别,引导用户关机后以正常模式重新开机
   - `State == "Running"` 且 `GPU > 0` → 进步骤 2
   - 其他 state → 异常,引导控制台检查

2. **(可选,引导用户自查 kernel 日志)** —— 平台侧没有 dmesg 工具,**不要尝试自行调用 dmesg**;若步骤 1/3 未定位问题,提示用户在实例内(JupyterLab 终端)只读运行 `dmesg --since='5 minutes ago' | grep -i nvidia` 并回报结果
   - 用户回报含 `NVRM:` / `nvidia` 错误 → 内核日志已记录驱动问题,引导用户提供完整 dmesg 给客服
   - 用户回报无 nvidia 相关错误,或暂不自查 → 继续看步骤 3 的云侧监控

3. **查 GPU 监控指标**(call `GetCompShareInstanceMonitor` with UHostId)
   - 看 `cloudwatch_gpu_util` + `cloudwatch_gpu_memory_usage` 最新值
   - `gpu_util > 0 || gpu_memory > 0` → 云侧 GPU 工作正常,问题可能在容器内驱动版本/镜像配置;引导用户核对镜像和驱动环境
   - `gpu_util` 跟 `gpu_memory` 监控均无数据 → **不能下"健康"结论**(memory `pr2_5_联调_2026_05_28` 教训),输出"无法确认 GPU 状态",建议用户实例内只读自查 `nvidia-smi`
   - 监控数据完整但都为 0 → fallback verdict

## Pitfalls

- **无卡模式 ≠ 硬件问题**:用户配置实例时可能选了无卡启动(省钱),此时 nvidia-smi 当然找不到设备,这不是故障
- **监控数据空 ≠ healthy**:监控未返回不能等价 GPU 健康,必须明确输出"无法确认"(memory `pr2_5_联调_2026_05_28`)
- **容器内驱动 vs 云侧硬件**:云侧监控显示 GPU 活动正常但用户报 nvidia-smi 报错 → 大概率容器内驱动版本不匹配或 LD_LIBRARY_PATH 问题,引导用户检查镜像
- **State 字段大小写敏感**:平台 API 返回 `"Stopped"` 不是 `"stopped"`,switch 必须精确匹配

## 兜底建议

云侧监控未发现明确问题时:引导用户实例内只读自查 `nvidia-smi`;如命令报错,通过控制台重启实例;仍异常则联系技术支持并提供实例 ID。**不主动建议修改驱动 / 重装内核**(超出 read-only 边界)。
