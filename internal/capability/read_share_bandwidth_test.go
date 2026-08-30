package capability

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type shareBandwidthReadExec struct {
	result        map[string]any
	directCalls   []fakeReadExecCall
	internalCalls []fakeReadExecCall
}

func (e *shareBandwidthReadExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.directCalls = append(e.directCalls, fakeReadExecCall{action: action, args: args})
	return e.result, nil
}

func (e *shareBandwidthReadExec) ExecuteInternal(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.internalCalls = append(e.internalCalls, fakeReadExecCall{action: action, args: args})
	return e.result, nil
}

func TestResourceInfoSharedBandwidthReadsTheInstanceEIPScope(t *testing.T) {
	exec := &shareBandwidthReadExec{result: describeFixture(map[string]any{
		"UHostId": "uhost-a", "Name": "train-a", "State": "Running", "GpuType": "H20",
		"IPSet": []any{map[string]any{
			"IP": "203.0.113.8",
			"ShareBandwidth": map[string]any{
				"ShareBandwidthId": "public-share-1",
				"Scope":            "Public", "Bandwidth": float64(2), "Status": "Available",
				"CanSwitch": true, "TargetScope": "Company",
			},
		}},
	})}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{
		ResourceType: resourceTypeShareBandwidth,
	}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.internalCalls, 1)
	assert.Empty(t, exec.directCalls)
	assert.Equal(t, true, exec.internalCalls[0].args["IncludeShareBandwidth"])
	assert.Contains(t, result.Reply, "平台公共共享带宽 2 Gbps")
	assert.Contains(t, result.Reply, "已有可用的公司独享共享带宽池")
	assert.Contains(t, result.Reply, "不等于端到端传输测速")
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindResourceInfo, result.Envelope.Kind)
	assert.Equal(t, envelope.SubjectInstance, result.Envelope.Subjects[0].Type)
	scope, ok := resourceFactValue(result.Envelope, "uhost-a", "eip_1_scope")
	require.True(t, ok)
	assert.Equal(t, "Public", scope)
	_, publicIDExposed := resourceFactValue(result.Envelope, "uhost-a", "eip_1_share_bandwidth_id")
	assert.False(t, publicIDExposed, "platform-owned pool ids are not customer resource facts")
	bandwidthFact := findShareBandwidthFact(result.Envelope.Facts, "eip_1_bandwidth")
	require.NotNil(t, bandwidthFact)
	assert.Contains(t, bandwidthFact.Label, "非单实例带宽保证")
	publicMeaning, ok := computedFactValue(result.Envelope, "eip_1_public_scope_interpretation")
	require.True(t, ok)
	assert.Contains(t, publicMeaning, "不是单实例保底带宽")
	switchMeaning, ok := computedFactValue(result.Envelope, "eip_1_switch_interpretation")
	require.True(t, ok)
	assert.Contains(t, switchMeaning, "不证明支持购买")
}

func findShareBandwidthFact(facts []envelope.Fact, key string) *envelope.Fact {
	for index := range facts {
		if facts[index].Key == key {
			return &facts[index]
		}
	}
	return nil
}

func TestResourceInfoSharedBandwidthRejectsDiskParameters(t *testing.T) {
	exec := &shareBandwidthReadExec{}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{
		ResourceType: resourceTypeShareBandwidth,
		DiskIDs:      []string{"volume-1"},
	}, ReadRuntime{Executor: exec})

	assert.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Empty(t, exec.directCalls)
	assert.Empty(t, exec.internalCalls)
}

func TestOrdinaryResourceInfoDoesNotRequestShareBandwidthExpansion(t *testing.T) {
	exec := &shareBandwidthReadExec{result: describeFixture(instanceRowMap("uhost-a", "train-a", "Running"))}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.directCalls, 1)
	assert.NotContains(t, exec.directCalls[0].args, "IncludeShareBandwidth")
	assert.Empty(t, exec.internalCalls)
}

func TestResourceInfoSharedBandwidthReturnsAnEmptyReadWhenNoInstanceMatches(t *testing.T) {
	exec := &shareBandwidthReadExec{result: describeFixture()}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{
		ResourceType: resourceTypeShareBandwidth,
	}, ReadRuntime{Executor: exec})

	assert.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Contains(t, result.Reply, "当前账号没有查询到实例")
	assert.Nil(t, result.Envelope)
}

func TestResourceInfoSharedBandwidthPaginatesAndKeepsTheExistingDisplayCap(t *testing.T) {
	rows := accountWithInstances(101)
	for _, row := range rows {
		row["IPSet"] = []any{map[string]any{
			"ShareBandwidth": map[string]any{"Scope": "Public", "Bandwidth": float64(2)},
		}}
	}
	exec := &pagedInstanceExec{rows: rows}
	result := NewReadCapability(resourceReadSpec()).Run(context.Background(), ResourceInfoRequest{
		ResourceType: resourceTypeShareBandwidth,
	}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, true, exec.calls[0].args["IncludeShareBandwidth"])
	assert.Equal(t, true, exec.calls[1].args["IncludeShareBandwidth"])
	require.NotNil(t, result.Envelope)
	assert.Len(t, result.Envelope.Subjects, 50)
	total, ok := computedFactValue(result.Envelope, "total_count")
	require.True(t, ok)
	assert.Equal(t, "101", total)
	truncated, ok := computedFactValue(result.Envelope, "truncated")
	require.True(t, ok)
	assert.Equal(t, "true", truncated)
}
