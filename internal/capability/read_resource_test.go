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
// reference resolves to a UHostId and the describe call pins exactly that id
// (the user already chose the target, so nothing is truncated).
func TestResourceHandle_ExplicitIDPinsTargetAndDoesNotTruncate(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(instanceRowMap("uhost-a", "train-a", "Running"))}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"}, [2]string{"uhost-b", "train-b"})

	result := runResource(t, exec, resolver, ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-a", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
}

// TestResourceHandle_EmptyListIsStructuredEmpty: an account with no instances is
// a structured Empty read (issue 1) — the Agent tells it apart from a Handled
// answer and pairs it with CanAssertAbsence to state "you have none".
func TestResourceHandle_EmptyListIsStructuredEmpty(t *testing.T) {
	result := runResource(t, &fakeReadExec{result: describeFixture()}, nil, ResourceInfoRequest{})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
}

// TestResourceHandle_AmbiguousNameIsConflict: a name matching multiple instances
// is a structured Conflict (issue 1) asserted as a status, not only a fallback
// reason plus follow-up prose.
func TestResourceHandle_AmbiguousNameIsConflict(t *testing.T) {
	resolver := refundResolver(t, [2]string{"uhost-a", "dup"}, [2]string{"uhost-b", "dup"})

	result := runResource(t, &fakeReadExec{result: describeFixture()}, resolver, ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "dup", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusConflict, result.Status)
	assert.True(t, result.NeedsClarification, "a conflict drives the Agent to ask which one")
	assert.Contains(t, result.Reply, "多个实例")
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

// TestResourceHandle_ColdRegistryExactIDPointQueries: a user-typed exact id that
// misses a cold (never-synced) registry is unverifiable locally, not absent, so
// resource_info passes it through to a DescribeCompShareInstance point-query
// instead of refusing before the call. The upstream RESPONSE is what establishes
// the instance really exists.
func TestResourceHandle_ColdRegistryExactIDPointQueries(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(instanceRowMap("uhost-cold", "train-cold", "Running"))}

	result := runResource(t, exec, coldRegistrySnapshot(), ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-cold", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-cold"}, exec.calls[0].args["UHostIds"], "a cold exact id must point-query, not be refused before the call")
	assert.Contains(t, result.Reply, "uhost-cold")
}

// TestResourceHandle_FreshCompleteAbsentExactIDFallsBack: a synced, complete
// registry HAS standing to assert absence, so a genuinely-absent exact id is a
// real "not in your account" — refused before any upstream call, never a wasted
// point-query. The cold-registry pass-through applies only when absence cannot
// be asserted.
func TestResourceHandle_FreshCompleteAbsentExactIDFallsBack(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(instanceRowMap("uhost-a", "train-a", "Running"))}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runResource(t, exec, resolver, ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-ghost", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackUnresolvedTarget, result.FallbackReason)
	assert.Empty(t, exec.calls, "an authoritative registry that can assert absence must not point-query")
}

// TestResourceHandle_ColdIDResponseMismatchIsEmpty is the same-id contract on the
// read path (bug #4): the user asks for uhost-requested, the cold-registry point-
// query returns a DIFFERENT instance (uhost-other). The response is filtered to the
// requested id — which is absent — so the read is a structured Empty, never a
// Handled answer that renders the wrong instance as if it were the one asked about.
func TestResourceHandle_ColdIDResponseMismatchIsEmpty(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(instanceRowMap("uhost-other", "someone-else", "Running"))}

	result := runResource(t, exec, coldRegistrySnapshot(), ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-requested", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusEmpty, result.Status,
		"a response that does not echo the requested id yields Empty, not a wrong-instance Handled")
	assert.NotContains(t, result.Reply, "uhost-other")
	assert.Empty(t, result.Effects, "a mismatched response verifies no instance existence")
}

// TestResourceHandle_EmitsVerifiedInstancesEffect: a same-id-verified response
// declares its echoed ids as write-path existence evidence via RememberVerified
// Instances — the only read channel that authorizes existence for a later write.
func TestResourceHandle_EmitsVerifiedInstancesEffect(t *testing.T) {
	exec := &fakeReadExec{result: describeFixture(instanceRowMap("uhost-a", "train-a", "Running"))}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"}, [2]string{"uhost-b", "train-b"})

	result := runResource(t, exec, resolver, ResourceInfoRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-a", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Equal(t, []ReadEffect{RememberVerifiedInstances{IDs: []string{"uhost-a"}}}, result.Effects,
		"the echoed id is declared as same-id-verified existence evidence")
}

func TestResourceHandle_UpstreamError(t *testing.T) {
	result := runResource(t, errReadExec{err: errors.New("boom")}, nil, ResourceInfoRequest{})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, "DescribeCompShareInstance", result.ToolAction)
	assert.Equal(t, resourceCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}
