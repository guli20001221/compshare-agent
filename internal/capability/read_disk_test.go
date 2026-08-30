package capability

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func diskFixture() map[string]any {
	return map[string]any{"TotalCount": float64(2), "DiskSet": []any{
		map[string]any{
			"Name": "training-data", "ResourceId": "udisk-1", "Configuration": "100GB", "DiskType": "CLOUD_SSD",
			"Zone": "cn-wlcb-01", "MountInstance": "uhost-1", "MountPoint": "/dev/vdb", "Status": "InUse",
			"ChargeType": "Postpay", "Source": "UDisk", "CreateTime": float64(1770000000), "ExpiredTime": float64(1771000000),
		},
		map[string]any{
			"Name": "volume-2", "ResourceId": "volume-2", "Configuration": "40GB", "DiskType": "CVolume",
			"Zone": "cn-bj2-03", "MountInstance": "cpod-2", "MountPoint": "/", "Status": "InUse",
			"ChargeType": "Month", "Source": "CVolume",
		},
	}}
}

func TestDiskInfoByInstanceUsesResolvedHostAndProjectsFacts(t *testing.T) {
	exec := &fakeReadExec{result: diskFixture()}
	reg := NewReadCapability(resourceReadSpec())
	result := reg.Run(context.Background(), ResourceInfoRequest{ResourceType: resourceTypeDisks, Targets: []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: "uhost-1", Source: platform.SourceUserText,
	}}}, ReadRuntime{Executor: exec, Resolver: entity.RegistrySnapshot{}})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, diskInfoAction, exec.calls[0].action)
	assert.Equal(t, "uhost-1", exec.calls[0].args["HostId"])
	assert.Contains(t, result.Reply, "training-data（udisk-1）")
	assert.Contains(t, result.Reply, "容量 100GB")
	assert.Contains(t, result.Reply, "挂载点 /dev/vdb")
	assert.Contains(t, result.Reply, "来源 UDisk")
	assert.Contains(t, result.Reply, "可用区 cn-wlcb-01")
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindDiskInfo, result.Envelope.Kind)
	assert.Equal(t, envelope.SubjectDisk, result.Envelope.Subjects[0].Type)
	assertDiskFact(t, result.Envelope, "udisk-1", "configuration", "100GB")
	assertDiskFact(t, result.Envelope, "udisk-1", "mount_instance", "uhost-1")
	assertDiskFact(t, result.Envelope, "udisk-1", "source", "UDisk")
	assertDiskFact(t, result.Envelope, "udisk-1", "zone", "cn-wlcb-01")
	assertDiskFact(t, result.Envelope, "udisk-1", "expired_time", "2026-02-14 00:26")
}

func TestDiskInfoByResourceIDFiltersTheUnifiedResponse(t *testing.T) {
	exec := &fakeReadExec{result: diskFixture()}
	reg := NewReadCapability(resourceReadSpec())
	result := reg.Run(context.Background(), ResourceInfoRequest{
		ResourceType: resourceTypeDisks, DiskIDs: []string{"VOLUME-2"},
	}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.NotContains(t, exec.calls[0].args, "HostId")
	assert.NotContains(t, result.Reply, "udisk-1")
	assert.Contains(t, result.Reply, "volume-2")
	require.Len(t, result.Envelope.Subjects, 1)
	assert.Equal(t, "volume-2", result.Envelope.Subjects[0].ID)
}

func TestDiskInfoReturnsStructuredEmptyForUnknownDiskID(t *testing.T) {
	exec := &fakeReadExec{result: diskFixture()}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{
		ResourceType: resourceTypeDisks, DiskIDs: []string{"udisk-missing"},
	}, ReadRuntime{Executor: exec})

	assert.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Equal(t, diskInfoAction, result.ToolAction)
	assert.Equal(t, "未查询到符合条件的磁盘。", result.Reply)
}

func TestDiskInfoRejectsMultipleInstanceFiltersBeforeCallingUpstream(t *testing.T) {
	exec := &fakeReadExec{result: diskFixture()}
	resolver := entity.RegistrySnapshot{Instances: map[string]entity.InstanceSnapshot{
		"uhost-1": {UHostId: "uhost-1"},
		"uhost-2": {UHostId: "uhost-2"},
	}}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{ResourceType: resourceTypeDisks, Targets: []platform.TargetRef{
		{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-1", Source: platform.SourceUserText},
		{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-2", Source: platform.SourceUserText},
	}}, ReadRuntime{Executor: exec, Resolver: resolver})

	assert.Equal(t, platform.ReadStatusConflict, result.Status)
	assert.True(t, result.NeedsClarification)
	assert.Empty(t, exec.calls)
}

func TestResourceInfoDoesNotIgnoreDiskIDsWithoutDiskMode(t *testing.T) {
	exec := &fakeReadExec{result: diskFixture()}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{
		DiskIDs: []string{"udisk-1"},
	}, ReadRuntime{Executor: exec})

	assert.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Empty(t, exec.calls)
}

func assertDiskFact(t *testing.T, env *envelope.Envelope, subjectID, key string, want any) {
	t.Helper()
	for _, fact := range env.Facts {
		if fact.SubjectID == subjectID && fact.Key == key {
			assert.Equal(t, want, fact.Value)
			return
		}
	}
	t.Fatalf("missing disk fact %s/%s", subjectID, key)
}
