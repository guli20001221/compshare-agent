package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/prompt"
)

// These gates run the REAL pipeline order from executeTool (engine.go ~4266-4287):
//
//	truncateDescribeResultForReAct   -> caps UHostSet at 10, adds Shown/Truncated
//	attachDeterministicInstanceTable -> ADDS RenderedInstanceTable + DisplayInstruction
//	projectToolResultForReAct        -> no-op for this action (not in reactProjectionActions)
//	prompt.FormatToolResult          -> the cap
//
// Testing prompt.FormatToolResult alone would miss the interaction that matters:
// the deterministic table is a large top-level STRING added immediately before the
// cap is applied. Under the old byte-cut it survived by accident — Go marshals map
// keys in sorted order and "RenderedInstanceTable" sorts before "UHostSet", so the
// cut ate the instance list and left the table whole. That accident is the only
// reason the user-visible list stayed correct while the model's view was garbage.
//
// The new shrink ladder must preserve that protection ON PURPOSE, and its harshest
// rungs clip long strings — which is exactly what the table is. So: prove the table
// still survives intact.

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

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	truncateDescribeResultForReAct(args, result)
	eng.attachDeterministicInstanceTable("DescribeCompShareInstance", result, result)
	if projectToolResultForReAct("DescribeCompShareInstance", result) {
		t.Fatal("precondition: DescribeCompShareInstance is not in reactProjectionActions, " +
			"so projection must be a no-op — if this fires, the projection whitelist changed " +
			"and this gate's premise needs rechecking")
	}
	return prompt.FormatToolResult(result)
}

// TestRealPipeline_InstanceListStaysParseableAndHonest is the end-to-end form of the
// bug: on prod defaults, an unpinned DescribeCompShareInstance inside ReAct.
func TestRealPipeline_InstanceListStaysParseableAndHonest(t *testing.T) {
	SetAgentDeterministicRenderEnabled(true) // prod default
	t.Cleanup(func() { SetAgentDeterministicRenderEnabled(false) })

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

			// The deterministic table is what keeps the USER-visible list correct even
			// when the model's JSON view is degraded. The shrink ladder must not clip it.
			table, ok := parsed[renderedInstanceTableKey].(string)
			if !ok || table == "" {
				t.Fatalf("n=%d: the rendered instance table must survive the cap — it is the "+
					"only thing standing between a truncated payload and a fabricated "+
					"instance list", n)
			}
			// HONEST SCOPE NOTE. The presence check above IS a gate: emptying the
			// shrink ladder sends the payload to oversizeNotice, the table disappears,
			// and it goes red (verified).
			//
			// The clip check below is NOT independently verified — it is a tripwire.
			// I could not construct a mutation of the ladder that trips it, because on
			// this action the array shrink always rescues the payload before a scalar
			// rung is ever reached: truncateDescribeResultForReAct caps UHostSet at 10,
			// and dropping that list to 2 rows fits under the cap with the table whole.
			// So on today's shape the assertion is vacuously true. It is kept because a
			// future ladder edit (or a bulkier table) could make scalar clipping
			// reachable, and clipping the table is the one change here that would be a
			// REGRESSION against the old byte-cut — under which the table survived by
			// the accident of Go's sorted key order putting it ahead of UHostSet.
			if strings.Contains(table, "此处已截断") {
				t.Errorf("n=%d: the rendered table was clipped by a scalar rung; the user-visible "+
					"list is now incomplete, which is a REGRESSION against the old byte-cut", n)
			}
			shown := 10
			if n < shown {
				shown = n
			}
			for i := 1; i <= shown; i++ {
				id := fmt.Sprintf("uhost-1exampleaa%02d", i)
				if !strings.Contains(table, id) {
					t.Errorf("n=%d: instance %s is missing from the rendered table", n, id)
				}
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
	SetAgentDeterministicRenderEnabled(true)
	t.Cleanup(func() { SetAgentDeterministicRenderEnabled(false) })

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
