package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// describeInstanceRow approximates the documented upstream row size with
// synthetic identifiers so truncation tests exercise realistic payloads.
func describeInstanceRow(i int) map[string]any {
	return map[string]any{
		"UHostId":          fmt.Sprintf("uhost-1exampleaa%02d", i),
		"Name":             fmt.Sprintf("训练节点-%02d（PyTorch 多卡）", i),
		"State":            "Running",
		"Zone":             "cn-wlcb-01",
		"CPU":              32,
		"Memory":           131072,
		"GPU":              4,
		"GpuType":          "NVIDIA-GeForce-RTX-4090",
		"ChargeType":       "Dynamic",
		"CreateTime":       1752460800 + i,
		"ExpireTime":       1755052800 + i,
		"BasicImageId":     "uimage-1exampleimg01",
		"BasicImageName":   "PyTorch 2.3.0 / CUDA 12.1 / Ubuntu 22.04 (预装 JupyterLab、TensorBoard)",
		"ImageId":          "uimage-1exampleimg01",
		"OsName":           "Ubuntu 22.04 64-bit",
		"OsType":           "Linux",
		"SshLoginCommand":  fmt.Sprintf("ssh -p 2%03d root@ssh.compshare.example.cn", 200+i),
		"JupyterUrl":       fmt.Sprintf("https://jupyter-%02d.compshare.example.cn/lab", i),
		"TotalDiskSpace":   500,
		"BootDiskState":    "Normal",
		"StorageType":      "CLOUD_SSD",
		"MachineType":      "G-GPU-4090",
		"HostType":         "GPU",
		"UHostType":        "GPU",
		"CpuPlatform":      "Intel/Xeon-Platinum-8352V",
		"NetCapability":    "Super",
		"NetworkState":     "Connected",
		"SubnetType":       "Private",
		"LifeCycle":        "Normal",
		"AutoRenew":        "No",
		"Remark":           "",
		"Tag":              "Default",
		"IsolationGroup":   "",
		"RestrictMode":     "No",
		"HotPlugMaxCpu":    64,
		"HotplugFeature":   true,
		"HpcFeature":       false,
		"CloudInitFeature": true,
		"IPv6Feature":      false,
		"EpcInstance":      false,
		"SecGroupInstance": false,
		"HiddenKvm":        false,
		"RdmaClusterId":    "",
		"KeyPair":          map[string]any{},
		"SpotAttribute":    map[string]any{},
		"UDHostAttribute":  map[string]any{},
		"IPSet": []any{
			map[string]any{
				"IP": "10.9.0." + fmt.Sprint(10+i), "IPId": "eip-1exampleip001", "Type": "Private",
				"VPCId": "uvpc-1examplevpc0", "SubnetId": "subnet-1examplesb", "Mac": "52:54:00:00:00:01",
				"Bandwidth": 0, "Weight": 0, "Default": "false", "IPMode": "static",
				"NetworkInterfaceId": "eni-1exampleeni01",
			},
		},
		"DiskSet": []any{
			map[string]any{
				"DiskId": "udisk-1exampledsk", "DiskType": "CLOUD_SSD", "Size": 500,
				"Drive": "/dev/vda", "IsBoot": "True", "Encrypted": "false", "Type": "Boot",
			},
		},
	}
}

func describeInstanceResult(n int) map[string]any {
	rows := make([]any, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, describeInstanceRow(i))
	}
	return map[string]any{
		"Action":     "DescribeCompShareInstanceResponse",
		"RetCode":    0,
		"TotalCount": n,
		"UHostSet":   rows,
	}
}

func TestFormatToolResult_RealInstancePayloadStaysParseable(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 10, 30} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			out := FormatToolResult(describeInstanceResult(n))

			var parsed map[string]any
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				tail := out
				if len(tail) > 120 {
					tail = tail[len(tail)-120:]
				}
				t.Fatalf("a tool result handed to the model must be parseable JSON; "+
					"n=%d failed to unmarshal (%v)\nit ends: ...%s", n, err, tail)
			}
			if got := utf8.RuneCountInString(out); got > maxToolResultRunes {
				t.Fatalf("n=%d: %d runes exceeds the %d cap", n, got, maxToolResultRunes)
			}
		})
	}
}

// A truncated list must carry enough information for the model to reconcile the
// retained rows with TotalCount.
func TestFormatToolResult_ATruncatedListSaysSoToTheModel(t *testing.T) {
	for _, n := range []int{3, 4, 10, 30} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			out := FormatToolResult(describeInstanceResult(n))

			var parsed map[string]any
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("precondition: result must parse: %v", err)
			}
			rows, ok := parsed["UHostSet"].([]any)
			if !ok {
				t.Fatalf("UHostSet missing from the model-visible result: %s", out)
			}

			// Count what the model can actually read, and find the notice.
			readable, notice := 0, ""
			for _, row := range rows {
				switch typed := row.(type) {
				case map[string]any:
					readable++
				case string:
					notice = typed
				}
			}

			if readable >= n {
				return // nothing was dropped; no notice owed
			}
			if notice == "" {
				t.Fatalf("n=%d: only %d of %d rows survived and the payload does not say so — "+
					"the model is told TotalCount=%d and shown %d rows with no marker, "+
					"which is an invitation to invent the rest\ngot: %s",
					n, readable, n, n, readable, out)
			}
			if !strings.Contains(notice, fmt.Sprint(n)) {
				t.Errorf("n=%d: the notice must name the true total so the model can reconcile it "+
					"with TotalCount; got %q", n, notice)
			}
			if readable > 0 && !strings.Contains(notice, fmt.Sprint(readable)) {
				t.Errorf("n=%d: the notice must name how many rows are actually readable (%d); got %q",
					n, readable, notice)
			}
		})
	}
}

func TestFormatToolResult_GiantScalarStaysParseable(t *testing.T) {
	out := FormatToolResult(map[string]any{
		"RetCode": 0,
		"Action":  "DiagnoseSomething",
		"log":     strings.Repeat("x", 10000),
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("a giant scalar must still yield parseable JSON, got %v\n%s", err, out)
	}
	if got := utf8.RuneCountInString(out); got > maxToolResultRunes {
		t.Fatalf("%d runes exceeds the %d cap", got, maxToolResultRunes)
	}
	if _, ok := parsed["RetCode"]; !ok {
		t.Error("the identifying scalars must survive so the model knows which call this was")
	}
}

// Nested image versions exercise recursive list truncation.
func TestFormatToolResult_NestedListStaysParseable(t *testing.T) {
	inner := make([]any, 100)
	for i := range inner {
		inner[i] = map[string]any{"ImageId": fmt.Sprintf("uimage-%03d", i), "Name": strings.Repeat("镜", 60)}
	}
	out := FormatToolResult(map[string]any{
		"RetCode": 0,
		"CompshareImageGroup": []any{
			map[string]any{"ImageName": "PyTorch", "Data": inner},
		},
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("a nested list must still yield parseable JSON, got %v\n%s", err, out)
	}
	if got := utf8.RuneCountInString(out); got > maxToolResultRunes {
		t.Fatalf("%d runes exceeds the %d cap", got, maxToolResultRunes)
	}
}

// Results that fit are returned unchanged.
func TestFormatToolResult_UnderCapIsByteIdentical(t *testing.T) {
	small := describeInstanceResult(1)
	direct, err := json.Marshal(small)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if utf8.RuneCount(direct) > maxToolResultRunes {
		t.Fatalf("precondition: a single instance must fit under the cap, got %d runes",
			utf8.RuneCount(direct))
	}
	if got := FormatToolResult(small); got != string(direct) {
		t.Errorf("a result under the cap must be returned untouched\nwant %s\ngot  %s", direct, got)
	}
}

func TestFormatToolResultTraceMeasuresTheGenericFormattingLayer(t *testing.T) {
	t.Run("under cap", func(t *testing.T) {
		input := map[string]any{"answer": "可用"}
		out, trace := FormatToolResultWithTrace(input)
		requireRawRunes(t, trace, utf8.RuneCountInString(out))
		if trace.VisibleRunes != utf8.RuneCountInString(out) {
			t.Fatalf("visible runes = %d, actual = %d", trace.VisibleRunes, utf8.RuneCountInString(out))
		}
		if trace.Truncated {
			t.Fatal("an under-cap result must not be marked truncated")
		}
	})

	t.Run("CJK truncation counts runes not bytes", func(t *testing.T) {
		input := map[string]any{"rows": []any{strings.Repeat("数", maxToolResultRunes+100)}}
		out, trace := FormatToolResultWithTrace(input)
		if trace.RawRunes == nil || *trace.RawRunes <= maxToolResultRunes {
			t.Fatalf("raw runes = %v, want a measured over-cap input", trace.RawRunes)
		}
		if !trace.Truncated {
			t.Fatal("an over-cap result must be marked truncated")
		}
		actualVisible := utf8.RuneCountInString(out)
		if trace.VisibleRunes != actualVisible || actualVisible > maxToolResultRunes {
			t.Fatalf("visible runes = %d, actual = %d, cap = %d", trace.VisibleRunes, actualVisible, maxToolResultRunes)
		}
		if len(out) == actualVisible {
			t.Fatal("precondition: CJK fixture must distinguish byte count from rune count")
		}
	})

	t.Run("unserializable raw size stays absent", func(t *testing.T) {
		out, trace := FormatToolResultWithTrace(map[string]any{"bad": make(chan int)})
		if trace.RawRunes != nil {
			t.Fatalf("raw runes = %v, want absent because serialization failed", *trace.RawRunes)
		}
		if trace.VisibleRunes != utf8.RuneCountInString(out) {
			t.Fatalf("visible runes = %d, actual = %d", trace.VisibleRunes, utf8.RuneCountInString(out))
		}
		if trace.Truncated {
			t.Fatal("serialization failure is not generic size truncation")
		}
	})
}

func requireRawRunes(t *testing.T, trace ToolResultFormatTrace, want int) {
	t.Helper()
	if trace.RawRunes == nil {
		t.Fatal("raw runes unexpectedly absent")
	}
	if *trace.RawRunes != want {
		t.Fatalf("raw runes = %d, want %d", *trace.RawRunes, want)
	}
}
