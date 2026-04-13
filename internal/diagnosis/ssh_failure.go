package diagnosis

import "fmt"

// SSHFailureChain returns the 3-step diagnostic chain for SSH connection failures.
// Flow: check instance state -> check SSH port -> check resource usage -> fallback.
func SSHFailureChain() *Chain {
	return &Chain{
		Name:        "DiagnoseSSH",
		Description: "诊断 SSH 连接失败：检查实例状态 → 检查 SSH 端口 → 检查资源使用 → 兜底建议",
		Steps: []Step{
			stepCheckInstanceState(),
			stepCheckSSHPort(),
			stepCheckResourceUsage(),
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "未发现明确的 SSH 连接问题。实例运行正常，SSH 端口已开放，资源使用正常。",
			Suggestion: "请检查您本地的网络连接，或尝试使用 JupyterLab 网页终端作为替代方案。SSH 登录命令可在实例详情中找到。",
		},
	}
}

func stepCheckInstanceState() Step {
	return Step{
		Name: "检查实例状态",
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{dCtx.Params["UHostId"]},
			}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			hosts, _ := result["UHostSet"].([]any)
			if len(hosts) == 0 {
				return Verdict{
					Action:     Conclude,
					Conclusion: "未找到该实例，可能已被释放或 ID 输入有误。",
					Suggestion: "请使用 DescribeCompShareInstance 查看当前实例列表，确认实例 ID。",
				}
			}
			host, _ := hosts[0].(map[string]any)
			state, _ := host["State"].(string)

			switch state {
			case "Stopped":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例当前处于关机状态，无法进行 SSH 连接。",
					Suggestion: "需要先开机才能 SSH 连接。可以使用 StartInstanceWorkflow 开机。",
				}
			case "Install":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在初始化中，尚未就绪。初始化通常需要 2-3 分钟。",
					Suggestion: "请耐心等待初始化完成后再尝试 SSH 连接。",
				}
			case "InstallFail":
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
					Suggestion: "请等待关机完成后，再使用 StartInstanceWorkflow 重新开机。",
				}
			case "Rebooting":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在重启中，请稍等片刻。",
					Suggestion: "重启通常需要 1-2 分钟，完成后即可 SSH 连接。",
				}
			case "Running":
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

func stepCheckSSHPort() Step {
	return Step{
		Name: "检查 SSH 端口",
		Tool: "DescribeCompShareSoftwarePort",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			ports, _ := result["SoftwarePort"].([]any)
			for _, p := range ports {
				port, _ := p.(map[string]any)
				software, _ := port["Software"].(string)
				if software == "SSH" {
					return Verdict{Action: Continue}
				}
			}
			return Verdict{
				Action:     Conclude,
				Conclusion: "SSH 端口未开放。平台当前软件端口列表中没有 SSH 服务。",
				Suggestion: "请联系客服开放 SSH 端口，或使用 JupyterLab 网页终端替代。",
			}
		},
	}
}

func stepCheckResourceUsage() Step {
	return Step{
		Name: "检查资源使用",
		Tool: "GetCompShareInstanceMonitor",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{dCtx.Params["UHostId"]},
			}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			cpuUsage, memUsage := extractLatestMetrics(result)
			const threshold = 95.0

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
			return Verdict{Action: Continue}
		},
	}
}

// extractLatestMetrics gets the latest CPU and memory usage from monitor data.
func extractLatestMetrics(result map[string]any) (cpu, mem float64) {
	data, _ := result["Data"].(map[string]any)
	if data == nil {
		return 0, 0
	}
	list, _ := data["List"].([]any)
	if len(list) == 0 {
		return 0, 0
	}
	instance, _ := list[0].(map[string]any)
	metrics, _ := instance["Metrics"].([]any)

	for _, m := range metrics {
		metric, _ := m.(map[string]any)
		key, _ := metric["MetricKey"].(string)
		val := latestValue(metric)
		switch key {
		case "uhost_cpu_used":
			cpu = val
		case "cloudwatch_memory_usage":
			mem = val
		}
	}
	return cpu, mem
}

// latestValue extracts the most recent value from a metric's Results.
func latestValue(metric map[string]any) float64 {
	results, _ := metric["Results"].([]any)
	if len(results) == 0 {
		return 0
	}
	first, _ := results[0].(map[string]any)
	values, _ := first["Values"].([]any)
	if len(values) == 0 {
		return 0
	}
	last, _ := values[len(values)-1].(map[string]any)
	val, _ := last["Value"].(float64)
	return val
}
