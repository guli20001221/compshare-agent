package sshops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/stretchr/testify/require"
)

type contextDescriber struct {
	describe   map[string]any
	monitor    map[string]any
	monitorErr error
	calls      []string
}

func (d *contextDescriber) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	d.calls = append(d.calls, action)
	switch action {
	case "DescribeCompShareInstance":
		return d.describe, nil
	case "GetCompShareInstanceMonitor":
		return d.monitor, d.monitorErr
	default:
		return nil, nil
	}
}

func TestDiagnoseWithContextProjectsOnlyAllowlistedFacts(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	describer := &contextDescriber{
		describe: map[string]any{"UHostSet": []any{map[string]any{
			"UHostId":             "uhost-abc",
			"State":               "Running",
			"SshLoginCommand":     "ssh -p 23 root@198.51.100.9",
			"Password":            b64,
			"IPSet":               []any{"198.51.100.9"},
			"JupyterToken":        "jupyter-secret-value",
			"FileBrowserPassword": "filebrowser-secret-value",
			"GPU":                 float64(2),
			"GpuType":             "4090",
			"CompShareImageName":  "ComfyUI",
			"ImageType":           "App",
			"DiskSet":             []any{map[string]any{"DiskType": "Data", "Size": float64(100), "Status": "InUse"}},
			"Ports":               map[string]any{"HttpPorts": []any{float64(8188)}, "TcpPorts": []any{float64(6006)}},
			"TcpForwards":         []any{map[string]any{"InternalPort": float64(8188), "ExternalPort": float64(30188)}},
		}}},
		monitor: map[string]any{
			"Data": map[string]any{
				"List": []any{
					map[string]any{
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "cloudwatch_gpu_util",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1778420000), "Value": float64(87)},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	runner := &fakeRunner{res: Result{Output: "done", ContextApplied: true, Steps: []Step{
		{Command: "df -h", Disposition: "ran"},
		{Command: "rm -rf /root/.cache/pip", Disposition: "refused"},
	}}}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)
	modelContext := opscontext.Context{
		SchemaVersion:     opscontext.SchemaVersion,
		CurrentUserReport: &opscontext.UserReport{Text: "8188 打不开", Source: "chat.current_user", ObservedAt: "unknown", Status: opscontext.StatusReported},
	}

	_, err := svc.DiagnoseWithContext(context.Background(), describer, Owner{RequestUUID: "req", TurnID: "turn"}, "uhost-abc", "排查 Web UI", modelContext, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, describer.calls)
	require.Equal(t, opscontext.SchemaVersion, runner.lastContext.SchemaVersion)
	require.NotEmpty(t, runner.lastContext.PlatformFacts)
	for _, fact := range runner.lastContext.PlatformFacts {
		require.NotEmpty(t, fact.Source)
		require.NotEmpty(t, fact.ObservedAt)
		require.NotEmpty(t, fact.Status)
	}

	encoded, err := json.Marshal(runner.lastContext)
	require.NoError(t, err)
	text := string(encoded)
	for _, forbidden := range []string{
		"SshLoginCommand", "198.51.100.9", secretPW, "jupyter-secret-value", "filebrowser-secret-value",
		"Password", "IPSet", "JupyterToken", "FileBrowserPassword",
	} {
		require.NotContains(t, text, forbidden)
	}
	require.Contains(t, text, "8188")
	require.Contains(t, text, "instance.reported_ports")
	require.NotContains(t, text, "configured_ports")
	require.Contains(t, text, "not_observed")
	require.Contains(t, text, "monitor.gpu_usage")

	begin, done := audit.Events[0], audit.Events[1]
	require.Equal(t, opscontext.SchemaVersion, begin.ContextSchemaVersion)
	require.NotZero(t, begin.ContextFactCoverage&opscontext.CoveragePorts)
	require.NotZero(t, begin.ContextFactCoverage&opscontext.CoverageMonitor)
	require.Equal(t, 1, done.CommandsRan)
	require.Equal(t, 1, done.CommandsRefused)
	require.Equal(t, "targeted_validation", done.FirstCommandClass)
	require.Equal(t, opscontext.SchemaVersion, done.ContextSchemaVersion)
	require.NotZero(t, done.ContextFactCoverage)
}

func TestFinishedAuditClearsUnconfirmedContextReceipt(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	describer := &contextDescriber{describe: describeResp("ssh root@10.0.0.9", b64)}
	runner := &fakeRunner{res: Result{Output: "done"}}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)
	modelContext := opscontext.Context{
		SchemaVersion: opscontext.SchemaVersion,
		CurrentUserReport: &opscontext.UserReport{Text: "Web UI 无法访问", Source: "chat.current_user",
			ObservedAt: "unknown", Status: opscontext.StatusReported},
	}

	_, err := svc.DiagnoseWithContext(context.Background(), describer, Owner{TurnID: "turn"}, "uhost-abc", "排查服务", modelContext, nil, nil)
	require.NoError(t, err)
	require.Equal(t, opscontext.SchemaVersion, audit.Events[0].ContextSchemaVersion)
	require.NotZero(t, audit.Events[0].ContextFactCoverage)
	require.Zero(t, audit.Events[1].ContextSchemaVersion)
	require.Zero(t, audit.Events[1].ContextFactCoverage)
}

func TestMonitorContextFailureDoesNotBlockSSHDiagnosis(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	describer := &contextDescriber{
		describe:   describeResp("ssh root@10.0.0.9", b64),
		monitorErr: errors.New("monitor unavailable"),
	}
	runner := &fakeRunner{res: Result{Output: "SSH diagnosis still ran"}}
	svc := NewService(runner, &MemAuditWriter{})

	_, err := svc.DiagnoseWithContext(context.Background(), describer, Owner{TurnID: "turn"}, "uhost-abc", "task",
		opscontext.Context{SchemaVersion: opscontext.SchemaVersion}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, runner.calls)
	require.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, describer.calls)
	var monitorUnknown bool
	for _, fact := range runner.lastContext.PlatformFacts {
		if fact.Key == "monitor" && fact.Status == opscontext.StatusUnknown {
			monitorUnknown = true
		}
	}
	require.True(t, monitorUnknown)
}

func TestContextDoesNotChangeTaskHash(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	describe := describeResp("ssh root@10.0.0.9", b64)
	firstAudit, secondAudit := &MemAuditWriter{}, &MemAuditWriter{}
	first := NewService(&fakeRunner{res: Result{Output: "ok"}}, firstAudit)
	second := NewService(&fakeRunner{res: Result{Output: "ok"}}, secondAudit)
	firstContext := opscontext.Context{SchemaVersion: opscontext.SchemaVersion, CurrentUserReport: &opscontext.UserReport{Text: "8188 不通", Source: "chat.current_user", ObservedAt: "unknown", Status: opscontext.StatusReported}}
	secondContext := opscontext.Context{SchemaVersion: opscontext.SchemaVersion, CurrentUserReport: &opscontext.UserReport{Text: "显存 100%", Source: "chat.current_user", ObservedAt: "unknown", Status: opscontext.StatusReported}}

	_, err := first.DiagnoseWithContext(context.Background(), stubDescriber{resp: describe}, Owner{TurnID: "one"}, "uhost-abc", "同一个任务", firstContext, nil, nil)
	require.NoError(t, err)
	_, err = second.DiagnoseWithContext(context.Background(), stubDescriber{resp: describe}, Owner{TurnID: "two"}, "uhost-abc", "同一个任务", secondContext, nil, nil)
	require.NoError(t, err)
	require.Equal(t, firstAudit.Events[0].TaskHash, secondAudit.Events[0].TaskHash)
	require.Equal(t, hashTask("同一个任务"), firstAudit.Events[0].TaskHash)
}

func TestSummarizeAuditStepsUsesOnlyFixedClasses(t *testing.T) {
	ran, refused, first := summarizeAuditSteps([]Step{
		{Command: "uname -a", Disposition: "ran"},
		{Command: "systemctl restart vllm", Disposition: "refused"},
	})
	require.Equal(t, 1, ran)
	require.Equal(t, 1, refused)
	require.Equal(t, "environment_discovery", first)
}

func TestAuditCommandClassifiesFileInspection(t *testing.T) {
	for _, command := range []string{
		"ls -lah /workspace", "find /var/log -type f", "tail -n 50 /var/log/app.log",
		"head -n 1 /tmp/log", "cat /var/log/app.log", "grep -R error /var/log",
	} {
		require.Equal(t, "file_inspection", auditCommandClass(command), command)
	}
	require.Equal(t, "environment_discovery", auditCommandClass("cat /etc/os-release"))
}
