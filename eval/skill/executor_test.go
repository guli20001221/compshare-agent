package skill_eval

import "context"

// toolCall is one recorded tool invocation: the action name AND the args it was
// called with. Recording args (not just the name) lets the offline layer catch a
// handler that calls the right tool with the WRONG parameters (e.g. pricing for
// the wrong GPU / zone / charge type) — which a name-only check would score 1.00.
type toolCall struct {
	Action string
	Args   map[string]any
}

// recordingExecutor implements intent.HandlerExecutor (Execute) with canned
// read-only data and records every (action, args) it is asked to run. The data
// shapes mirror the real API responses each fast-tier handler parses (and extend
// the engine's fastTierContractExecutor with the CPU/Memory collection that
// pricing needs to reach its price-table branch rather than the clarify branch).
type recordingExecutor struct{ calls []toolCall }

// actions returns the ordered action names (for expected/forbidden/extra checks).
func (e *recordingExecutor) actions() []string {
	out := make([]string, 0, len(e.calls))
	for _, c := range e.calls {
		out = append(out, c.Action)
	}
	return out
}

// argsFor returns the args of the first recorded call to action, or nil.
func (e *recordingExecutor) argsFor(action string) (map[string]any, bool) {
	for _, c := range e.calls {
		if c.Action == action {
			return c.Args, true
		}
	}
	return nil, false
}

func (e *recordingExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, toolCall{Action: action, Args: args})
	switch action {
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{
				"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"GraphicsMemory": map[string]any{"Value": float64(24)},
				"MachineSizes": []any{map[string]any{
					"Gpu":        float64(1),
					"Collection": []any{map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}}},
				}},
			},
			map[string]any{
				"Name": "A100", "Zone": "cn-wlcb-01", "Status": "Normal",
				"GraphicsMemory": map[string]any{"Value": float64(80)},
				"MachineSizes": []any{map[string]any{
					"Gpu":        float64(1),
					"Collection": []any{map[string]any{"Cpu": float64(16), "Memory": []any{float64(128)}}},
				}},
			},
		}}, nil
	case "GetCompShareInstancePrice":
		return map[string]any{"PriceSet": []any{map[string]any{
			"Price": float64(1.5), "ChargeType": "Dynamic",
		}}}, nil
	case "CheckCompShareResourceCapacity":
		return map[string]any{"Specs": []any{map[string]any{"Gpu": float64(1), "ResourceEnough": true}}}, nil
	case "CheckCompShareNetOptimizer":
		return map[string]any{"Optimized": false, "Info": []any{
			map[string]any{"Region": "cn-wlcb-01", "Optimized": false},
		}}, nil
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch 2.9", "ImageType": "App"},
		}}, nil
	case "DescribeCompShareCustomImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-2", "Name": "my-image", "Status": "Available"},
		}}, nil
	case "DescribeCommunityImages":
		return map[string]any{"CompshareImageGroup": []any{
			map[string]any{"ImageName": "LiveTalking", "Data": []any{map[string]any{"CompShareImageId": "img-3"}}},
		}}, nil
	}
	return map[string]any{}, nil
}
