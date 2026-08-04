package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// ShippedMaxTokensPerTurn is a mirror, and a mirror nobody checks is just a second
// copy. This is the check.
//
// It reads the yaml directly rather than through Load: Load substitutes ${ENV_VAR}
// and validates, so it would need the deployment's secrets present to answer a
// question about one integer. Parsing the file is the narrower thing to do and it
// cannot pass for the wrong reason — a missing key unmarshals to 0, which fails.
// baselineConfigCandidates are the names the baseline config file goes by. The
// deploy repo split config.yaml into config.local.yaml (the baseline, holding
// everything including agent.features and agent.rate_limit) plus a thin
// config.prod.yaml that `extends` it with production network overrides; github
// still carries the pre-split config.yaml. Both layouts are live right now, so
// this resolves by name rather than assuming one — and fails loudly if NEITHER
// exists, which is the case that would otherwise turn this test into a no-op.
var baselineConfigCandidates = []string{
	"../../deploy/conf/config.local.yaml",
	"../../deploy/conf/config.yaml",
}

func readBaselineConfig(t *testing.T) []byte {
	t.Helper()
	for _, path := range baselineConfigCandidates {
		if raw, err := os.ReadFile(path); err == nil {
			t.Logf("baseline config: %s", path)
			return raw
		}
	}
	t.Fatalf("no baseline config found at any of %v — the file was renamed again and this "+
		"test would otherwise silently stop checking anything", baselineConfigCandidates)
	return nil
}

func TestShippedConfigMatchesTheTokenCapConstant(t *testing.T) {
	raw := readBaselineConfig(t)

	var shipped struct {
		Agent struct {
			RateLimit struct {
				MaxTokensPerTurn int `yaml:"max_tokens_per_turn"`
			} `yaml:"rate_limit"`
		} `yaml:"agent"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &shipped))

	require.NotZero(t, shipped.Agent.RateLimit.MaxTokensPerTurn,
		"premise: the shipped config sets a cap. A zero here would make the equality "+
			"below assert nothing if the constant were ever zeroed too")

	require.Equal(t, ShippedMaxTokensPerTurn, shipped.Agent.RateLimit.MaxTokensPerTurn,
		"the baseline deploy config raised the per-turn token cap without updating "+
			"ShippedMaxTokensPerTurn. Everything that sizes history against the cap "+
			"(maxReplayedHistoryRunes, the history ceiling) derives from this number, "+
			"so a silent divergence re-creates the two-producer drift the constant "+
			"was added to end")
}
