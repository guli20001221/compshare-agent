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
			Conclusion: "平台端口映射正常。服务可能未就绪。",
			Suggestion: "云侧只能确认平台端口映射。请在控制台核对服务配置、镜像和安全组；具体服务状态需通过控制台指引或技术支持进一步确认。",
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

			// Priority 1: Check instance-level Softwares from step 1
			// (populated only for Running container instances with image-defined ports)
			instanceResult := dCtx.Result("检查实例状态")
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
