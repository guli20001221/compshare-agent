package capability

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

// TestLiveInstanceAccessSources is a read-only, env-gated conformance probe
// for the public APIs used by the instance-access capability. It never
// logs an IP address, login command, instance id, or credential.
func TestLiveInstanceAccessSources(t *testing.T) {
	if os.Getenv("RUN_ACCESS_DIAGNOSIS_PROBE") != "1" {
		t.Skip("set RUN_ACCESS_DIAGNOSIS_PROBE=1 and the ACCESS_PROBE_* variables to run")
	}

	configPath := strings.TrimSpace(os.Getenv("ACCESS_PROBE_CONFIG"))
	require.NotEmpty(t, configPath, "ACCESS_PROBE_CONFIG is required")
	topOrg := requiredProbeUint32(t, "ACCESS_PROBE_TOP_ORG")
	org := requiredProbeUint32(t, "ACCESS_PROBE_ORG")

	cfg, err := config.Load(configPath)
	require.NoError(t, err)
	projectID := strings.TrimSpace(os.Getenv("ACCESS_PROBE_PROJECT_ID"))
	if projectID == "" {
		projectID = cfg.Agent.ProjectId
	}
	roleUrn := cfg.Agent.STS.DefaultRoleUrn
	if roleUrn == "" {
		roleUrn, err = tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, topOrg)
		require.NoError(t, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx = tools.WithUser(ctx, tools.UserContext{
		TopOrganizationID: topOrg,
		OrganizationID:    org,
		CompanyID:         topOrg,
		RoleUrn:           roleUrn,
		SessionName:       cfg.Agent.STS.DefaultSessionName,
		ProjectId:         projectID,
		Region:            cfg.Agent.Region,
		UserEmail:         strings.TrimSpace(os.Getenv("ACCESS_PROBE_USER_EMAIL")),
	})

	safe := tools.NewSafeToolExecutor(tools.NewExternalExecutor(cfg.Agent))
	exec := safe.AsToolExecutor(tools.OriginDiagnosisInternal)

	ports, err := exec.Execute(ctx, "DescribeCompShareSoftwarePort", map[string]any{})
	require.NoError(t, err)
	portRows, _ := ports["SoftwarePort"].([]any)
	require.NotEmpty(t, portRows, "software-port catalog is unexpectedly empty")
	t.Logf("DescribeCompShareSoftwarePort: ok, catalog_entries=%d, has_jupyter=%t",
		len(portRows), accessProbeCatalogHasJupyter(portRows))

	instanceArgs := map[string]any{"Limit": 100}
	explicitID := strings.TrimSpace(os.Getenv("ACCESS_PROBE_INSTANCE_ID"))
	if explicitID != "" {
		instanceArgs = map[string]any{"UHostIds": []string{explicitID}}
	}
	instances, err := exec.Execute(ctx, "DescribeCompShareInstance", instanceArgs)
	require.NoError(t, err)
	hosts, _ := instances["UHostSet"].([]any)
	t.Logf("DescribeCompShareInstance response: keys=%v, total_count=%v, configured_region=%q, sts=%t",
		sortedProbeKeys(instances), instances["TotalCount"], cfg.Agent.Region, cfg.Agent.STS.ServiceAK != "")
	if len(hosts) == 0 {
		t.Log("DescribeCompShareInstance: ok, account currently has no instance to inspect")
		return
	}
	host, hostID, hasJupyter := selectAccessProbeHost(hosts)
	require.NotNil(t, host)
	require.NotEmpty(t, hostID)
	if explicitID != "" {
		require.Equal(t, explicitID, hostID, "point query must echo the requested instance id")
	}
	t.Logf("DescribeCompShareInstance: ok, instances=%d, selected_kind=%s, running=%t, jupyter_metadata=%t",
		len(hosts), accessProbeHostKind(hostID), strings.EqualFold(probeString(host["State"]), "Running"), hasJupyter)

	monitor, err := exec.Execute(ctx, "GetCompShareInstanceMonitor", map[string]any{"UHostIds": []string{hostID}})
	require.NoError(t, err)
	t.Logf("GetCompShareInstanceMonitor: ok, response_keys=%v", sortedProbeKeys(monitor))

	tokenResult, err := exec.Execute(ctx, "DescribeCompShareJupyterToken", map[string]any{"UHostIds": []string{hostID}})
	require.NoError(t, err)
	token := strings.TrimSpace(probeString(tokenResult["JupyterToken"]))
	require.NotEmpty(t, token, "Jupyter token API succeeded but returned no token")
	t.Logf("DescribeCompShareJupyterToken: ok, token_present=true, token_length=%d", len(token))

	accessResult := NewReadCapability(instanceAccessReadSpec()).Run(ctx, InstanceAccessRequest{
		Targets: accessTarget(hostID), AccessType: accessTypeJupyterToken,
	}, ReadRuntime{
		Executor: accessProbeReadExecutor{ToolExecutor: exec},
		Resolver: coldRegistrySnapshot(),
		Now:      time.Now(),
	})
	require.Equal(t, "handled", string(accessResult.Status))
	require.Contains(t, accessResult.Reply, token)
	require.NotNil(t, accessResult.Envelope)
	require.Contains(t, accessResult.Envelope.SourceActions, instanceAccessTokenAction)
	t.Log("ReadCapability_instance_access: explicit Jupyter token path ok")
}

type accessProbeReadExecutor struct {
	tools.ToolExecutor
}

func (e accessProbeReadExecutor) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func requiredProbeUint32(t *testing.T, name string) uint32 {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	require.NotEmpty(t, raw, "%s is required", name)
	value, err := strconv.ParseUint(raw, 10, 32)
	require.NoError(t, err, "%s must be uint32", name)
	require.NotZero(t, value, "%s must be non-zero", name)
	return uint32(value)
}

func selectAccessProbeHost(hosts []any) (map[string]any, string, bool) {
	var fallback map[string]any
	var fallbackID string
	for _, raw := range hosts {
		host, _ := raw.(map[string]any)
		id := strings.TrimSpace(probeString(host["UHostId"]))
		if id == "" {
			continue
		}
		if fallback == nil {
			fallback, fallbackID = host, id
		}
		if !strings.EqualFold(probeString(host["State"]), "Running") {
			continue
		}
		hasJupyter := accessProbeHostHasJupyter(host)
		if hasJupyter {
			return host, id, true
		}
		if fallback == nil || !strings.EqualFold(probeString(fallback["State"]), "Running") {
			fallback, fallbackID = host, id
		}
	}
	return fallback, fallbackID, accessProbeHostHasJupyter(fallback)
}

func accessProbeHostHasJupyter(host map[string]any) bool {
	if host == nil {
		return false
	}
	softwares, _ := host["Softwares"].([]any)
	for _, raw := range softwares {
		switch software := raw.(type) {
		case string:
			if strings.Contains(strings.ToLower(software), "jupyter") {
				return true
			}
		case map[string]any:
			for _, key := range []string{"Name", "Software", "SoftwareName"} {
				if strings.Contains(strings.ToLower(probeString(software[key])), "jupyter") {
					return true
				}
			}
		}
	}
	return false
}

func accessProbeCatalogHasJupyter(rows []any) bool {
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		for _, key := range []string{"Name", "Software", "SoftwareName"} {
			if strings.Contains(strings.ToLower(probeString(row[key])), "jupyter") {
				return true
			}
		}
	}
	return false
}

func accessProbeHostKind(id string) string {
	if strings.HasPrefix(strings.ToLower(id), "cpod-") {
		return "pod"
	}
	return "vm"
}

func probeString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func sortedProbeKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
