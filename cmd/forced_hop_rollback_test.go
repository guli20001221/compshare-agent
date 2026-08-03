package main

import (
	"path/filepath"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The forced knowledge hop is off, and the claim attached to that decision — in
// deploy/conf/config.prod.yaml, in CLAUDE.md, and in the commit that made it — is
// that COMPSHARE_FORCED_KNOWLEDGE_HOP=1 rolls it back without a deploy.
//
// That claim is a property of the SHIPPED FILE, not of the code: putBoolEnv
// records a non-nil *bool unconditionally and RuntimeGetenv consults the overlay
// before os.Getenv, so writing `forced_knowledge_hop: false` would out-rank the
// environment and the documented rollback would silently do nothing. The key is
// therefore omitted rather than set false. This test reads the real deploy
// config so the promise cannot drift away from the file that has to keep it.
func TestDeployConfig_ForcedHopIsOffButEnvCanStillRollItBack(t *testing.T) {
	path := filepath.Join("..", "deploy", "conf", "config.prod.yaml")
	require.FileExists(t, path)
	cfg, err := config.Load(path)
	require.NoError(t, err, "the shipped deploy config must parse")

	require.Nil(t, cfg.Agent.Features.ForcedKnowledgeHop,
		"deploy/conf/config.prod.yaml must OMIT forced_knowledge_hop; an explicit false out-ranks COMPSHARE_FORCED_KNOWLEDGE_HOP and breaks the documented rollback")

	t.Run("shipped default is off", func(t *testing.T) {
		enabled, unknown := forcedKnowledgeHopEnabledFromEnv(cfg.RuntimeGetenv(func(string) string { return "" }))
		assert.False(t, enabled, "with the key omitted and the env unset, the hop must be off")
		assert.Empty(t, unknown)
	})

	t.Run("env var rolls it back", func(t *testing.T) {
		base := func(key string) string {
			if key == "COMPSHARE_FORCED_KNOWLEDGE_HOP" {
				return "1"
			}
			return ""
		}
		enabled, unknown := forcedKnowledgeHopEnabledFromEnv(cfg.RuntimeGetenv(base))
		assert.True(t, enabled,
			"COMPSHARE_FORCED_KNOWLEDGE_HOP=1 must re-enable the hop against the real deploy config — this is the advertised no-deploy rollback")
		assert.Empty(t, unknown)
	})
}
