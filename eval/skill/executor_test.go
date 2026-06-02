package skill_eval

import "context"

// recordingExecutor implements intent.HandlerExecutor (Execute) with canned
// read-only data and records the ordered list of actions it was asked to run, so
// the offline layer can assert expected / forbidden tool calls. The data shapes
// mirror the real API responses each fast-tier handler parses (and extend the
// engine's fastTierContractExecutor with the CPU/Memory collection that pricing
// needs to reach its price-table branch rather than the clarify branch).
type recordingExecutor struct{ calls []string }

func (e *recordingExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, action)
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
