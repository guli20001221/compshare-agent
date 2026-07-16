package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func instanceRowMap(id, name, state string) map[string]any {
	return map[string]any{
		"UHostId":   id,
		"Name":      name,
		"State":     state,
		"GpuType":   "4090",
		"GPU":       float64(1),
		"CPU":       float64(8),
		"Memory":    float64(64),
		"ImageType": "Ubuntu",
	}
}

func describeFixture(rows ...map[string]any) map[string]any {
	set := make([]any, len(rows))
	for i, r := range rows {
		set[i] = r
	}
	return map[string]any{"TotalCount": float64(len(rows)), "UHostSet": set}
}

func runResource(t *testing.T, exec ReadExecutor, resolver EntityResolver, req ResourceInfoRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(resourceReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec, Resolver: resolver})
}

func computedFactValue(env *envelope.Envelope, key string) (string, bool) {
	if env == nil {
		return "", false
	}
	for _, f := range env.Computed {
		if f.Key == key {
			if s, ok := f.Value.(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

func TestResourceInfoRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, ResourceInfoRequest{}.MissingFields())
}

// TestResourceHandle_ListsAllInstances: an empty target set lists the account —
// the describe call uses the paging Limit (not a pinned UHostIds set), the reply
// enumerates the instances and the envelope is a resource_info envelope with
// selection candidates for a later "第 N 台" follow-up.
func TestResourceHandle_ListsAllInstances(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(
		instanceRowMap("uhost-a", "train-a", "Running"),
		instanceRowMap("uhost-b", "train-b", "Stopped"),
	)}

	result := runResource(t, exec, nil, ResourceInfoRequest{})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "DescribeCompShareInstance", result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, 100, exec.calls[0].args["Limit"], "list-all must use the paging limit, not a pinned id set")
	_, hasUHostIds := exec.calls[0].args["UHostIds"]
	assert.False(t, hasUHostIds)
	assert.Contains(t, result.Reply, "uhost-a")
	assert.Contains(t, result.Reply, "uhost-b")
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindResourceInfo, result.Envelope.Kind)
	assert.Len(t, result.ResourceSelectionCandidates, 2)
}

// TestResourceHandle_AppliesStateFilter: a filter ref describes everything then
// filters client-side; the envelope advertises the applied filter + matched
// count, and the excluded instance is absent from the reply.
func TestResourceHandle_AppliesStateFilter(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(
		instanceRowMap("uhost-a", "train-a", "Running"),
		instanceRowMap("uhost-b", "train-b", "Stopped"),
	)}

	result := runResource(t, exec, nil, ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefFilter, Value: "state=running", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "uhost-a")
	assert.NotContains(t, result.Reply, "uhost-b")
	applied, ok := computedFactValue(result.Envelope, "filter_applied")
	require.True(t, ok)
	assert.Equal(t, "state=running", applied)
	matched, ok := computedFactValue(result.Envelope, "matched_count")
	require.True(t, ok)
	assert.Equal(t, "1", matched)
}

// TestResourceHandle_ExplicitIDPinsTargetAndDoesNotTruncate: an explicit id
// reference resolves to a UHostId, the describe call pins exactly that id, and
// no selection candidates are produced (the user already chose the target).
func TestResourceHandle_ExplicitIDPinsTargetAndDoesNotTruncate(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(instanceRowMap("uhost-a", "train-a", "Running"))}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"}, [2]string{"uhost-b", "train-b"})

	result := runResource(t, exec, resolver, ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-a", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Empty(t, result.ResourceSelectionCandidates, "explicit id targets are not selection candidates")
}

func TestResourceHandle_UnresolvedTargetFallsBack(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(instanceRowMap("uhost-a", "train-a", "Running"))}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runResource(t, exec, resolver, ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "ghost", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackUnresolvedTarget, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

func TestResourceHandle_UpstreamError(t *testing.T) {
	result := runResource(t, errReadExec{err: errors.New("boom")}, nil, ResourceInfoRequest{})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, "DescribeCompShareInstance", result.ToolAction)
	assert.Equal(t, resourceCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}
