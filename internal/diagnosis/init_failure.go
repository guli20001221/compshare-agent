package diagnosis

import "fmt"

// InitFailureChain returns a single-step diagnostic chain that checks instance
// state via DescribeCompShareInstance and provides init-failure guidance.
func InitFailureChain() *Chain {
	return &Chain{
		Name:        "DiagnoseInitFailure",
		Description: "诊断实例初始化失败问题",
		Steps: []Step{
			{
				Name: "check_init_state",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					uhostID, ok := dCtx.Params["UHostId"]
					if !ok || uhostID == "" {
						return nil, fmt.Errorf("missing required param: UHostId")
					}
					args := map[string]any{"UHostId": uhostID}
					if zone, ok := dCtx.Params["Zone"]; ok {
						args["Zone"] = zone
					}
					return args, nil
				},
				Evaluate: evaluateInitState,
			},
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "无法确定初始化状态",
			Suggestion: "请联系客服",
		},
	}
}

func evaluateInitState(result map[string]any, _ *Context) Verdict {
	uhostSet, ok := result["UHostSet"]
	if !ok {
		return Verdict{
			Action:     Conclude,
			Conclusion: "未找到实例",
			Suggestion: "请确认实例 ID 是否正确",
		}
	}

	hosts, ok := uhostSet.([]any)
	if !ok || len(hosts) == 0 {
		return Verdict{
			Action:     Conclude,
			Conclusion: "未找到实例",
			Suggestion: "请确认实例 ID 是否正确",
		}
	}

	host, ok := hosts[0].(map[string]any)
	if !ok {
		return Verdict{
			Action:     Conclude,
			Conclusion: "无法确定初始化状态",
			Suggestion: "请联系客服",
		}
	}

	state, _ := host["State"].(string)

	switch state {
	case "InstallFail":
		conclusion := "实例初始化失败"
		if imageName, ok := host["CompShareImageName"].(string); ok && imageName != "" {
			conclusion = fmt.Sprintf("实例初始化失败（镜像：%s）。可能原因包括：镜像异常、资源分配冲突、或平台临时问题。", imageName)
		}
		return Verdict{
			Action:     Conclude,
			Conclusion: conclusion,
			Suggestion: "建议删除重建，换用官方镜像",
		}
	case "Install":
		return Verdict{
			Action:     Conclude,
			Conclusion: "仍在初始化中，通常需要 2-3 分钟",
			Suggestion: "超过 10 分钟请联系客服",
		}
	case "Running":
		return Verdict{
			Action:     Conclude,
			Conclusion: "已正常运行",
			Suggestion: "初始化已成功完成",
		}
	default:
		return Verdict{
			Action:     Conclude,
			Conclusion: fmt.Sprintf("当前状态为 %s，并非初始化失败", state),
			Suggestion: "如有疑问请联系客服",
		}
	}
}
