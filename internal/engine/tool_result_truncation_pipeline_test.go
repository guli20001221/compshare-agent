package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"unicode/utf8"

	"github.com/compshare-agent/internal/prompt"
)

// These gates run the REAL pipeline order from executeTool (engine.go ~2542):
//
//	truncateDescribeResultForReAct -> caps UHostSet at 10, adds Shown/Truncated
//	projectToolResultForReAct      -> no-op for this action (not in reactProjectionActions)
//	prompt.FormatToolResult        -> the cap
//
// Testing prompt.FormatToolResult alone would miss the interaction that matters: the
// cap is applied to a payload that a previous stage has already rewritten, and the
// counts that stage wrote (Shown / Truncated / TotalCount) must still be reconcilable
// against the rows that survive the cap. Go marshals map keys in sorted order, so
// those scalars sort AHEAD of "UHostSet" and survive a cut that eats the rows they
// describe — the model is then told "10 shown, 30 total" while holding one readable
// row. Accounting for rows it cannot see is precisely how this repo has produced
// phantom instances before.

func pipelineInstanceRow(i int) map[string]any {
	return map[string]any{
		"UHostId":         fmt.Sprintf("uhost-1exampleaa%02d", i),
		"Name":            fmt.Sprintf("训练节点-%02d（PyTorch 多卡）", i),
		"State":           "Running",
		"Zone":            "cn-wlcb-01",
		"CPU":             32,
		"Memory":          131072,
		"GPU":             4,
		"GpuType":         "NVIDIA-GeForce-RTX-4090",
		"ChargeType":      "Dynamic",
		"CreateTime":      1752460800 + i,
		"ExpireTime":      1755052800 + i,
		"BasicImageId":    "uimage-1exampleimg01",
		"BasicImageName":  "PyTorch 2.3.0 / CUDA 12.1 / Ubuntu 22.04 (预装 JupyterLab、TensorBoard)",
		"OsName":          "Ubuntu 22.04 64-bit",
		"SshLoginCommand": fmt.Sprintf("ssh -p 2%03d root@ssh.compshare.example.cn", 200+i),
		"TotalDiskSpace":  500,
		"StorageType":     "CLOUD_SSD",
		"MachineType":     "G-GPU-4090",
		"CpuPlatform":     "Intel/Xeon-Platinum-8352V",
		"NetCapability":   "Super",
		"NetworkState":    "Connected",
		"LifeCycle":       "Normal",
		"BootDiskState":   "Normal",
		"HotPlugMaxCpu":   64,
		"IPSet": []any{map[string]any{
			"IP": "10.9.0." + fmt.Sprint(10+i), "IPId": "eip-1exampleip001", "Type": "Private",
			"VPCId": "uvpc-1examplevpc0", "SubnetId": "subnet-1examplesb", "Mac": "52:54:00:00:00:01",
			"NetworkInterfaceId": "eni-1exampleeni01",
		}},
		"DiskSet": []any{map[string]any{
			"DiskId": "udisk-1exampledsk", "DiskType": "CLOUD_SSD", "Size": 500,
			"Drive": "/dev/vda", "IsBoot": "True", "Type": "Boot",
		}},
	}
}

func TestRealPipelineReportsTheGenericFormatterMeasurements(t *testing.T) {
	rows := make([]any, 0, 30)
	for i := 1; i <= 30; i++ {
		rows = append(rows, pipelineInstanceRow(i))
	}
	result := map[string]any{
		"Action": "DescribeCompShareInstanceResponse", "RetCode": 0,
		"TotalCount": 30, "UHostSet": rows,
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": result,
	}}, nil)
	var steps []StepEvent
	out := eng.executeTool(context.Background(), toolCall("format-trace", "DescribeCompShareInstance", `{}`), func(ev StepEvent) {
		steps = append(steps, ev)
	})

	var completion *StepEvent
	for i := range steps {
		if steps[i].Type == StepToolResult && steps[i].Action == "DescribeCompShareInstance" {
			completion = &steps[i]
		}
	}
	if completion == nil || completion.ToolResultRawRunes == nil ||
		completion.ToolResultVisibleRunes == nil || completion.ToolResultTruncated == nil {
		t.Fatalf("generic formatter observation missing from tool completion: %#v", completion)
	}
	if *completion.ToolResultVisibleRunes != utf8.RuneCountInString(out) {
		t.Fatalf("visible runes = %d, actual model-visible output = %d", *completion.ToolResultVisibleRunes, utf8.RuneCountInString(out))
	}
	if !*completion.ToolResultTruncated {
		t.Fatal("precondition: the post-projection fixture must exercise generic truncation")
	}
	if *completion.ToolResultRawRunes <= *completion.ToolResultVisibleRunes {
		t.Fatalf("raw runes = %d, visible = %d; truncation must reduce this fixture",
			*completion.ToolResultRawRunes, *completion.ToolResultVisibleRunes)
	}
}

// runRealToolResultPipeline reproduces executeTool's DescribeCompShareInstance path.
func runRealToolResultPipeline(t *testing.T, n int) string {
	t.Helper()

	rows := make([]any, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, pipelineInstanceRow(i))
	}
	result := map[string]any{
		"Action":     "DescribeCompShareInstanceResponse",
		"RetCode":    0,
		"TotalCount": n,
		"UHostSet":   rows,
	}

	args := map[string]any{} // unpinned: a full-account list, the shape that bites

	truncateDescribeResultForReAct(args, result)
	if projectToolResultForReAct("DescribeCompShareInstance", result) {
		t.Fatal("precondition: DescribeCompShareInstance is not in reactProjectionActions, " +
			"so projection must be a no-op — if this fires, the projection whitelist changed " +
			"and this gate's premise needs rechecking")
	}
	return prompt.FormatToolResult(result)
}

// TestRealPipeline_InstanceListStaysParseable is the end-to-end form of the bug: an
// unpinned DescribeCompShareInstance payload, run through the real pipeline order.
// A payload the model cannot parse at all is the worst outcome — it degrades to
// guessing about an account it was just shown.
func TestRealPipeline_InstanceListStaysParseable(t *testing.T) {
	for _, n := range []int{3, 4, 10, 30} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			out := runRealToolResultPipeline(t, n)

			var parsed map[string]any
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				tail := out
				if len(tail) > 100 {
					tail = tail[len(tail)-100:]
				}
				t.Fatalf("the model's view of a %d-instance account must be parseable JSON; "+
					"got %v\nit ends: ...%s", n, err, tail)
			}
			if _, ok := parsed["UHostSet"]; !ok {
				t.Fatalf("n=%d: UHostSet vanished entirely from the model's view: %s", n, out)
			}
		})
	}
}

// TestRealPipeline_TheModelIsNeverToldACountItCannotReconcile is the anti-fabrication
// gate. truncateDescribeResultForReAct writes Shown/Truncated/TotalCount, and Go's
// sorted key order put all three AHEAD of UHostSet — so under the byte-cut they
// survived while the rows they described did not. The model was told "10 shown, 30
// total" and handed one readable row, with the "已截取前 5 条" notice cut off the end.
// Accounting for rows it cannot see is precisely how this repo has produced phantom
// instances before.
func TestRealPipeline_TheModelIsNeverToldACountItCannotReconcile(t *testing.T) {
	for _, n := range []int{3, 10, 30} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			out := runRealToolResultPipeline(t, n)

			var parsed map[string]any
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("precondition: must parse: %v", err)
			}
			rows, ok := parsed["UHostSet"].([]any)
			if !ok {
				t.Fatalf("n=%d: UHostSet vanished entirely from the model's view: %s", n, out)
			}

			readable, notice := 0, ""
			for _, row := range rows {
				switch typed := row.(type) {
				case map[string]any:
					readable++
				case string:
					notice = typed
				}
			}

			// Whatever count the payload asserts, the payload must also explain it.
			claimed := 0
			if v, ok := parsed["Shown"].(float64); ok {
				claimed = int(v)
			}
			if claimed > readable && notice == "" {
				t.Fatalf("n=%d: the payload claims Shown=%d but only %d rows are readable and "+
					"nothing says why — the model is being invited to account for %d rows it "+
					"cannot see\ngot: %s", n, claimed, readable, claimed-readable, out)
			}
			if readable < n && notice == "" {
				t.Fatalf("n=%d: only %d of %d rows are readable with no marker at all", n, readable, n)
			}
		})
	}
}
