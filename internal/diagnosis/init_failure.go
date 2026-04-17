package diagnosis

import "fmt"

// InitFailureChain returns a diagnostic chain that checks instance init state.
// When UHostId is provided, diagnoses that specific instance.
// When UHostId is omitted, queries ALL instances and reports every one that
// is in "Install Fail" or "Install" state — so the user sees the full picture.
func InitFailureChain() *Chain {
	return &Chain{
		Name:        "DiagnoseInitFailure",
		Description: "诊断实例初始化失败问题",
		Steps: []Step{
			{
				Name: "check_init_state",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					if id, ok := dCtx.Params["UHostId"]; ok && id != nil && id != "" {
						s, ok := id.(string)
						if ok && s != "" {
							return map[string]any{"UHostIds": []any{s}}, nil
						}
					}
					// No UHostId — query all instances (API default Limit=20, set higher)
					return map[string]any{"Limit": 100}, nil
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

func evaluateInitState(result map[string]any, dCtx *Context) Verdict {
	hosts, _ := result["UHostSet"].([]any)
	if len(hosts) == 0 {
		return Verdict{
			Action:     Conclude,
			Conclusion: "未找到实例",
			Suggestion: "请确认实例 ID 是否正确",
		}
	}

	// Single instance mode (UHostId was specified)
	if id, ok := dCtx.Params["UHostId"]; ok && id != nil && id != "" {
		host, ok := hosts[0].(map[string]any)
		if !ok {
			return Verdict{
				Action:     Conclude,
				Conclusion: "无法确定初始化状态",
				Suggestion: "请联系客服",
			}
		}
		return evaluateSingleInstance(host)
	}

	// Multi-instance mode: scan all, report every abnormal one
	var failedLines, installingLines, startingLines []string
	var normalCount int

	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}
		id, _ := host["UHostId"].(string)
		name, _ := host["Name"].(string)
		state, _ := host["State"].(string)
		imageName, _ := host["CompShareImageName"].(string)

		label := fmt.Sprintf("%s (%s)", id, name)
		switch state {
		case "Install Fail":
			detail := label + " — 初始化失败"
			if imageName != "" {
				detail += fmt.Sprintf("（镜像：%s）", imageName)
			}
			failedLines = append(failedLines, detail)
		case "Install":
			installingLines = append(installingLines, label+" — 正在初始化中")
		case "Starting":
			startingLines = append(startingLines, label+" — 正在启动中（不收费）")
		default:
			normalCount++
		}
	}

	if len(failedLines) == 0 && len(installingLines) == 0 && len(startingLines) == 0 {
		return Verdict{
			Action:     Conclude,
			Conclusion: "所有实例状态正常，没有初始化失败的实例。",
			Suggestion: "如有疑问请联系客服。",
		}
	}

	conclusion := ""
	if len(failedLines) > 0 {
		conclusion += fmt.Sprintf("发现 %d 台实例初始化失败：\n", len(failedLines))
		for _, line := range failedLines {
			conclusion += "- " + line + "\n"
		}
	}
	if len(installingLines) > 0 {
		if conclusion != "" {
			conclusion += "\n"
		}
		conclusion += fmt.Sprintf("另有 %d 台正在初始化中：\n", len(installingLines))
		for _, line := range installingLines {
			conclusion += "- " + line + "\n"
		}
	}
	if len(startingLines) > 0 {
		if conclusion != "" {
			conclusion += "\n"
		}
		conclusion += fmt.Sprintf("另有 %d 台正在启动中：\n", len(startingLines))
		for _, line := range startingLines {
			conclusion += "- " + line + "\n"
		}
	}

	var suggestionParts []string
	if len(failedLines) > 0 {
		suggestionParts = append(suggestionParts, "初始化失败的实例建议删除重建，可换用官方系统镜像。")
	}
	if len(installingLines) > 0 {
		suggestionParts = append(suggestionParts, "正在初始化的实例请耐心等待，超过 5 分钟请联系客服。")
	}
	if len(startingLines) > 0 {
		suggestionParts = append(suggestionParts, "启动中的实例请等待 1-2 分钟，此状态不收费。")
	}
	suggestion := ""
	for i, part := range suggestionParts {
		if i > 0 {
			suggestion += " "
		}
		suggestion += part
	}

	return Verdict{
		Action:     Conclude,
		Conclusion: conclusion,
		Suggestion: suggestion,
	}
}

func evaluateSingleInstance(host map[string]any) Verdict {
	state, _ := host["State"].(string)

	switch state {
	case "Install Fail":
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
			Conclusion: "实例仍在初始化中，通常需要 2-3 分钟。",
			Suggestion: "请耐心等待。超过 5 分钟仍未完成请联系客服处理。",
		}
	case "Starting":
		return Verdict{
			Action:     Conclude,
			Conclusion: "实例正在启动中（Starting），此状态不产生费用。",
			Suggestion: "请等待启动完成，通常 1-2 分钟。如长时间卡在启动中，请联系客服。",
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
