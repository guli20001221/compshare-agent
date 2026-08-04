package replayiso

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/config"
)

var testTenant = Tenant{TopOrganizationID: 1, OrganizationID: 2, ProjectID: "proj-test"}

func baselinePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "conf", "config.local.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("deploy baseline not present (%v)", err)
	}
	return path
}

// TestTenantIsMandatory pins the isolation dimension. A replay runs production's
// tool surface, so the account it may touch is not something to infer from
// whatever config happens to be on disk.
func TestTenantIsMandatory(t *testing.T) {
	path := baselinePath(t)

	for _, missing := range []Tenant{
		{},
		{TopOrganizationID: 1},
		{OrganizationID: 2},
	} {
		_, err := LoadIsolatedReplayConfig(path, missing, "")
		assert.Error(t, err, "an unnamed tenant must be refused, not defaulted: %+v", missing)
	}

	isolated, err := LoadIsolatedReplayConfig(path, testTenant, "")
	require.NoError(t, err)
	assert.Equal(t, uint32(1), isolated.Tenant.TopOrganizationID)
	assert.Equal(t, "proj-test", isolated.Config.Agent.ProjectId,
		"the tenant's project must override whatever the baseline carried")
}

// TestIsolationKeepsWhatMakesACallReal is the regression for the mistake this
// package was rewritten to fix.
//
// The first version stripped the STS service key and the platform AK/SK, on the
// theory that removing credentials removes risk. It removed no risk — which
// tenant a service key reaches is decided per request by the organization IDs,
// not by the key — and it broke the measurement outright: with no credentials
// every platform read failed and the agent narrated the auth failure as a
// product outcome ("实例状态查询暂时失败"), in 12 of 12 tool-calling turns of the
// first smoke run. A replay measuring an outage is not measuring the flag.
func TestIsolationKeepsWhatMakesACallReal(t *testing.T) {
	path := baselinePath(t)

	raw, err := config.Load(path)
	require.NoError(t, err)
	require.NotEmpty(t, strings.TrimSpace(raw.Agent.STS.ServiceAK),
		"premise: the baseline is supposed to carry STS service credentials; without that this test "+
			"asserts nothing about them being preserved")

	isolated, err := LoadIsolatedReplayConfig(path, testTenant, "")
	require.NoError(t, err)

	assert.Equal(t, raw.Agent.STS.ServiceAK, isolated.Config.Agent.STS.ServiceAK,
		"STS must survive isolation, or every platform read fails and the replay measures an outage")
	assert.Equal(t, raw.Agent.STS.ServiceSK, isolated.Config.Agent.STS.ServiceSK)
	assert.Equal(t, raw.Agent.LLM.APIKey, isolated.Config.Agent.LLM.APIKey,
		"the model credential is what the replay runs on")
}

// TestIsolationRefusesAConfigThatCannotMakeRealCalls closes the loop the other
// way: rather than trusting that nobody strips STS again, loading a baseline
// without it must fail loudly instead of yielding a config that produces
// plausible-looking replies built on failed tool calls.
func TestIsolationRefusesAConfigThatCannotMakeRealCalls(t *testing.T) {
	path := baselinePath(t)
	body, err := os.ReadFile(path)
	require.NoError(t, err)

	stripped := strings.ReplaceAll(string(body), "service_ak:", "service_ak_disabled:")
	require.NotEqual(t, string(body), stripped, "premise: the baseline must actually contain service_ak")

	tmp := filepath.Join(t.TempDir(), "no_sts.yaml")
	require.NoError(t, os.WriteFile(tmp, []byte(stripped), 0o600))

	_, err = LoadIsolatedReplayConfig(tmp, testTenant, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "STS service credentials",
		"the failure must name the cause; a silent auth failure reads as a model failure")
}

// TestIsolationStripsWhatWouldOutliveTheRun covers the things a replay is not
// allowed to touch, and asserts the baseline really carries them first so a
// future empty baseline cannot make this vacuous.
func TestIsolationStripsWhatWouldOutliveTheRun(t *testing.T) {
	path := baselinePath(t)

	raw, err := config.Load(path)
	require.NoError(t, err)
	present := 0
	for _, reach := range productionReaches() {
		if strings.TrimSpace(reach.Read(raw)) != "" {
			present++
		}
	}
	require.GreaterOrEqual(t, present, 2,
		"premise: the baseline is supposed to carry a live DSN, a Feishu secret and an mcp_url. "+
			"Only %d found — the strip below would prove nothing", present)

	isolated, err := LoadIsolatedReplayConfig(path, testTenant, "")
	require.NoError(t, err)
	for _, reach := range productionReaches() {
		assert.Empty(t, strings.TrimSpace(reach.Read(isolated.Config)),
			"%s survived isolation", reach.Name)
	}
	assert.NotEmpty(t, isolated.Stripped, "the strip must report what it removed")
}

// TestInheritedFlagsReportProductionValues pins that the harness can state the
// runtime it measured. mutating_tools in particular changes the SYSTEM PROMPT
// (segment_readonly.go drops the read-only boundary when writes are enabled), so
// a replay that quietly diverges on it is not measuring production even on turns
// that never call a write tool.
func TestInheritedFlagsReportProductionValues(t *testing.T) {
	path := baselinePath(t)
	isolated, err := LoadIsolatedReplayConfig(path, testTenant, "")
	require.NoError(t, err)

	for _, flag := range InheritedFlags() {
		_, ok := isolated.Inherited[flag.Name]
		assert.True(t, ok, "%s must be reported, not assumed", flag.Name)
	}
	assert.True(t, isolated.Inherited["features.mutating_tools"],
		"the deploy baseline ships writes ON; if that changes, the replay's prompt-fidelity claim "+
			"changes with it and this test is where you find out")
}

// TestReplayDSNMustNotBeTheProductionOne pins the one thing that makes running
// the ssh-ops lane at its production setting safe. The lane needs a database for
// its fail-closed audit, so the replay supplies one — and "I passed the local
// one" is exactly the class of claim that was false three times over when a
// read-only probe created three billed instances.
func TestReplayDSNMustNotBeTheProductionOne(t *testing.T) {
	path := baselinePath(t)

	raw, err := config.Load(path)
	require.NoError(t, err)
	production := strings.TrimSpace(raw.Agent.MySQL.DSN)
	require.NotEmpty(t, production, "premise: the baseline must carry a production DSN to guard against")

	_, err = LoadIsolatedReplayConfig(path, testTenant, production)
	require.Error(t, err, "handing the replay the production DSN must be refused")
	assert.Contains(t, err.Error(), "production DSN")

	local := "postgresql://postgres@127.0.0.1:15432/compshare_agent?sslmode=disable"
	require.NotEqual(t, production, local, "premise: the local DSN must actually differ")
	isolated, err := LoadIsolatedReplayConfig(path, testTenant, local)
	require.NoError(t, err)
	assert.Equal(t, local, isolated.Config.Agent.MySQL.DSN,
		"a distinct DSN is accepted, so the ssh-ops audit can run at its production setting")
}
