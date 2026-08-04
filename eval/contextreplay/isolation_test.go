package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/config"
)

func baselinePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "conf", "config.local.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("deploy baseline not present (%v)", err)
	}
	return path
}

// TestReplayIsolationStripsEveryProductionReach is the guard the replay harness
// is not allowed to run without.
//
// It is written to fail when isolation is INCOMPLETE, not to confirm that
// someone looked. A read-only probe in this repo once created three billed 4090
// instances because three guard flags were all silently false and every one of
// them had been eyeballed.
//
// The premise assertions matter as much as the conclusions: if the baseline ever
// stops carrying live production values, this test would keep passing while
// asserting nothing, so it first proves the baseline HAS the reach it then
// proves was stripped.
func TestReplayIsolationStripsEveryProductionReach(t *testing.T) {
	path := baselinePath(t)

	raw, err := config.Load(path)
	require.NoError(t, err, "the baseline must load, or the strip is untested rather than unnecessary")

	// PREMISE: the baseline really is dangerous. Without this, a future baseline
	// that happens to be empty would make every assertion below vacuous.
	dangerous := 0
	for _, reach := range productionReaches() {
		if strings.TrimSpace(reach.Read(raw)) != "" {
			dangerous++
		}
	}
	require.GreaterOrEqual(t, dangerous, 4,
		"premise: the deploy baseline is supposed to carry live production reach (DSN, STS, keys, "+
			"mutating_tools). Only %d found — either the baseline changed shape or this table is "+
			"reading the wrong fields, and in both cases the strip below proves nothing", dangerous)

	isolated, stripped, err := LoadIsolatedReplayConfig(path)
	require.NoError(t, err)
	assert.NotEmpty(t, stripped, "the strip must report what it removed")

	for _, reach := range productionReaches() {
		assert.Empty(t, strings.TrimSpace(reach.Read(isolated)),
			"%s survived isolation — the replay could reach production through it", reach.Name)
	}

	// The one thing it must KEEP, or the harness cannot run at all.
	assert.NotEmpty(t, isolated.Agent.LLM.APIKey, "the model credential is the only thing inherited on purpose")
}

// TestIsolationTableCoversTheBaselineSecrets is the half that catches a reach
// nobody added to the table.
//
// productionReaches() is hand-maintained, so on its own it can only strip what
// someone remembered. This scans the raw baseline text for the shapes of live
// credentials and requires each match to be attributable to a field the table
// clears — so adding a new secret-bearing key to the deploy config fails here
// instead of silently riding into a replay run.
func TestIsolationTableCoversTheBaselineSecrets(t *testing.T) {
	path := baselinePath(t)
	body, err := os.ReadFile(path)
	require.NoError(t, err)

	// Keys the table clears, plus the ones deliberately inherited or inert.
	covered := map[string]string{
		"dsn":               "agent.mysql.dsn",
		"service_ak":        "agent.sts.service_ak",
		"service_sk":        "agent.sts.service_sk",
		"public_key":        "agent.public_key",
		"private_key":       "agent.private_key",
		"app_secret":        "agent.feishu.app_secret",
		"mcp_bearer_token":  "agent.retrieval.mcp_bearer_token",
		"api_key":           "INHERITED ON PURPOSE (model credential)",
		"app_id":            "cleared with agent.feishu",
		// Not a credential (a synthetic bot address), but it is long enough to
		// trip the scanner and it IS cleared by the agent.feishu reset. Recorded
		// rather than pattern-excluded: this scanner earns its keep by being
		// noisy about anything the table has not accounted for.
		"user_email": "cleared with agent.feishu",
		"default_role_urn":  "cleared with agent.sts",
		"role_urn_template": "cleared with agent.sts",
	}

	// A value long enough to be a credential rather than a setting.
	secretish := regexp.MustCompile(`(?m)^\s*([a-z_]+)\s*:\s*"?([^"\s#]{24,})"?\s*$`)
	var uncovered []string
	for _, match := range secretish.FindAllStringSubmatch(string(body), -1) {
		key := match[1]
		if _, ok := covered[key]; ok {
			continue
		}
		// URLs and paths are settings, not credentials.
		value := match[2]
		if strings.HasPrefix(value, "http") || strings.ContainsAny(value, "/\\") {
			continue
		}
		uncovered = append(uncovered, key+": "+value[:8]+"...")
	}

	assert.Empty(t, uncovered,
		"the deploy baseline gained credential-shaped keys that isolation does not clear. Add them to "+
			"productionReaches() (and to the covered map here) before running a replay: %v", uncovered)
}
