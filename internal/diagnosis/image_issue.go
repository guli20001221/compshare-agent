package diagnosis

import "fmt"

// ImageIssueChain returns the diagnostic chain for image-related problems.
// Single step: checks instance state and image info, then analyzes image type
// if instance is Running.
func ImageIssueChain() *Chain {
	return &Chain{
		Name:        "DiagnoseImageIssue",
		Description: "诊断镜像问题：检查实例状态与镜像类型 → 给出建议",
		Steps: []Step{
			stepCheckImageAndState(),
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "无法确定镜像问题。",
			Suggestion: "建议尝试用官方系统镜像重新创建实例。",
		},
	}
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

func stepCheckImageAndState() Step {
	return Step{
		Name: "检查实例与镜像",
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
			imageName, _ := host["CompShareImageName"].(string)

			switch state {
			case "Install Fail":
				conclusion := "实例初始化失败，可能是镜像问题。"
				if imageName != "" {
					conclusion = fmt.Sprintf("实例初始化失败（镜像：%s），可能是镜像本身存在问题。", imageName)
				}
				return Verdict{
					Action:     Conclude,
					Conclusion: conclusion,
					Suggestion: "建议换用官方系统镜像重新创建实例。如使用的是社区镜像，可联系镜像作者反馈。",
				}
			case "Install":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在初始化中，镜像正在安装。",
					Suggestion: "请耐心等待 2-3 分钟。超过 5 分钟仍未完成请联系客服。",
				}
			case "Starting":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在启动中（Starting），此状态不产生费用。",
					Suggestion: "请等待启动完成，通常 1-2 分钟。启动完成后即可检查镜像状态。",
				}
			case "Running":
				// Analyze image type
				imageType, _ := host["CompShareImageType"].(string)
				if imageType == "Community" {
					conclusion := "当前使用的是社区镜像"
					if imageName != "" {
						conclusion = fmt.Sprintf("当前使用的是社区镜像「%s」", imageName)
					}
					conclusion += "，社区镜像由第三方用户制作和维护，可能存在兼容性或环境配置问题。"
					return Verdict{
						Action:     Conclude,
						Conclusion: conclusion,
						Suggestion: "建议联系镜像作者反馈问题，或换用官方系统镜像重新创建实例。",
					}
				}
				return Verdict{
					Action:     Conclude,
					Conclusion: "镜像加载正常，当前镜像类型为「" + imageType + "」。",
					Suggestion: "如果仍有问题，请检查应用配置或联系客服。",
				}
			default:
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例当前状态为「" + state + "」，未运行。",
					Suggestion: "需要先开机才能检查镜像状态。",
				}
			}
		},
	}
}
