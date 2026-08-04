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
func TestShippedConfigMatchesTheTokenCapConstant(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/conf/config.yaml")
	require.NoError(t, err, "the shipped config is tracked; this test reads it, not a fixture")

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
		"deploy/conf/config.yaml raised the per-turn token cap without updating "+
			"ShippedMaxTokensPerTurn. Everything that sizes history against the cap "+
			"(maxReplayedHistoryRunes, the history ceiling) derives from this number, "+
			"so a silent divergence re-creates the two-producer drift the constant "+
			"was added to end")
}
