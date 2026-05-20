package diagnosis

import (
	"fmt"
	"strings"
)

// PortFirewallChain returns the 2-step diagnostic chain for port/service
// reachability issues. It checks instance state (which also returns instance-
// level Softwares), then queries the platform software port catalog as a
// reference fallback.
//
// Data source priority:
//  1. DescribeCompShareInstance → Softwares (instance-level, only populated for
//     Running container instances with image-defined ports)
//  2. DescribeCompShareSoftwarePort (platform-wide catalog, reference only)
func PortFirewallChain() *Chain {
	return &Chain{
		Name:        "DiagnosePortOrFirewall",
		Description: "诊断端口/服务可达性：检查实例状态与应用列表 → 查询平台端口目录 → 给出排查线索",
		Steps: []Step{
			stepPortCheckState(),
			stepCheckServicePort(),
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "未指定具体服务，无法判断具体端口是否正常。",
			Suggestion: "请说明要检查的服务名，例如 JupyterLab、FileBrowser、SSH 或自定义服务名。云侧可核对控制台应用入口；自定义服务需结合实例内只读命令进一步判断。",
		},
	}
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

func stepPortCheckState() Step {
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
					Suggestion: "请确认实例 ID 是否正确。",
				}
			}
			host, _ := hosts[0].(map[string]any)
			state, _ := host["State"].(string)

			if state != "Running" {
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例当前状态为「" + state + "」，未运行。",
					Suggestion: "需要先在控制台开机后才能访问服务。",
				}
			}
			return Verdict{Action: Continue}
		},
	}
}

func stepCheckServicePort() Step {
	return Step{
		Name: "查询服务端口",
		Tool: "DescribeCompShareSoftwarePort",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			// If user did not specify a service, fall through to chain Fallback
			targetService, _ := dCtx.Params["Service"].(string)
			if targetService == "" {
				return Verdict{Action: Continue}
			}

			normalized := normalizeServiceName(targetService)

			instanceResult := dCtx.Result("检查实例状态")
			if normalized == "SSH" {
				return evaluateSSHServicePort(instanceResult)
			}

			// Priority 1: Check instance-level Softwares from step 1
			// (populated only for Running container instances with image-defined ports)
			if found, url := findInstanceSoftware(instanceResult, normalized); found {
				conclusion := "该实例已配置「" + normalized + "」服务"
				if url != "" {
					conclusion += "，访问地址：" + url
				}
				conclusion += "。"
				return Verdict{
					Action:     Conclude,
					Conclusion: conclusion,
					Suggestion: "如果仍无法访问，请在控制台核对服务入口、安全组和本地网络连通性。",
				}
			}

			// Priority 2: Fall back to platform-wide catalog (reference only)
			ports, _ := result["SoftwarePort"].([]any)
			if len(ports) == 0 {
				return Verdict{
					Action:     Conclude,
					Conclusion: "平台端口目录未返回数据，无法确认「" + targetService + "」的默认端口。",
					Suggestion: "请先在控制台查看实例应用入口和安全组。若是自定义服务，可在实例内用只读命令自查监听端口：`ss -lntp`。",
				}
			}
			for _, p := range ports {
				port, _ := p.(map[string]any)
				software, _ := port["Software"].(string)
				portNum, _ := port["Port"].(float64)
				if strings.EqualFold(software, normalized) {
					return Verdict{
						Action:     Conclude,
						Conclusion: "该实例的应用列表中未发现「" + normalized + "」，但平台端口目录显示此服务的默认端口为 " + formatPort(portNum) + "。",
						Suggestion: "当前实例可能未配置该服务。请在控制台核对镜像、应用配置和安全组；如需预装此服务，可选择包含该应用的社区镜像重新创建实例。",
					}
				}
			}

			return Verdict{
				Action:     Conclude,
				Conclusion: "平台服务端口列表中未找到「" + targetService + "」对应的服务。",
				Suggestion: "该服务可能是自定义部署的。云侧无法确认该自定义服务状态，请在控制台核对网络、安全组和服务配置。",
			}
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// findInstanceSoftware checks the instance-level Softwares array from
// DescribeCompShareInstance for a matching service name.
// Returns (found, url).
func findInstanceSoftware(instanceResult map[string]any, normalizedName string) (bool, string) {
	if instanceResult == nil {
		return false, ""
	}
	hosts, _ := instanceResult["UHostSet"].([]any)
	if len(hosts) == 0 {
		return false, ""
	}
	host, _ := hosts[0].(map[string]any)
	softwares, _ := host["Softwares"].([]any)
	for _, s := range softwares {
		sw, _ := s.(map[string]any)
		name, _ := sw["Name"].(string)
		url, _ := sw["URL"].(string)
		if strings.EqualFold(name, normalizedName) {
			return true, url
		}
	}
	return false, ""
}

// serviceAliases maps common user inputs to canonical platform service names.
var serviceAliases = map[string]string{
	"jupyter":      "JupyterLab",
	"jupyterlab":   "JupyterLab",
	"jupyter lab":  "JupyterLab",
	"ssh":          "SSH",
	"terminal":     "SSH",
	"filebrowser":  "FileBrowser",
	"file browser": "FileBrowser",
	"文件管理":         "FileBrowser",
}

// normalizeServiceName maps user input to the canonical platform service name.
func normalizeServiceName(input string) string {
	lower := strings.ToLower(strings.TrimSpace(input))
	if canonical, ok := serviceAliases[lower]; ok {
		return canonical
	}
	return input // return as-is if no alias match
}

func formatPort(port float64) string {
	if port == 0 {
		return "未知"
	}
	return fmt.Sprintf("%.0f", port)
}

func evaluateSSHServicePort(instanceResult map[string]any) Verdict {
	host, ok := firstHostFromResult(instanceResult)
	if !ok {
		return Verdict{
			Action:     Conclude,
			Conclusion: "未找到实例，无法确认 SSH 登录入口。",
			Suggestion: "请确认实例 ID 是否正确。",
		}
	}
	if isWindowsHost(host) {
		return Verdict{
			Action:     Conclude,
			Conclusion: "这是 Windows 实例，不适用 Linux SSH 登录方式。",
			Suggestion: "请使用控制台提供的 Windows RDP 入口，或本地打开 mstsc 进行远程桌面连接。",
		}
	}
	if cmd := sshLoginCommandFromHost(host); cmd != "" {
		return Verdict{
			Action:     Conclude,
			Conclusion: "该实例已返回 SSH 登录入口：" + cmd + "。",
			Suggestion: "如果仍连不上，请继续使用 DiagnoseSSH 做资源和登录入口诊断；若能进入 JupyterLab，可用只读命令自查：`ss -lntp | grep ':22'`。",
		}
	}
	return Verdict{
		Action:     Conclude,
		Conclusion: "云侧未返回 SSH 登录命令，无法确认该实例已有可用 SSH 登录入口。",
		Suggestion: "请先在控制台核对登录入口和公网 IP。若能进入 JupyterLab，可用只读命令自查：`systemctl status ssh --no-pager`、`ss -lntp | grep ':22'`。",
	}
}
