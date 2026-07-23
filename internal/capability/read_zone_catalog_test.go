package capability

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/require"
)

func testZoneCatalog(entries ...deployment.ZoneCatalogEntry) *deployment.ZoneCatalogSnapshot {
	return deployment.NewZoneCatalogSnapshot(true, entries)
}

func testZoneEntry(zone, display, region string, id, az uint32, pod bool) deployment.ZoneCatalogEntry {
	return deployment.ZoneCatalogEntry{
		Placement:   deployment.ZonePlacement{Zone: zone, Region: region, ZoneID: id, AzGroup: az, IsPod: pod},
		DisplayName: display,
	}
}

func TestZoneCatalogReadListsStructuredLiveFacts(t *testing.T) {
	reg := NewReadCapability(zoneCatalogReadSpec())
	req, err := reg.Decode(map[string]any{})
	require.NoError(t, err)
	result := reg.Run(context.Background(), req, ReadRuntime{ZoneCatalog: testZoneCatalog(
		testZoneEntry("cn-wlcb-01", "华北二A", "cn-wlcb", 10027, 3007, false),
		testZoneEntry("cn-bj2-03", "华北一C", "cn-bj2", 5001, 3003, true),
	)})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Contains(t, result.Reply, "华北一C：ZoneID=cn-bj2-03，Region=cn-bj2，环境=容器区")
	require.NotNil(t, result.Envelope)
	require.Len(t, result.Envelope.Subjects, 2)
	require.Equal(t, zoneCatalogAction, result.ToolAction)
}

func TestZoneCatalogReadExactDisplayNameAllowsFormattingSpaces(t *testing.T) {
	reg := NewReadCapability(zoneCatalogReadSpec())
	req, err := reg.Decode(map[string]any{"query": "华北一 C"})
	require.NoError(t, err)
	result := reg.Run(context.Background(), req, ReadRuntime{ZoneCatalog: testZoneCatalog(
		testZoneEntry("cn-bj2-03", "华北一C", "cn-bj2", 5001, 3003, true),
		testZoneEntry("cn-wlcb-01", "华北二A", "cn-wlcb", 10027, 3007, false),
	)})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, result.Envelope.Subjects, 1)
	require.Equal(t, "cn-bj2-03", result.Envelope.Subjects[0].ID)
}

func TestZoneCatalogReadKeepsEmptyUnavailableAndConflictDistinct(t *testing.T) {
	reg := NewReadCapability(zoneCatalogReadSpec())

	t.Run("successful empty", func(t *testing.T) {
		req, err := reg.Decode(map[string]any{})
		require.NoError(t, err)
		result := reg.Run(context.Background(), req, ReadRuntime{ZoneCatalog: testZoneCatalog()})
		require.Equal(t, platform.ReadStatusEmpty, result.Status)
	})

	t.Run("unavailable", func(t *testing.T) {
		req, err := reg.Decode(map[string]any{})
		require.NoError(t, err)
		result := reg.Run(context.Background(), req, ReadRuntime{ZoneCatalog: deployment.NewZoneCatalogSnapshot(false, nil)})
		require.Equal(t, platform.ReadStatusUnavailable, result.Status)
	})

	t.Run("ambiguous display name", func(t *testing.T) {
		req, err := reg.Decode(map[string]any{"query": "重复名称"})
		require.NoError(t, err)
		result := reg.Run(context.Background(), req, ReadRuntime{ZoneCatalog: testZoneCatalog(
			testZoneEntry("zone-a", "重复名称", "region-a", 1, 1, false),
			testZoneEntry("zone-b", "重复名称", "region-b", 2, 2, true),
		)})
		require.Equal(t, platform.ReadStatusConflict, result.Status)
		require.True(t, result.NeedsClarification)
	})
}
