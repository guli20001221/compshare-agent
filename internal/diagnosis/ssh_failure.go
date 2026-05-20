package diagnosis

import (
	"fmt"
	"strings"
)

// SSHFailureChain returns the diagnostic chain for SSH connection failures.
// Flow: check instance state/login command -> check resource usage -> fallback.
func SSHFailureChain() *Chain {
	return &Chain{
		Name:        "DiagnoseSSH",
		Description: "诊断 SSH 连接失败：检查实例状态与 SSH 登录入口 → 检查资源使用 → 兜底建议",
		Steps: []Step{
			stepCheckInstanceState(),
			stepCheckResourceUsage(),
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "云侧未发现明确的 SSH 连接问题。实例运行中，已返回 SSH 登录入口，CPU/内存监控未见高压。",
			Suggestion: "请先使用控制台展示的 SSH 登录命令重试。若能通过 JupyterLab 进入终端，可用只读命令自查：`systemctl status ssh --no-pager`、`ss -lntp | grep ':22'`。如仍无法连接，请联系技术支持并提供实例 ID。",
		},
	}
}

func stepCheckInstanceState() Step {
	return Step{
		Name: "检查实例状态",
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			id, err := dCtx.RequireUHostId()
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"UHostIds": []any{id},
			}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			hosts, _ := result["UHostSet"].([]any)
			if len(hosts) == 0 {
				return Verdict{
					Action:     Conclude,
					Conclusion: "未找到该实例，可能已被释放或 ID 输入有误。",
					Suggestion: "请确认实例 ID 是否正确，可以告诉我「查看我的实例」来查看当前实例列表。",
				}
			}
			host, _ := hosts[0].(map[string]any)
			state, _ := host["State"].(string)

			switch state {
			case "Stopped":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例当前处于关机状态，无法进行 SSH 连接。",
					Suggestion: "需要先在控制台开机后才能 SSH 连接。",
				}
			case "Install":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在初始化中，尚未就绪。初始化通常需要 2-3 分钟。",
					Suggestion: "请耐心等待初始化完成后再尝试 SSH 连接。",
				}
			case "Install Fail":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例初始化失败，无法正常使用。",
					Suggestion: "建议删除重建该实例。如果问题反复出现，请联系客服。",
				}
			case "Starting":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在启动中，请稍等片刻。",
					Suggestion: "启动通常需要 1-2 分钟，完成后即可 SSH 连接。",
				}
			case "Stopping":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在关机中，无法 SSH 连接。",
					Suggestion: "请等待关机完成后，再到控制台开机。",
				}
			case "Rebooting":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在重启中，请稍等片刻。",
					Suggestion: "重启通常需要 1-2 分钟，完成后即可 SSH 连接。",
				}
			case "Running":
				if isWindowsHost(host) {
					return Verdict{
						Action:     Conclude,
						Conclusion: "这是 Windows 实例，不适用 Linux SSH 登录方式。",
						Suggestion: "请使用控制台提供的 Windows RDP 入口，或本地打开 mstsc 进行远程桌面连接。",
					}
				}
				if sshLoginCommandFromHost(host) == "" {
					return Verdict{
						Action:     Conclude,
						Conclusion: "云侧未返回 SSH 登录命令，无法确认该实例已有可用 SSH 登录入口。",
						Suggestion: "请先在控制台核对实例登录入口和公网 IP。若能通过 JupyterLab 进入终端，可用只读命令自查：`systemctl status ssh --no-pager`、`ss -lntp | grep ':22'`。安装或启动 SSH 服务属于会修改实例环境的可选修复，请确认后再执行。",
					}
				}
				return Verdict{Action: Continue}
			default:
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例当前状态为「" + state + "」，可能处于异常状态。",
					Suggestion: "请到控制台查看实例详情或联系客服。",
				}
			}
		},
	}
}

func stepCheckResourceUsage() Step {
	return Step{
		Name: "检查资源使用",
		Tool: "GetCompShareInstanceMonitor",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			id, err := dCtx.RequireUHostId()
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"UHostIds": []any{id},
			}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			cpuUsage, memUsage, cpuOK, memOK := extractLatestMetrics(result)
			// 90% catches degradation earlier than the previous 95% cutoff,
			// which left users at 90-94% with a contradictory "resources normal"
			// verdict while their SSH was already timing out.
			const threshold = 90.0

			if cpuUsage >= threshold || memUsage >= threshold {
				detail := ""
				if cpuUsage >= threshold {
					detail += fmt.Sprintf("CPU 使用率 %.1f%%", cpuUsage)
				}
				if memUsage >= threshold {
					if detail != "" {
						detail += "，"
					}
					detail += fmt.Sprintf("内存使用率 %.1f%%", memUsage)
				}
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例资源耗尽：" + detail + "。系统资源不足可能导致 SSH 无法响应。",
					Suggestion: "建议通过控制台重启实例释放资源，或升级到更高配置。",
				}
			}
			if !cpuOK || !memOK {
				return Verdict{
					Action:     Conclude,
					Conclusion: "监控未返回 CPU/内存数据，无法确认资源是否耗尽。",
					Suggestion: "云侧已确认实例运行并返回 SSH 登录入口，但监控数据不完整。若能通过 JupyterLab 进入终端，可用只读命令自查：`free -h`、`uptime`、`top -b -n 1 | head`。如仍无法登录，请联系技术支持并提供实例 ID。",
				}
			}
			return Verdict{Action: Continue}
		},
	}
}

// extractLatestMetrics gets the latest CPU and memory usage from monitor data.
func extractLatestMetrics(result map[string]any) (cpu, mem float64, cpuOK, memOK bool) {
	data, _ := result["Data"].(map[string]any)
	if data == nil {
		return 0, 0, false, false
	}
	list, _ := data["List"].([]any)
	if len(list) == 0 {
		return 0, 0, false, false
	}
	instance, _ := list[0].(map[string]any)
	metrics, _ := instance["Metrics"].([]any)

	for _, m := range metrics {
		metric, _ := m.(map[string]any)
		key, _ := metric["MetricKey"].(string)
		val, ok := latestValue(metric)
		if !ok {
			continue
		}
		switch key {
		case "uhost_cpu_used":
			cpu = val
			cpuOK = true
		case "cloudwatch_memory_usage":
			mem = val
			memOK = true
		}
	}
	return cpu, mem, cpuOK, memOK
}

// latestValue extracts the most recent value from a metric's Results.
func latestValue(metric map[string]any) (float64, bool) {
	results, _ := metric["Results"].([]any)
	if len(results) == 0 {
		return 0, false
	}
	first, _ := results[0].(map[string]any)
	values, _ := first["Values"].([]any)
	if len(values) == 0 {
		return 0, false
	}
	last, _ := values[len(values)-1].(map[string]any)
	val, _ := last["Value"].(float64)
	return val, true
}

func firstHostFromResult(instanceResult map[string]any) (map[string]any, bool) {
	hosts, _ := instanceResult["UHostSet"].([]any)
	if len(hosts) == 0 {
		return nil, false
	}
	host, ok := hosts[0].(map[string]any)
	return host, ok
}

func sshLoginCommandFromHost(host map[string]any) string {
	cmd, _ := host["SshLoginCommand"].(string)
	return strings.TrimSpace(cmd)
}

func isWindowsHost(host map[string]any) bool {
	osType, _ := host["OsType"].(string)
	return strings.EqualFold(strings.TrimSpace(osType), "Windows")
}
