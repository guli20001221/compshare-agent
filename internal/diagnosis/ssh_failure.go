package diagnosis

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/tools"
)

// SSHFailureChain performs a cloud-side SSH precheck. It verifies the exact
// instance, its lifecycle state, the structured login endpoint and monitor
// signals. It does not probe the public TCP path or enter the guest OS.
func SSHFailureChain() *Chain {
	return &Chain{
		Name: "InstanceAccessSSHPrecheck",
		Steps: []Step{
			stepCheckInstanceState(),
			stepCheckResourceUsage(),
		},
		Fallback: Verdict{
			Action:         Conclude,
			Conclusion:     "云侧预检未发现明确阻断：实例运行中，SSH 登录入口完整，CPU/内存监控未见高压。该预检未实际探测公网端口，也未进入实例检查 SSH 服务或认证日志。",
			Suggestion:     sshFailureSuggestion(sshFailureUnknown),
			PrecheckStatus: PrecheckConfigured,
		},
	}
}

// SSHFailureChainWithDescribeResult reuses an exact-id instance response that
// a typed capability already fetched. This keeps the original SSH checks and
// monitor step without querying DescribeCompShareInstance twice.
func SSHFailureChainWithDescribeResult(raw map[string]any) *Chain {
	chain := SSHFailureChain()
	chain.Steps[0].Execute = func(context.Context, tools.ToolExecutor, map[string]any) (map[string]any, error) {
		return raw, nil
	}
	return chain
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
			return map[string]any{"UHostIds": []any{id}}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			id, err := dCtx.RequireUHostId()
			if err != nil {
				return Verdict{Action: Conclude, Conclusion: "缺少要诊断的实例 ID。", Suggestion: "请提供要检查的实例 ID。", PrecheckStatus: PrecheckUnknown}
			}
			host, ok := hostForRequestedID(result, id)
			if !ok {
				return Verdict{
					Action:         Conclude,
					Conclusion:     "查询结果中未找到该实例，可能已被释放、ID 输入有误，或当前账号无权访问。",
					Suggestion:     "请确认实例 ID 和当前账号；也可以先查看实例列表。",
					PrecheckStatus: PrecheckBlocked,
				}
			}
			state := strings.TrimSpace(stringValue(host["State"]))

			switch state {
			case "Stopped":
				return Verdict{Action: Conclude, Conclusion: "实例当前处于关机状态，无法进行 SSH 连接。", Suggestion: "需要先在控制台开机后才能 SSH 连接。", PrecheckStatus: PrecheckBlocked}
			case "Initializing", "Install":
				return Verdict{Action: Conclude, Conclusion: "实例正在初始化中，尚未就绪。", Suggestion: "请等待控制台显示实例进入运行状态后再尝试 SSH 连接。", PrecheckStatus: PrecheckBlocked}
			case "Install Fail":
				return Verdict{
					Action:         Conclude,
					Conclusion:     "实例初始化失败，当前无法正常使用。",
					Suggestion:     "请先查看控制台中的失败详情并联系技术支持。若之后选择重建，请先确认实例中没有需要保留的数据。",
					PrecheckStatus: PrecheckBlocked,
				}
			case "Starting":
				return Verdict{Action: Conclude, Conclusion: "实例正在启动中，尚未就绪。", Suggestion: "请等待控制台显示实例进入运行状态后再尝试 SSH 连接。", PrecheckStatus: PrecheckBlocked}
			case "Stopping":
				return Verdict{Action: Conclude, Conclusion: "实例正在关机中，无法 SSH 连接。", Suggestion: "请等待状态稳定后再决定是否重新开机。", PrecheckStatus: PrecheckBlocked}
			case "Rebooting":
				return Verdict{Action: Conclude, Conclusion: "实例正在重启中，尚未就绪。", Suggestion: "请等待控制台显示实例进入运行状态后再尝试 SSH 连接。", PrecheckStatus: PrecheckBlocked}
			case "Running":
				if isWindowsHost(host) {
					return Verdict{
						Action:         Conclude,
						Conclusion:     "这是 Windows 实例；平台默认远程入口是 RDP，云侧预检不能确认实例内是否另行安装并启用了 OpenSSH。",
						Suggestion:     "请优先使用控制台提供的 Windows RDP 入口；若你自行配置过 OpenSSH，请在系统内核对其服务和端口。",
						PrecheckStatus: PrecheckUnknown,
					}
				}
				if _, command, valid := validatedSSHEndpoint(host); !valid {
					if command == "" {
						return Verdict{
							Action:         Conclude,
							Conclusion:     "云侧未返回 SSH 登录命令，无法确认该实例已有可用 SSH 登录入口。",
							Suggestion:     "请先在控制台核对实例登录入口和公网 IP。若能通过 JupyterLab 进入终端，可用只读命令自查：`systemctl status ssh --no-pager`、`ss -lntp`。安装或启动 SSH 服务属于会修改实例环境的可选修复，请确认后再执行。",
							PrecheckStatus: PrecheckUnknown,
						}
					}
					return Verdict{
						Action:         Conclude,
						Conclusion:     "云侧返回的 SSH 登录入口不完整，无法确认主机和端口可用。",
						Suggestion:     "请在控制台核对公网 IP 和 SSH 端口；Pod 实例还需确认 SSH 的 TCP 转发存在且映射到内部 23 端口。",
						PrecheckStatus: PrecheckBlocked,
					}
				}
				return Verdict{Action: Continue}
			default:
				shown := state
				if shown == "" {
					shown = "未知"
				}
				return Verdict{
					Action:         Conclude,
					Conclusion:     "实例当前状态为「" + shown + "」，云侧预检不能确认它已具备 SSH 条件。",
					Suggestion:     "请到控制台查看实例详情；状态长时间不变化时联系技术支持。",
					PrecheckStatus: PrecheckUnknown,
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
			return map[string]any{"UHostIds": []any{id}}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			id, err := dCtx.RequireUHostId()
			if err != nil {
				return Verdict{Action: Conclude, Conclusion: "缺少要诊断的实例 ID。", Suggestion: "请提供要检查的实例 ID。", PrecheckStatus: PrecheckUnknown}
			}
			kind := failureKindFromContext(dCtx)
			cpuUsage, memUsage, diskUsage, cpuOK, memOK, diskOK := extractLatestMetrics(result, id)
			const threshold = 90.0
			const diskThreshold = 95.0

			if diskOK && diskUsage >= diskThreshold {
				return Verdict{
					Action:         Conclude,
					Conclusion:     fmt.Sprintf("监控显示系统盘使用率 %.1f%%，这是可能影响 SSH 登录的风险信号；单凭这一条监控不能确认它就是本次失败根因。", diskUsage),
					Suggestion:     appendSuggestion("若能通过 JupyterLab 进入终端，可用只读命令检查：`df -h`、`du -sh /* 2>/dev/null | sort -h`。清理或扩容会修改实例，请确认后再执行。", sshFailureSuggestion(kind)),
					PrecheckStatus: PrecheckUnknown,
				}
			}

			if (cpuOK && cpuUsage >= threshold) || (memOK && memUsage >= threshold) {
				details := make([]string, 0, 2)
				if cpuOK && cpuUsage >= threshold {
					details = append(details, fmt.Sprintf("CPU 使用率 %.1f%%", cpuUsage))
				}
				if memOK && memUsage >= threshold {
					details = append(details, fmt.Sprintf("内存使用率 %.1f%%", memUsage))
				}
				return Verdict{
					Action:         Conclude,
					Conclusion:     "监控显示高负载风险信号：" + strings.Join(details, "，") + "；高负载可能影响 SSH 响应，但不能确认它就是本次失败根因。",
					Suggestion:     appendSuggestion("若能通过 JupyterLab 进入终端，可先用只读命令检查：`uptime`、`free -h`、`top -b -n 1 | head`。", sshFailureSuggestion(kind)),
					PrecheckStatus: PrecheckUnknown,
				}
			}
			if !cpuOK || !memOK {
				return Verdict{
					Action:         Conclude,
					Conclusion:     "监控未返回 CPU/内存数据，无法确认资源状态。云侧仅确认实例运行并返回了完整的 SSH 登录入口。",
					Suggestion:     appendSuggestion("若能通过 JupyterLab 进入终端，可用只读命令检查：`free -h`、`uptime`、`top -b -n 1 | head`。", sshFailureSuggestion(kind)),
					PrecheckStatus: PrecheckUnknown,
				}
			}
			return Verdict{
				Action:         Conclude,
				Conclusion:     "云侧预检未发现明确阻断：实例运行中，SSH 登录入口完整，CPU/内存监控未见高压。该预检未实际探测公网端口，也未进入实例检查 SSH 服务或认证日志。",
				Suggestion:     sshFailureSuggestion(kind),
				PrecheckStatus: PrecheckConfigured,
			}
		},
	}
}

// extractLatestMetrics reads only the monitor record whose UHostId equals the
// requested instance. UHost and pod responses use different metric shapes.
func extractLatestMetrics(result map[string]any, expectedID string) (cpu, mem, disk float64, cpuOK, memOK, diskOK bool) {
	data, _ := result["Data"].(map[string]any)
	if data == nil {
		return 0, 0, 0, false, false, false
	}

	if instance, ok := recordForID(data["List"], expectedID); ok {
		metrics, _ := instance["Metrics"].([]any)
		for _, rawMetric := range metrics {
			metric, _ := rawMetric.(map[string]any)
			key, _ := metric["MetricKey"].(string)
			val, ok := latestValue(metric)
			if !ok {
				continue
			}
			switch key {
			case "uhost_cpu_used":
				cpu, cpuOK = val, true
			case "cloudwatch_memory_usage":
				mem, memOK = val, true
			case "cloudwatch_sys_disk_used_per":
				disk, diskOK = val, true
			}
		}
		return cpu, mem, disk, cpuOK, memOK, diskOK
	}

	if pod, ok := recordForID(data["PodList"], expectedID); ok {
		metrics, _ := pod["Metrics"].(map[string]any)
		if v, ok := latestPointByTimestamp(metrics["Cpu"]); ok {
			cpu, cpuOK = v, true
		}
		if v, ok := latestPointByTimestamp(metrics["Memory"]); ok {
			mem, memOK = v, true
		}
		if v, ok := latestPointByTimestamp(metrics["SysDiskUsed"]); ok {
			disk, diskOK = v, true
		}
	}
	return cpu, mem, disk, cpuOK, memOK, diskOK
}

func latestValue(metric map[string]any) (float64, bool) {
	results, _ := metric["Results"].([]any)
	if len(results) == 0 {
		return 0, false
	}
	first, _ := results[0].(map[string]any)
	return latestPointByTimestamp(first["Values"])
}

func latestPointByTimestamp(raw any) (float64, bool) {
	points, _ := raw.([]any)
	if len(points) == 0 {
		return 0, false
	}
	var bestVal, bestTS float64
	found := false
	for _, rawPoint := range points {
		point, _ := rawPoint.(map[string]any)
		val, ok := point["Value"].(float64)
		if !ok {
			continue
		}
		ts, _ := point["Timestamp"].(float64)
		if !found || ts >= bestTS {
			bestVal, bestTS, found = val, ts, true
		}
	}
	return bestVal, found
}

type sshEndpoint struct {
	host string
	port int
}

func validatedSSHEndpoint(host map[string]any) (sshEndpoint, string, bool) {
	command := strings.TrimSpace(stringValue(host["SshLoginCommand"]))
	endpoint, ok := parseSSHLoginCommand(command)
	if !ok {
		return sshEndpoint{}, command, false
	}

	id := strings.TrimSpace(stringValue(host["UHostId"]))
	if platform.IsPodInstanceID(id) {
		rawForwards, present := host["TcpForwards"]
		if !present {
			return endpoint, command, true // backward-compatible deployed response
		}
		forwards, _ := rawForwards.([]any)
		for _, rawForward := range forwards {
			forward, _ := rawForward.(map[string]any)
			internalPort, internalOK := integerValue(forward["InternalPort"])
			externalPort, externalOK := integerValue(forward["ExternalPort"])
			externalHost := strings.TrimSpace(stringValue(forward["ExternalHost"]))
			if internalOK && externalOK && internalPort == 23 && externalPort == endpoint.port &&
				(externalHost == "" || strings.EqualFold(externalHost, endpoint.host)) {
				return endpoint, command, true
			}
		}
		return sshEndpoint{}, command, false
	}

	rawIPs, present := host["IPSet"]
	if !present {
		return endpoint, command, true // backward-compatible deployed response
	}
	ips, _ := rawIPs.([]any)
	for _, rawIP := range ips {
		ip, _ := rawIP.(map[string]any)
		address := strings.TrimSpace(stringValue(ip["IP"]))
		if address == "" {
			address = strings.TrimSpace(stringValue(ip["Ip"]))
		}
		ipType := strings.TrimSpace(stringValue(ip["Type"]))
		if address == "" || strings.EqualFold(ipType, "Private") {
			continue
		}
		if strings.EqualFold(address, endpoint.host) {
			return endpoint, command, true
		}
	}
	return sshEndpoint{}, command, false
}

func parseSSHLoginCommand(command string) (sshEndpoint, bool) {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "ssh" {
		return sshEndpoint{}, false
	}
	port := 22
	target := ""
	for i := 1; i < len(fields); i++ {
		switch fields[i] {
		case "-p":
			if i+1 >= len(fields) {
				return sshEndpoint{}, false
			}
			parsed, err := strconv.Atoi(fields[i+1])
			if err != nil || parsed < 1 || parsed > 65535 {
				return sshEndpoint{}, false
			}
			port = parsed
			i++
		default:
			if (len(fields[i]) > 0 && fields[i][0] == '-') || target != "" {
				return sshEndpoint{}, false
			}
			target = fields[i]
		}
	}
	userHost := strings.Split(target, "@")
	if len(userHost) != 2 || strings.TrimSpace(userHost[0]) == "" || strings.TrimSpace(userHost[1]) == "" {
		return sshEndpoint{}, false
	}
	return sshEndpoint{host: strings.TrimSpace(userHost[1]), port: port}, true
}

func hostForRequestedID(result map[string]any, expectedID string) (map[string]any, bool) {
	return recordForID(result["UHostSet"], expectedID)
}

func recordForID(raw any, expectedID string) (map[string]any, bool) {
	records, _ := raw.([]any)
	for _, rawRecord := range records {
		record, _ := rawRecord.(map[string]any)
		if strings.TrimSpace(stringValue(record["UHostId"])) == expectedID {
			return record, true
		}
	}
	return nil, false
}

func integerValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int(value), int64(int(value)) == value
	case float64:
		parsed := int(value)
		return parsed, float64(parsed) == value
	case float32:
		parsed := int(value)
		return parsed, float32(parsed) == value
	default:
		return 0, false
	}
}

func stringValue(raw any) string {
	value, _ := raw.(string)
	return value
}

type sshFailureKind string

const (
	sshFailureTimeout              sshFailureKind = "timeout"
	sshFailureConnectionRefused    sshFailureKind = "connection_refused"
	sshFailureAuthenticationFailed sshFailureKind = "authentication_failed"
	sshFailureConnectionDropped    sshFailureKind = "connection_dropped"
	sshFailureUnknown              sshFailureKind = "unknown"
)

func failureKindFromContext(dCtx *Context) sshFailureKind {
	kind := sshFailureKind(strings.TrimSpace(stringValue(dCtx.Params["FailureKind"])))
	switch kind {
	case sshFailureTimeout, sshFailureConnectionRefused, sshFailureAuthenticationFailed, sshFailureConnectionDropped:
		return kind
	default:
		return sshFailureUnknown
	}
}

func sshFailureSuggestion(kind sshFailureKind) string {
	switch kind {
	case sshFailureTimeout:
		return "云侧预检没有实际探测公网端口，连接超时仍需检查网络或端口路径。可在本地运行 `ssh -vvv -o ConnectTimeout=10 <控制台命令中的目标>`，隐去 IP、用户名和密钥路径后提供停住的位置。"
	case sshFailureConnectionRefused:
		return "连接被拒通常需要检查实例内 SSH 服务是否监听。若能通过 JupyterLab 进入终端，可用只读命令检查：`systemctl status ssh --no-pager`、`ss -lntp`；启动或修改服务前请先确认。"
	case sshFailureAuthenticationFailed:
		return "认证失败时请核对控制台命令中的用户名、密码或密钥是否匹配；使用私钥时还要检查本地私钥文件权限。请勿在对话中发送密码或私钥内容。"
	case sshFailureConnectionDropped:
		return "若连接建立后经常中断，可先用 `ssh -o ServerAliveInterval=30 -o ServerAliveCountMax=6 <控制台命令中的目标>` 验证是否改善；这只能缓解空闲链路断开，不能代替网络排查。"
	default:
		return "请提供客户端显示的准确错误。也可以运行 `ssh -vvv <控制台命令中的目标>`，隐去 IP、用户名和密钥路径后提供末尾日志，以区分网络、服务和认证问题。若能通过 JupyterLab 进入终端，可用只读命令检查：`systemctl status ssh --no-pager`、`ss -lntp`。"
	}
}

func appendSuggestion(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, " ")
}

func isWindowsHost(host map[string]any) bool {
	return strings.EqualFold(strings.TrimSpace(stringValue(host["OsType"])), "Windows")
}
