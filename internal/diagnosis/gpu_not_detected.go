package diagnosis

// GPUNotDetectedChain returns a 2-step diagnostic chain for "nvidia-smi 报错" issues.
// Flow: check instance state & GPU config -> check GPU monitor metrics -> fallback.
func GPUNotDetectedChain() *Chain {
	return &Chain{
		Name:        "DiagnoseGPUNotDetected",
		Description: "诊断 GPU 检测不到：检查实例状态与 GPU 配置 → 检查 GPU 监控数据 → 兜底建议",
		Steps: []Step{
			stepCheckInstanceGPU(),
			stepCheckGPUMonitor(),
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "实例已分配 GPU 但无法确认 GPU 工作状态。可能是驱动未正确加载。",
			Suggestion: "建议通过控制台重启实例。如问题持续，请尝试换用官方系统镜像重新创建实例。",
		},
	}
}

func stepCheckInstanceGPU() Step {
	return Step{
		Name: "检查实例状态与 GPU 配置",
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
					Conclusion: "实例当前处于关机状态，无法检测 GPU。",
					Suggestion: "需要先开机才能使用 GPU。可以使用 StartInstanceWorkflow 开机。",
				}
			case "Install":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在初始化中，尚未就绪。初始化通常需要 2-3 分钟。",
					Suggestion: "请耐心等待初始化完成后再尝试使用 GPU。",
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
					Suggestion: "启动通常需要 1-2 分钟，完成后即可使用 GPU。",
				}
			case "Stopping":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在关机中，无法检测 GPU。",
					Suggestion: "请等待关机完成后，再使用 StartInstanceWorkflow 重新开机。",
				}
			case "Rebooting":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在重启中，请稍等片刻。",
					Suggestion: "重启通常需要 1-2 分钟，完成后即可使用 GPU。",
				}
			case "Running":
				gpu, _ := host["GPU"].(float64)
				if gpu == 0 {
					return Verdict{
						Action:     Conclude,
						Conclusion: "实例当前未分配 GPU（无卡模式启动）。无卡模式下不加载 GPU 驱动，nvidia-smi 无法识别设备。",
						Suggestion: "请关机后以正常模式（带 GPU）重新开机。",
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

func stepCheckGPUMonitor() Step {
	return Step{
		Name: "检查 GPU 监控数据",
		Tool: "GetCompShareInstanceMonitor",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{dCtx.Params["UHostId"]},
			}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			gpuUtil, gpuMem := extractGPUMetrics(result)
			if gpuUtil > 0 || gpuMem > 0 {
				return Verdict{
					Action:     Conclude,
					Conclusion: "GPU 硬件工作正常（监控显示有 GPU 活动）。nvidia-smi 报错可能是容器内驱动版本不匹配。",
					Suggestion: "尝试在终端执行 `ldconfig` 或重启实例。如使用自定义镜像，请确认镜像内 CUDA 驱动与宿主机兼容。",
				}
			}
			return Verdict{Action: Continue}
		},
	}
}

// extractGPUMetrics gets the latest GPU utilization and GPU memory usage from monitor data.
func extractGPUMetrics(result map[string]any) (gpuUtil, gpuMem float64) {
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
		case "cloudwatch_gpu_util":
			gpuUtil = val
		case "cloudwatch_gpu_memory_usage":
			gpuMem = val
		}
	}
	return gpuUtil, gpuMem
}
