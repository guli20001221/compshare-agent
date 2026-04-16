package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
)

// goldenExecutor is a mock executor that returns controlled responses
// so golden tests don't depend on real API state.
type goldenExecutor struct {
	results map[string]map[string]any
	calls   []string
}

func (e *goldenExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, action)
	if r, ok := e.results[action]; ok {
		return r, nil
	}
	return map[string]any{"Action": action, "RetCode": 0}, nil
}

// singleInstanceExecutor returns a mock with one running 4090 instance.
func singleInstanceExecutor() *goldenExecutor {
	return &goldenExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-xxx", "Name": "my-gpu", "State": "Running",
				"GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
				"Zone": "cn-wlcb-01", "InstanceType": "Container",
				"Softwares": []any{
					map[string]any{"Name": "JupyterLab", "URL": "http://1.2.3.4:8888?token=abc"},
					map[string]any{"Name": "SSH", "URL": "ssh://root@1.2.3.4:22"},
				},
			},
		}},
		"DescribeCompShareImages":        {"ImageSet": []any{map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04 CUDA 12"}}},
		"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}}},
		"GetCompShareInstanceUserPrice":      {"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.58}}},
		"CreateCompShareInstance":        {"UHostIds": []any{"uhost-new001"}},
		"StopCompShareInstance":          {"RetCode": 0},
		"StartCompShareInstance":         {"RetCode": 0},
		"RebootCompShareInstance":        {"RetCode": 0},
		"ModifyCompShareInstanceName":    {"RetCode": 0},
		"ResetCompShareInstancePassword": {"RetCode": 0},
		"UpdateCompShareStopScheduler":   {"RetCode": 0},
		"DeleteCompShareStopScheduler":   {"RetCode": 0},
		"DescribeCompShareSoftwarePort":  {"SoftwarePort": []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}, map[string]any{"Software": "SSH", "Port": float64(22)}}},
		"GetCompShareInstanceMonitor":    {"Data": map[string]any{"List": []any{}}},
		"DescribeCompShareJupyterToken":  {"DataSet": []any{map[string]any{"UHostId": "uhost-xxx", "JupyterToken": "eyJhbGciOiJIUzI1NiJ9.test-token"}}},
	}}
}

// billingExecutor returns a mock with one running + one stopped instance, with pricing fields
// including CompShareImagePrice for paid community image testing.
// NOTE: The mock returns full pricing data on every call regardless of args.
// The two-step price-discovery logic (step1 no-price list → step2 with-price query)
// is properly tested in internal/diagnosis/billing_anomaly_test.go unit tests.
func billingExecutor() *goldenExecutor {
	return &goldenExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-run", "Name": "train-gpu", "State": "Running",
				"GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
				"InstancePrice": float64(1.58), "DiskPrice": float64(0.05),
				"CompShareImagePrice": float64(0.30),
			},
			map[string]any{
				"UHostId": "uhost-off", "Name": "idle-gpu", "State": "Stopped",
				"GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
				"InstancePrice": float64(1.58), "DiskPrice": float64(0.05),
				"CompShareImagePrice": float64(0.30),
			},
		}},
	}}
}

// startingExecutor returns a mock with one Starting instance (for Starting-not-billed test).
func startingExecutor() *goldenExecutor {
	return &goldenExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-boot", "Name": "boot-gpu", "State": "Starting",
				"GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			},
		}},
	}}
}

// initFailureExecutor returns a mock with one Install Fail instance.
func initFailureExecutor() *goldenExecutor {
	return &goldenExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-fail", "Name": "broken-gpu", "State": "Install Fail",
				"GpuType": "A100", "GPU": float64(1), "ChargeType": "Dynamic",
				"CompShareImageName": "PyTorch 2.1",
			},
		}},
	}}
}

type goldenScope string

const (
	goldenScopeEngine  goldenScope = "engine"
	goldenScopeRealCLI goldenScope = "real_cli"
)

// multiInstanceExecutor returns a mock with three instances to exercise
// disambiguation and explicit-ID routing in a way that is closer to the
// real account state used in manual CLI validation.
func multiInstanceExecutor() *goldenExecutor {
	return &goldenExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running",
				"GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
				"Zone": "cn-wlcb-01", "InstanceType": "Container",
				"Softwares": []any{
					map[string]any{"Name": "JupyterLab", "URL": "http://1.2.3.4:8888?token=token-a"},
					map[string]any{"Name": "SSH", "URL": "ssh://root@1.2.3.4:22"},
				},
			},
			map[string]any{
				"UHostId": "uhost-2", "Name": "train-b", "State": "Running",
				"GpuType": "A100", "GPU": float64(1), "ChargeType": "Dynamic",
				"Zone": "cn-wlcb-01", "InstanceType": "Container",
				"Softwares": []any{
					map[string]any{"Name": "JupyterLab", "URL": "http://2.3.4.5:8888?token=token-b"},
					map[string]any{"Name": "SSH", "URL": "ssh://root@2.3.4.5:22"},
				},
			},
			map[string]any{
				"UHostId": "uhost-3", "Name": "dev", "State": "Stopped",
				"GpuType": "3090", "GPU": float64(1), "ChargeType": "Month",
				"Zone": "cn-wlcb-01", "InstanceType": "Container",
			},
		}},
		"DescribeCompShareImages":        {"ImageSet": []any{map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04 CUDA 12"}}},
		"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}}},
		"GetCompShareInstanceUserPrice":      {"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.58}}},
		"CreateCompShareInstance":        {"UHostIds": []any{"uhost-new001"}},
		"StopCompShareInstance":          {"RetCode": 0},
		"StartCompShareInstance":         {"RetCode": 0},
		"RebootCompShareInstance":        {"RetCode": 0},
		"ModifyCompShareInstanceName":    {"RetCode": 0},
		"ResetCompShareInstancePassword": {"RetCode": 0},
		"UpdateCompShareStopScheduler":   {"RetCode": 0},
		"DeleteCompShareStopScheduler":   {"RetCode": 0},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
				map[string]any{"Software": "SSH", "Port": float64(22)},
			},
		},
		"GetCompShareInstanceMonitor":   {"Data": map[string]any{"List": []any{}}},
		"DescribeCompShareJupyterToken": {"DataSet": []any{map[string]any{"UHostId": "uhost-2", "JupyterToken": "eyJhbGciOiJIUzI1NiJ9.test-token"}}},
	}}
}

// goldenStep defines one turn in a multi-turn golden conversation.
type goldenStep struct {
	Input            string
	ExpectToolCalls  []string
	RejectToolCalls  []string
	ReplyContains    []string
	ReplyNotContains []string
	ExpectNoToolCall bool
	ExpectFirstTool  string
}

// goldenCase defines one golden script test.
type goldenCase struct {
	ID          string
	Scope       goldenScope
	Input       string
	UserContext string          // injected as user state; empty = default
	Executor    *goldenExecutor // nil = use singleInstanceExecutor()

	// Assertions (all optional — nil = skip that check)
	ExpectToolCalls  []string // at least one of these tools should appear in events
	RejectToolCalls  []string // none of these should appear in events
	ReplyContains    []string // reply text must contain all of these
	ReplyNotContains []string // reply text must not contain any of these
	ExpectBlocked    bool     // expect a StepBlocked event
	ExpectDisplay    bool     // expect a non-empty Display in some event
	DisplayContains  string   // Display content must contain this substring
	ExpectNoToolCall bool     // expect zero tool calls (knowledge_qa / clarification)
	ExpectFirstTool  string   // first StepToolCall action must match when set

	Steps []goldenStep // if non-empty, multi-turn mode (Input is ignored)
}

func TestMultiInstanceExecutor_DescribeCompShareInstance(t *testing.T) {
	exec := multiInstanceExecutor()
	result, err := exec.Execute(context.Background(), "DescribeCompShareInstance", nil)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	items, ok := result["UHostSet"].([]any)
	if !ok {
		t.Fatalf("UHostSet type = %T, want []any", result["UHostSet"])
	}
	if len(items) != 3 {
		t.Fatalf("len(UHostSet) = %d, want 3", len(items))
	}

	gotIDs := make([]string, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("UHostSet item type = %T, want map[string]any", item)
		}
		gotIDs = append(gotIDs, row["UHostId"].(string))
	}

	wantIDs := []string{"uhost-1", "uhost-2", "uhost-3"}
	for i, want := range wantIDs {
		if gotIDs[i] != want {
			t.Fatalf("UHostSet[%d] = %q, want %q", i, gotIDs[i], want)
		}
	}
}

func TestValidateGoldenCase_ExplicitIDRequiresFirstWorkflow(t *testing.T) {
	gc := goldenCase{
		ID:              "explicit_id_reboot",
		ExpectToolCalls: []string{"RebootInstanceWorkflow"},
		ExpectFirstTool: "RebootInstanceWorkflow",
	}
	events := []engine.StepEvent{
		{Type: engine.StepToolCall, Action: "DescribeCompShareInstance"},
		{Type: engine.StepToolResult, Action: "DescribeCompShareInstance"},
	}

	failures := validateGoldenCase(gc, "这里是实例详情。", events)
	if len(failures) == 0 {
		t.Fatal("validateGoldenCase returned no failures, want first-tool mismatch")
	}

	found := false
	for _, failure := range failures {
		if strings.Contains(failure, "first tool") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("failures = %v, want first-tool mismatch", failures)
	}
}

var engineGoldenCases = []goldenCase{
	{
		ID:              "golden_01_create",
		Input:           "帮我开一台4090",
		UserContext:     "用户当前没有实例。",
		ExpectToolCalls: []string{"CreateInstanceWorkflow"},
	},
	{
		ID:              "golden_02_start",
		Input:           "把 uhost-xxx 开机",
		UserContext:     "您有 1 个实例（1 个关机）\n- test (uhost-xxx): GPU=4090×1, 状态=关机, 计费=Dynamic",
		ExpectToolCalls: []string{"StartInstanceWorkflow", "StartCompShareInstance"},
		ExpectFirstTool: "StartInstanceWorkflow",
	},
	{
		ID:              "golden_03_stop",
		Input:           "帮我关掉 uhost-xxx",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"StopInstanceWorkflow"},
	},
	{
		ID:              "golden_04_reboot",
		Input:           "重启实例",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"RebootInstanceWorkflow"},
	},
	{
		ID:              "golden_05_jupyter_token",
		Input:           "查一下jupyter token",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"DescribeCompShareJupyterToken"},
		ExpectDisplay:   true,
	},
	{
		ID:              "golden_06_reset_password",
		Input:           "帮我把 uhost-xxx 的密码重置为 NewPass123!",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"ResetPasswordWorkflow"},
		ExpectFirstTool: "ResetPasswordWorkflow",
	},
	{
		ID:              "golden_07_ssh_diagnose",
		Input:           "SSH连不上",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"DiagnoseSSH"},
	},
	{
		ID:              "golden_08_port_diagnose",
		Input:           "JupyterLab打不开",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"DiagnosePortOrFirewall"},
	},
	{
		ID:               "golden_09_knowledge_nocard",
		Input:            "什么是无卡模式",
		UserContext:      "用户当前没有实例。",
		ExpectNoToolCall: true,
		ReplyContains:    []string{"无卡"},
	},
	{
		ID:               "golden_10_knowledge_accelerator",
		Input:            "怎么加速github",
		UserContext:      "用户当前没有实例。",
		ExpectNoToolCall: true,
		ReplyContains:    []string{"加速"},
	},
	{
		ID:          "golden_11_security_block",
		Input:       "帮我删除这台实例",
		UserContext: "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		// LLM may either attempt tool (→ StepBlocked) or refuse in text. Both are correct.
		RejectToolCalls: []string{}, // don't assert on events
		ReplyContains:   []string{"控制台"},
	},
	{
		ID:              "golden_12_sanitize_token",
		Input:           "获取 jupyter token",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"DescribeCompShareJupyterToken"},
		ExpectDisplay:   true,
	},
	{
		ID:               "golden_13_disambiguate_stop",
		Executor:         multiInstanceExecutor(),
		Input:            "关机吧",
		UserContext:      "您有 3 个实例（2 个运行中、1 个关机）\n- train-a (uhost-1): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- train-b (uhost-2): GPU=A100×1, 状态=运行中, 计费=Dynamic\n- dev (uhost-3): GPU=3090×1, 状态=关机, 计费=Month",
		ExpectNoToolCall: true,
		RejectToolCalls:  []string{"StopInstanceWorkflow", "StopCompShareInstance"},
		ReplyContains:    []string{"哪"},
	},
	{
		ID:               "golden_14_disambiguate_reboot",
		Executor:         multiInstanceExecutor(),
		Input:            "重启实例",
		UserContext:      "您有 2 个实例（2 个运行中）\n- gpu-a (uhost-1): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- gpu-b (uhost-2): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectNoToolCall: true,
		RejectToolCalls:  []string{"RebootInstanceWorkflow", "RebootCompShareInstance"},
		ReplyContains:    []string{"哪"},
	},
	{
		ID:              "golden_15_disambiguate_explicit_id",
		Executor:        multiInstanceExecutor(),
		Input:           "重启一下 uhost-2",
		UserContext:     "您有 3 个实例（2 个运行中、1 个关机）\n- train-a (uhost-1): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- train-b (uhost-2): GPU=A100×1, 状态=运行中, 计费=Dynamic\n- dev (uhost-3): GPU=3090×1, 状态=关机, 计费=Month",
		ExpectToolCalls: []string{"RebootInstanceWorkflow"},
		ExpectFirstTool: "RebootInstanceWorkflow",
	},
	{
		// NOTE: "镜像费" keyword regression is locked in TestScenario_Billing_RunningAndStopped
		// (scenario_test.go) which asserts directly on the diagnosis chain JSON.
		// The engine golden test checks LLM reply which may rephrase, so we use broader keywords.
		ID:              "golden_16_billing_diagnosis",
		Executor:        billingExecutor(),
		Input:           "为什么扣了这么多钱",
		UserContext:     "您有 2 个实例（1 个运行中、1 个关机）\n- train-gpu (uhost-run): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- idle-gpu (uhost-off): GPU=4090×1, 状态=关机, 计费=Dynamic",
		ExpectToolCalls: []string{"DiagnoseBilling"},
		ReplyContains:   []string{"费用", "关机"},
		ReplyNotContains: []string{"GetCompShareInstancePrice", "DescribeCompShareInstance"},
	},
	{
		ID:              "golden_17_init_failure_diagnosis",
		Executor:        initFailureExecutor(),
		Input:           "实例初始化失败了怎么办",
		UserContext:     "您有 1 个实例（1 个初始化失败）\n- broken-gpu (uhost-fail): GPU=A100×1, 状态=初始化失败, 计费=Dynamic",
		ExpectToolCalls: []string{"DiagnoseInitFailure"},
		ReplyContains:   []string{"初始化失败"},
	},
	{
		// NOTE: "不产生费用" regression is locked in TestScenario_InitFailure_Starting
		// (scenario_test.go) which asserts directly on the diagnosis chain JSON.
		ID:              "golden_18_starting_not_billed",
		Executor:        startingExecutor(),
		Input:           "实例卡在启动中不动了",
		UserContext:     "您有 1 个实例（1 个启动中）\n- boot-gpu (uhost-boot): GPU=4090×1, 状态=启动中, 计费=Dynamic",
		ExpectToolCalls: []string{"DiagnoseInitFailure"},
		ReplyContains:   []string{"启动"},
	},
	{
		ID:              "golden_19_set_stop_scheduler",
		Input:           "1小时后自动关机",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"SetStopSchedulerWorkflow"},
		ExpectFirstTool: "SetStopSchedulerWorkflow",
	},
	{
		ID:              "golden_20_cancel_stop_scheduler",
		Input:           "取消定时关机",
		UserContext:     "您有 1 个实例（1 个运行中）\n- my-gpu (uhost-xxx): GPU=4090×1, 状态=运行中, 计费=Dynamic",
		ExpectToolCalls: []string{"CancelStopSchedulerWorkflow"},
		ExpectFirstTool: "CancelStopSchedulerWorkflow",
	},
	{
		ID:               "golden_21_disambiguate_scheduler",
		Executor:         multiInstanceExecutor(),
		Input:            "帮我设个定时关机",
		UserContext:      "您有 2 个实例（2 个运行中）\n- train-a (uhost-1): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- train-b (uhost-2): GPU=A100×1, 状态=运行中, 计费=Dynamic",
		ExpectNoToolCall: true,
		RejectToolCalls:  []string{"SetStopSchedulerWorkflow"},
		ReplyContains:    []string{"哪"},
	},

	// ── Multi-turn golden cases ─────────────────────────────────────────
	{
		ID:       "multi_01_disambiguate_stop",
		Executor: multiInstanceExecutor(),
		UserContext: "您有 3 个实例（2 个运行中、1 个关机）\n- train-a (uhost-1): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- train-b (uhost-2): GPU=A100×1, 状态=运行中, 计费=Dynamic\n- dev (uhost-3): GPU=3090×1, 状态=关机, 计费=Month",
		Steps: []goldenStep{
			{
				Input:            "关机吧",
				ExpectNoToolCall: true,
				RejectToolCalls:  []string{"StopInstanceWorkflow", "StopCompShareInstance"},
				ReplyContains:    []string{"哪"},
			},
			{
				Input:           "关掉 train-a",
				ExpectToolCalls: []string{"StopInstanceWorkflow"},
				ExpectFirstTool: "StopInstanceWorkflow",
				ReplyContains:   []string{"关机"},
			},
		},
	},
	{
		ID:       "multi_02_scheduler_two_turn",
		Executor: multiInstanceExecutor(),
		UserContext: "您有 2 个实例（2 个运行中）\n- train-a (uhost-1): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- train-b (uhost-2): GPU=A100×1, 状态=运行中, 计费=Dynamic",
		Steps: []goldenStep{
			{
				Input:            "帮我设个定时关机",
				ExpectNoToolCall: true,
				ReplyContains:    []string{"哪"},
			},
			{
				Input:           "就那台 4090，1小时后关",
				ExpectToolCalls: []string{"SetStopSchedulerWorkflow"},
				ExpectFirstTool: "SetStopSchedulerWorkflow",
			},
		},
	},
	{
		ID:       "multi_03_billing_followup",
		Executor: billingExecutor(),
		UserContext: "您有 2 个实例（1 个运行中、1 个关机）\n- train-gpu (uhost-run): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- idle-gpu (uhost-off): GPU=4090×1, 状态=关机, 计费=Dynamic",
		Steps: []goldenStep{
			{
				Input:           "为什么在扣费",
				ExpectToolCalls: []string{"DiagnoseBilling"},
				ReplyContains:   []string{"费用"},
			},
			{
				Input:            "那关机后还有什么费",
				ExpectNoToolCall: true,
				ReplyContains:    []string{"磁盘"},
			},
		},
	},
	{
		ID:       "multi_04_diagnosis_supplement_id",
		Executor: multiInstanceExecutor(),
		UserContext: "您有 3 个实例（2 个运行中、1 个关机）\n- train-a (uhost-1): GPU=4090×1, 状态=运行中, 计费=Dynamic\n- train-b (uhost-2): GPU=A100×1, 状态=运行中, 计费=Dynamic\n- dev (uhost-3): GPU=3090×1, 状态=关机, 计费=Month",
		Steps: []goldenStep{
			{
				Input:            "SSH连不上",
				ExpectNoToolCall: true,
				ReplyContains:    []string{"哪"},
			},
			{
				Input:           "就是 uhost-1 那台",
				ExpectToolCalls: []string{"DiagnoseSSH"},
			},
		},
	},
}

// loadRealCLIGoldenCases loads CLI golden cases from the JSON file — the single
// source of truth. The Go slice was removed to eliminate dual-source drift.
func loadRealCLIGoldenCases(t *testing.T) []cliGoldenCase {
	t.Helper()
	data, err := os.ReadFile("real_cli_golden_cases.json")
	if err != nil {
		t.Fatalf("failed to load real_cli_golden_cases.json: %v", err)
	}
	var cases []cliGoldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("failed to parse real_cli_golden_cases.json: %v", err)
	}
	return cases
}

// cliGoldenStep mirrors a single step in a multi-turn CLI golden case.
type cliGoldenStep struct {
	Input            string   `json:"input"`
	Confirm          string   `json:"confirm,omitempty"`
	ExpectToolCalls  []string `json:"expect_tool_calls,omitempty"`
	ExpectFirstTool  string   `json:"expect_first_tool,omitempty"`
	ExpectNoToolCall bool     `json:"expect_no_tool_call,omitempty"`
	RejectToolCalls  []string `json:"reject_tool_calls,omitempty"`
	ReplyContains    []string `json:"reply_contains,omitempty"`
	ReplyContainsAny []string `json:"reply_contains_any,omitempty"`
	ReplyNotContains []string `json:"reply_not_contains,omitempty"`
}

// cliGoldenCase mirrors the JSON structure of real_cli_golden_cases.json.
type cliGoldenCase struct {
	ID               string          `json:"id"`
	Input            string          `json:"input"`
	Confirm          string          `json:"confirm,omitempty"`
	ExpectToolCalls  []string        `json:"expect_tool_calls,omitempty"`
	ExpectFirstTool  string          `json:"expect_first_tool,omitempty"`
	ExpectNoToolCall bool            `json:"expect_no_tool_call,omitempty"`
	ExpectDisplay    bool            `json:"expect_display,omitempty"`
	ReplyContains    []string        `json:"reply_contains,omitempty"`
	ReplyContainsAny []string        `json:"reply_contains_any,omitempty"`
	ReplyNotContains []string        `json:"reply_not_contains,omitempty"`
	Steps            []cliGoldenStep `json:"steps,omitempty"`
}

func validateGoldenCase(gc goldenCase, reply string, events []engine.StepEvent) []string {
	var failures []string
	toolsSeen := map[string]bool{}
	var hadBlocked, hadDisplay bool
	var displayContent string
	firstTool := ""

	for _, e := range events {
		if e.Type == engine.StepToolCall && e.Action != "" && firstTool == "" {
			firstTool = e.Action
		}
		if e.Action != "" {
			toolsSeen[e.Action] = true
		}
		if e.Type == engine.StepBlocked {
			hadBlocked = true
		}
		if e.Display != "" {
			hadDisplay = true
			displayContent = e.Display
		}
	}

	if len(gc.ExpectToolCalls) > 0 {
		found := false
		for _, tool := range gc.ExpectToolCalls {
			if toolsSeen[tool] {
				found = true
				break
			}
		}
		if !found {
			failures = append(failures, fmt.Sprintf("expected one of %v in events, got %v", gc.ExpectToolCalls, keys(toolsSeen)))
		}
	}

	for _, tool := range gc.RejectToolCalls {
		if toolsSeen[tool] {
			failures = append(failures, fmt.Sprintf("rejected tool %s appeared in events", tool))
		}
	}

	if gc.ExpectNoToolCall {
		for _, e := range events {
			if e.Type == engine.StepToolCall && e.Action != "" {
				failures = append(failures, fmt.Sprintf("expected no tool call, got %s", e.Action))
				break
			}
		}
	}

	if gc.ExpectFirstTool != "" {
		if firstTool == "" {
			failures = append(failures, fmt.Sprintf("first tool = (none), want %s", gc.ExpectFirstTool))
		} else if firstTool != gc.ExpectFirstTool {
			failures = append(failures, fmt.Sprintf("first tool = %s, want %s", firstTool, gc.ExpectFirstTool))
		}
	}

	for _, kw := range gc.ReplyContains {
		if !strings.Contains(reply, kw) {
			failures = append(failures, fmt.Sprintf("reply missing keyword %q", kw))
		}
	}

	for _, kw := range gc.ReplyNotContains {
		if strings.Contains(reply, kw) {
			failures = append(failures, fmt.Sprintf("reply should not contain %q", kw))
		}
	}

	if gc.ExpectBlocked && !hadBlocked {
		failures = append(failures, "expected StepBlocked event, none found")
	}
	if gc.ExpectDisplay && !hadDisplay {
		failures = append(failures, "expected Display content, none found")
	}
	if gc.DisplayContains != "" && !strings.Contains(displayContent, gc.DisplayContains) {
		failures = append(failures, fmt.Sprintf("display missing %q", gc.DisplayContains))
	}

	return failures
}

// TestGoldenCatalogIntegrity runs by default (no API key needed) and validates
// that the golden catalogs are structurally sound and have not degraded.
func TestGoldenCatalogIntegrity(t *testing.T) {
	// Engine golden cases: basic structural checks
	if len(engineGoldenCases) < 25 {
		t.Errorf("engine golden cases = %d, want >= 25", len(engineGoldenCases))
	}
	ids := map[string]bool{}
	for _, gc := range engineGoldenCases {
		if gc.ID == "" {
			t.Error("engine golden case has empty ID")
		}
		if gc.Input == "" && len(gc.Steps) == 0 {
			t.Errorf("engine golden case %s has empty Input and no Steps", gc.ID)
		}
		if ids[gc.ID] {
			t.Errorf("duplicate engine golden ID: %s", gc.ID)
		}
		ids[gc.ID] = true
	}

	// Real CLI golden cases: load from JSON (single source of truth)
	cliCases := loadRealCLIGoldenCases(t)
	if len(cliCases) < 25 {
		t.Errorf("real CLI golden cases = %d, want >= 25", len(cliCases))
	}
	cliIDs := map[string]bool{}
	for _, gc := range cliCases {
		if gc.ID == "" {
			t.Error("CLI golden case has empty ID")
		}
		if gc.Input == "" && len(gc.Steps) == 0 {
			t.Errorf("CLI golden case %s has empty Input and no Steps", gc.ID)
		}
		if cliIDs[gc.ID] {
			t.Errorf("duplicate CLI golden ID: %s", gc.ID)
		}
		cliIDs[gc.ID] = true
	}

	// Count parity: engine and CLI catalogs should have the same number of cases
	if len(engineGoldenCases) != len(cliCases) {
		t.Errorf("catalog count mismatch: engine=%d, CLI=%d", len(engineGoldenCases), len(cliCases))
	}
}

func TestGoldenScripts(t *testing.T) {
	modelName := *modelFlag
	if modelName == "" {
		t.Skip("use -model flag to specify model (e.g., -model 'Qwen3.6-Plus')")
	}

	m := FindModel(modelName)
	if m == nil {
		t.Fatalf("unknown model: %s", modelName)
	}
	if m.APIKey == "" {
		t.Skipf("SKIP %s: no API key", m.Name)
	}

	llmClient := llm.NewClient(config.LLMConfig{
		BaseURL: m.BaseURL,
		APIKey:  m.APIKey,
		Model:   m.ModelID,
	})

	for _, gc := range engineGoldenCases {
		t.Run(gc.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			// Use mock executor for controlled API responses
			exec := gc.Executor
			if exec == nil {
				exec = singleInstanceExecutor()
			}

			confirmFn := func(action string, args map[string]any) bool { return true }
			eng := engine.NewWithDeps(llmClient, exec, confirmFn)

			// Inject controlled user context
			userCtx := gc.UserContext
			if userCtx == "" {
				userCtx = "暂无用户信息"
			}
			eng.InitWithContext(userCtx)

			if len(gc.Steps) > 0 {
				// Multi-turn: call Chat() for each step, validate per-turn
				for si, step := range gc.Steps {
					var stepEvents []engine.StepEvent
					stepReply, stepErr := eng.Chat(ctx, step.Input, func(e engine.StepEvent) {
						stepEvents = append(stepEvents, e)
					})
					if stepErr != nil {
						t.Fatalf("Step %d Chat error: %v", si+1, stepErr)
					}

					t.Logf("Step %d Input: %s", si+1, step.Input)
					t.Logf("Step %d Reply: %.200s", si+1, stepReply)
					t.Logf("Step %d Events: %d", si+1, len(stepEvents))

					// Reuse validateGoldenCase with a temporary goldenCase
					stepGC := goldenCase{
						ExpectToolCalls:  step.ExpectToolCalls,
						RejectToolCalls:  step.RejectToolCalls,
						ReplyContains:    step.ReplyContains,
						ReplyNotContains: step.ReplyNotContains,
						ExpectNoToolCall: step.ExpectNoToolCall,
						ExpectFirstTool:  step.ExpectFirstTool,
					}
					stepFailures := validateGoldenCase(stepGC, stepReply, stepEvents)
					for _, failure := range stepFailures {
						t.Errorf("Step %d FAIL: %s", si+1, failure)
					}
				}
			} else {
				// Existing single-turn logic
				var events []engine.StepEvent
				reply, err := eng.Chat(ctx, gc.Input, func(e engine.StepEvent) {
					events = append(events, e)
				})
				if err != nil {
					t.Fatalf("Chat error: %v", err)
				}

				t.Logf("Reply: %.200s", reply)
				t.Logf("Events: %d", len(events))
				for i, e := range events {
					t.Logf("  [%d] type=%d action=%s msg=%s display=%s",
						i, e.Type, e.Action, truncate(e.Message, 80), truncate(e.Display, 40))
				}

				failures := validateGoldenCase(gc, reply, events)
				for _, failure := range failures {
					t.Errorf("FAIL: %s", failure)
				}

				if len(failures) == 0 {
					t.Logf("PASS")
				}
			}
		})
	}

}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// FormatGoldenReport generates a summary for golden script results.
func formatGoldenReport(title string, cases []goldenCase, results map[string]bool) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Golden Scripts Report — %s\n\n", time.Now().Format("2006-01-02")))
	sb.WriteString(fmt.Sprintf("**Scope:** %s\n\n", title))
	sb.WriteString("| # | Script | Result |\n")
	sb.WriteString("|---|--------|--------|\n")
	pass, total := 0, 0
	for _, gc := range cases {
		total++
		result := "FAIL"
		if results[gc.ID] {
			result = "PASS"
			pass++
		}
		sb.WriteString(fmt.Sprintf("| %d | %s | %s |\n", total, gc.ID, result))
	}
	sb.WriteString(fmt.Sprintf("\n**%d/%d passed**\n", pass, total))
	return sb.String()
}

// FormatGoldenReport generates a summary for automated engine golden results.
func FormatGoldenReport(results map[string]bool) string {
	return formatGoldenReport("Engine Golden Scripts Report", engineGoldenCases, results)
}

// FormatRealCLIGoldenChecklist renders the manual real-CLI golden catalog.
// Loads from real_cli_golden_cases.json — the single source of truth.
// NOTE: This function runs in _test.go context where CWD is the eval/ package directory.
func FormatRealCLIGoldenChecklist() string {
	data, err := os.ReadFile("real_cli_golden_cases.json")
	if err != nil {
		return fmt.Sprintf("Error loading real_cli_golden_cases.json: %v", err)
	}
	var cases []cliGoldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return fmt.Sprintf("Error parsing real_cli_golden_cases.json: %v", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Real CLI Golden Checklist - %s\n\n", time.Now().Format("2006-01-02")))
	sb.WriteString("| # | Script | Input | Primary Expectation |\n")
	sb.WriteString("|---|--------|-------|---------------------|\n")
	for i, gc := range cases {
		expectation := "manual verification"
		switch {
		case gc.ExpectFirstTool != "":
			expectation = "first tool: " + gc.ExpectFirstTool
		case len(gc.ExpectToolCalls) > 0:
			expectation = "tool: " + strings.Join(gc.ExpectToolCalls, "/")
		case gc.ExpectNoToolCall:
			expectation = "no tool call"
		}
		sb.WriteString(fmt.Sprintf("| %d | %s | %s | %s |\n", i+1, gc.ID, gc.Input, expectation))
	}
	return sb.String()
}
