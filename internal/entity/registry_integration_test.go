package entity

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestIntegrationResolveCurrentAccountInstances(t *testing.T) {
	if os.Getenv("RUN_ENTITY_INTEGRATION") != "1" {
		t.Skip("set RUN_ENTITY_INTEGRATION=1 to hit the real CompShare API")
	}
	publicKey := os.Getenv("COMPSHARE_PUBLIC_KEY")
	privateKey := os.Getenv("COMPSHARE_PRIVATE_KEY")
	if publicKey == "" || privateKey == "" {
		t.Skip("set COMPSHARE_PUBLIC_KEY and COMPSHARE_PRIVATE_KEY")
	}
	region := os.Getenv("COMPSHARE_REGION")
	if region == "" {
		region = "cn-wlcb"
	}

	exec := tools.NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: "https://api.compshare.cn/",
		PublicKey:       publicKey,
		PrivateKey:      privateKey,
		Region:          region,
		ProjectId:       os.Getenv("COMPSHARE_PROJECT_ID"),
	})
	reg := NewRegistry()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	require.NoError(t, reg.Sync(ctx, exec))
	require.NotEmpty(t, reg.Instances, "real account should return current instances for this gated probe")

	for id := range reg.Instances {
		got, res := reg.ResolveByID(id)
		require.Equal(t, ResolveHit, res.Status, "id %s", id)
		require.NotNil(t, got)
	}
}
