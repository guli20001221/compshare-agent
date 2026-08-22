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

func TestZoneCatalogReadSurfacesLiveImageRestrictions(t *testing.T) {
	reg := NewReadCapability(zoneCatalogReadSpec())
	req, err := reg.Decode(map[string]any{})
	require.NoError(t, err)
	result := reg.Run(context.Background(), req, ReadRuntime{ZoneCatalog: testZoneCatalog(
		deployment.ZoneCatalogEntry{
			Placement:   deployment.ZonePlacement{Zone: "cn-test-01", Region: "cn-test", ZoneID: 42},
			DisplayName: "测试区", DisableImageSync: true, UnsupportedImageTypes: []string{"Community", "Custom"},
		},
	)})
	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Contains(t, result.Reply, "容器自制镜像同步=禁用")
	require.Contains(t, result.Reply, "不支持镜像类型=Community,Custom")
}

func TestZoneCatalogReadRejectsFiltersAndAlwaysReturnsTheCompleteDirectory(t *testing.T) {
	reg := NewReadCapability(zoneCatalogReadSpec())
	_, err := reg.Decode(map[string]any{"query": "上海二"})
	require.Error(t, err, "the model-facing catalog must not regain a local filter")

	req, err := reg.Decode(map[string]any{})
	require.NoError(t, err)
	result := reg.Run(context.Background(), req, ReadRuntime{ZoneCatalog: testZoneCatalog(
		testZoneEntry("cn-sh2-01", "上海二A", "cn-sh2", 5001, 3003, true),
		testZoneEntry("cn-sh2-02", "上海二B", "cn-sh2", 10027, 3007, false),
	)})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, result.Envelope.Subjects, 2)
	require.ElementsMatch(t, []string{"cn-sh2-01", "cn-sh2-02"}, []string{
		result.Envelope.Subjects[0].ID,
		result.Envelope.Subjects[1].ID,
	})
	require.NotContains(t, result.Reply, "没有找到")
}

func TestZoneCatalogReadKeepsEmptyAndUnavailableDistinct(t *testing.T) {
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
}
