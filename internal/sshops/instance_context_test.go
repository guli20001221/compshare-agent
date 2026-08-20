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
	catalog    map[string]any
	catalogErr error
	calls      []string
}

func (d *contextDescriber) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	d.calls = append(d.calls, action)
	switch action {
	case "DescribeCompShareInstance":
		return d.describe, nil
	case "GetCompShareInstanceMonitor":
		return d.monitor, d.monitorErr
	case "DescribeCompShareSoftwarePort":
		return d.catalog, d.catalogErr
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
			// Each entry's URL carries a live access token. Only Name may cross into the context.
			"Softwares": []any{
				map[string]any{"Name": "ComfyUI", "URL": "http://198.51.100.9:8188/?token=comfy-token-value"},
				map[string]any{"Name": "JupyterLab", "URL": "http://198.51.100.9:8888/?token=jupyter-url-token"},
			},
		}}},
		catalog: map[string]any{"SoftwarePort": []any{
			map[string]any{"Software": "ComfyUI", "Port": float64(8188)},
			map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
			// Not declared by this instance: the catalog is region-wide and must be correlated down.
			map[string]any{"Software": "FileBrowser", "Port": float64(8080)},
		}},
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
	require.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor", "DescribeCompShareSoftwarePort"}, describer.calls)
	require.Equal(t, opscontext.SchemaVersion, runner.lastContext.SchemaVersion)
	require.NotEmpty(t, runner.lastContext.PlatformFacts)
	require.Len(t, runner.lastContext.EndpointTargets, 3)
	require.Equal(t, "platform-http-1", runner.lastContext.EndpointTargets[0].ID)
	require.Equal(t, "http", runner.lastContext.EndpointTargets[0].Kind)
	require.Contains(t, runner.lastContext.EndpointTargets[0].URL, "comfy-token-value")
	require.Equal(t, "tcp", runner.lastContext.EndpointTargets[2].Kind)
	require.Equal(t, 30188, runner.lastContext.EndpointTargets[2].Port)
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
		// Softwares[].URL: the name beside it is wanted, the token in it never is.
		"comfy-token-value", "jupyter-url-token", "token=", "URL",
	} {
		require.NotContains(t, text, forbidden)
	}
	require.Contains(t, text, "8188")
	// v2 states the port-shaped claims separately, and the merged v1 key is gone rather than aliased.
	require.Contains(t, text, "platform.instance_port_hints")
	require.Contains(t, text, "platform.tcp_forwards")
	require.Contains(t, text, "instance.declared_software")
	require.Contains(t, text, "catalog.expected_software_ports")
	require.NotContains(t, text, "instance.reported_ports")
	require.NotContains(t, text, "configured_ports")
	// The region-wide catalog is correlated down to what this instance declares: FileBrowser is in
	// the catalog and not on this box, so its port must not arrive as an expectation for this box.
	require.NotContains(t, text, "FileBrowser")
	require.NotContains(t, text, "8080")
	require.Contains(t, text, "not_observed")
	require.Contains(t, text, "monitor.gpu_usage")

	begin, done := audit.Events[0], audit.Events[1]
	require.Equal(t, opscontext.SchemaVersion, begin.ContextSchemaVersion)
	require.NotZero(t, begin.ContextFactCoverage&opscontext.CoveragePortHints)
	require.NotZero(t, begin.ContextFactCoverage&opscontext.CoverageTCPForwards)
	require.NotZero(t, begin.ContextFactCoverage&opscontext.CoverageSoftware)
	require.NotZero(t, begin.ContextFactCoverage&opscontext.CoverageCatalogPorts)
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
		// Both enrichment endpoints down at once: neither is allowed to turn a consented,
		// already-authorized SSH diagnosis into a failure, and each has to say so as its own fact.
		catalogErr: errors.New("software port catalog unavailable"),
	}
	runner := &fakeRunner{res: Result{Output: "SSH diagnosis still ran"}}
	svc := NewService(runner, &MemAuditWriter{})

	_, err := svc.DiagnoseWithContext(context.Background(), describer, Owner{TurnID: "turn"}, "uhost-abc", "task",
		opscontext.Context{SchemaVersion: opscontext.SchemaVersion}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, runner.calls)
	require.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor", "DescribeCompShareSoftwarePort"}, describer.calls)
	unknown := map[string]bool{}
	for _, fact := range runner.lastContext.PlatformFacts {
		if fact.Status == opscontext.StatusUnknown {
			unknown[fact.Key] = true
		}
	}
	require.True(t, unknown["monitor"])
	// The region key, not the expected-ports one: this fixture declares no Softwares, and the key is
	// chosen from what is knowable before the call, so a failed call reports under the same name a
	// successful one would have used.
	require.True(t, unknown["catalog.region_port_hints"])
	_, claimed := unknown["catalog.expected_software_ports"]
	require.False(t, claimed)
}

// TestInstanceContextCatalogIsNeverPresentedAsGuestState pins the distinction the whole v2 split
// exists for. The catalog answers "what port should this software use"; only SSH answers "what is
// listening". A catalog entry that arrived as StatusKnown would read as the second, and the model's
// stated rule is to trust `known` facts without re-checking them.
func TestInstanceContextCatalogIsNeverPresentedAsGuestState(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	describe := describeResp("ssh root@10.0.0.9", b64)
	host := describe["UHostSet"].([]any)[0].(map[string]any)
	host["Softwares"] = []any{map[string]any{"Name": "ComfyUI", "URL": "http://198.51.100.9:8188/?token=t"}}
	describer := &contextDescriber{
		describe: describe,
		catalog:  map[string]any{"SoftwarePort": []any{map[string]any{"Software": "ComfyUI", "Port": float64(8188)}}},
	}
	runner := &fakeRunner{res: Result{Output: "done"}}
	svc := NewService(runner, &MemAuditWriter{})

	_, err := svc.DiagnoseWithContext(context.Background(), describer, Owner{TurnID: "turn"}, "uhost-abc", "task",
		opscontext.Context{SchemaVersion: opscontext.SchemaVersion}, nil, nil)
	require.NoError(t, err)

	byKey := map[string]opscontext.Fact{}
	for _, fact := range runner.lastContext.PlatformFacts {
		byKey[fact.Key] = fact
	}
	catalog, ok := byKey["catalog.expected_software_ports"]
	require.True(t, ok)
	require.Equal(t, opscontext.StatusReported, catalog.Status)
	require.NotEqual(t, opscontext.StatusKnown, catalog.Status)
	require.Equal(t, "DescribeCompShareSoftwarePort", catalog.Source)
	// ...and the guest-side fact it must not be confused with still says nothing was observed.
	require.Equal(t, opscontext.StatusNotObserved, byKey["guest.listeners"].Status)
	require.Equal(t, "ssh", byKey["guest.listeners"].Source)
}

// TestUncorrelatedCatalogShipsUnderTheRegionKey covers the case the endpoint forces on us: it takes
// no instance argument, so its answer is region-wide, and correlating it to Softwares[].Name is the
// ONLY thing that makes it about this instance. With no declared software there is no correlation,
// and publishing that list as "this instance's expected ports" would assert a relevance the value
// does not have — another image's FileBrowser port arriving as this box's expected port is a
// diagnosis sent after a service that was never installed.
func TestUncorrelatedCatalogShipsUnderTheRegionKey(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	describe := describeResp("ssh root@10.0.0.9", b64)
	host := describe["UHostSet"].([]any)[0].(map[string]any)
	delete(host, "Softwares") // nothing to correlate against
	describer := &contextDescriber{
		describe: describe,
		catalog: map[string]any{"SoftwarePort": []any{
			map[string]any{"Software": "FileBrowser", "Port": float64(8080)},
			map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
		}},
	}
	runner := &fakeRunner{res: Result{Output: "done"}}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)

	_, err := svc.DiagnoseWithContext(context.Background(), describer, Owner{TurnID: "turn"}, "uhost-abc", "task",
		opscontext.Context{SchemaVersion: opscontext.SchemaVersion}, nil, nil)
	require.NoError(t, err)

	byKey := map[string]opscontext.Fact{}
	for _, fact := range runner.lastContext.PlatformFacts {
		byKey[fact.Key] = fact
	}
	hints, ok := byKey["catalog.region_port_hints"]
	require.True(t, ok, "uncorrelated catalog must still be sent, under the region key")
	require.Equal(t, opscontext.StatusReported, hints.Status)
	require.Len(t, hints.Value, 2)
	_, claimed := byKey["catalog.expected_software_ports"]
	require.False(t, claimed, "an uncorrelated list must never claim to be this instance's expected ports")
	// The declared-software fact is honest about why: unknown, not an empty "declares nothing".
	require.Equal(t, opscontext.StatusUnknown, byKey["instance.declared_software"].Status)
	// Separate coverage bits, so the audit can tell a correlated run from an uncorrelated one.
	require.NotZero(t, audit.Events[0].ContextFactCoverage&opscontext.CoverageRegionPortHints)
	require.Zero(t, audit.Events[0].ContextFactCoverage&opscontext.CoverageCatalogPorts)
	require.Zero(t, audit.Events[0].ContextFactCoverage&opscontext.CoverageSoftware)
}

// TestInstanceContextNeverCarriesEndpointsOrCredentials is the boundary test for the whole
// projection: it feeds the fields that actually appear on a real Describe response and asserts the
// serialized context contains none of them. Softwares[].URL is the newest of these and the reason
// v2 touches this file at all — the name beside it is a useful fact, the live token in it is not.
func TestInstanceContextNeverCarriesEndpointsOrCredentials(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	describer := &contextDescriber{
		describe: map[string]any{"UHostSet": []any{map[string]any{
			"UHostId":             "uhost-abc",
			"State":               "Running",
			"SshLoginCommand":     "ssh -p 23 root@198.51.100.9",
			"Password":            b64,
			"IPSet":               []any{map[string]any{"IP": "198.51.100.9", "Type": "International"}},
			"PrivateIP":           "10.64.46.5",
			"JupyterToken":        "jupyter-secret-value",
			"FileBrowserPassword": "filebrowser-secret-value",
			"Softwares": []any{map[string]any{
				"Name": "JupyterLab",
				"URL":  "http://198.51.100.9:8888/lab?token=live-jupyter-token",
			}},
		}}},
	}
	runner := &fakeRunner{res: Result{Output: "done"}}
	svc := NewService(runner, &MemAuditWriter{})

	_, err := svc.DiagnoseWithContext(context.Background(), describer, Owner{TurnID: "turn"}, "uhost-abc", "task",
		opscontext.Context{SchemaVersion: opscontext.SchemaVersion}, nil, nil)
	require.NoError(t, err)
	encoded, err := json.Marshal(runner.lastContext)
	require.NoError(t, err)
	text := string(encoded)

	for _, forbidden := range []string{
		"ssh -p 23", "root@", secretPW, b64,
		"198.51.100.9", "10.64.46.5",
		"live-jupyter-token", "token=", "lab?token", "http://",
		"jupyter-secret-value", "filebrowser-secret-value",
		"SshLoginCommand", "Password", "IPSet", "PrivateIP", "JupyterToken", "FileBrowserPassword", "URL",
	} {
		require.NotContainsf(t, text, forbidden, "context leaked %q", forbidden)
	}
	// The projection is not vacuous: the one allowlisted field of that same object did come through.
	require.Contains(t, text, "JupyterLab")
	require.Contains(t, text, "instance.declared_software")
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
