---
name: diagnose_port_firewall
description: Use this skill when user reports a CompShare instance service port unreachable (JupyterLab / FileBrowser / custom apps) — for SSH failure use diagnose_ssh instead
triggers:
  - "端口不通"
  - "服务访问不到"
  - "JupyterLab 进不去"
  - "FileBrowser 无法访问"
  - "应用端口被防火墙拦"
applicable_tiers: [agent]
required_tools:
  - DescribeCompShareInstance
  - DescribeCompShareSoftwarePort
related_skills:
  - safety_warning
body_cap_lines: 100
verification_status: unverified
field_refs_verified: false
---

# Diagnose: Port / Service Reachability

> 来源:从原 `PortFirewallChain` Go 代码 SOP 提炼(2026-05-29),未经真机故障验证。

## 排查步骤

1. **查实例状态 + 应用列表**(call `DescribeCompShareInstance` with UHostId)
   - `State != "Running"` → 未运行,引导用户开机后访问
   - `State == "Running"` → 拿 `Softwares` 字段(实例级,只对容器镜像填充),进步骤 2
2. **识别用户问的具体服务**
   - `Service == "SSH"` → **本 skill 不处理 SSH**(SSH 排查方法跟端口排查不同:`SshLoginCommand` 字段不走 SoftwarePort,memory `pr2_5_联调_2026_05_28` line 470 教训)。输出说明 + 建议用户重新表述为"SSH 连不上"让 planner 重新分类到 diagnose_ssh skill。**不调用 diagnose_ssh skill**(ADR-004 未定义 skill→skill delegation 机制)
   - `Service` 是常见应用名(JupyterLab / FileBrowser 等):
     - **优先级 1**: 在步骤 1 拿到的实例级 `Softwares` 里找,有就返回 URL
     - **优先级 2**: 找不到再 call `DescribeCompShareSoftwarePort` 查平台目录,作为参考(说"该镜像默认支持但本实例未启用,建议用户控制台检查应用入口")
   - `Service` 是自定义名(用户自己装的服务) → 平台无法判断,引导用户实例内 `ss -tlnp` 自查(只读),引导控制台检查公网 IP 和安全组

## Pitfalls

- **SSH 路由必须走 SshLoginCommand**:`DescribeCompShareSoftwarePort` 返回的是镜像应用层端口,不返回 SSH 端口;用 SoftwarePort 排查 SSH 是错路由
- **Softwares 字段只在 Running 容器实例填充**:Windows / 非容器化镜像可能为空,此时只能走 SoftwarePort 平台目录
- **未指定服务时不要瞎猜**:用户说"端口不通"未指定服务,不要默认猜 22 或 80,引导用户明说服务名
- **不主动建议改 iptables / firewalld**:这些是 mutating + 需 sudo,超出 read-only 边界,绝对作为"可选修复"标注

## 兜底建议

未指定具体服务时,引导用户说明服务名;指定后云侧未找到时,建议用户实例内 `ss -tlnp \| grep '<port>'` 自查 + 控制台核对公网 IP 与安全组配置。**自定义服务排查不超出 read-only 命令边界**(memory 决策原则)。
