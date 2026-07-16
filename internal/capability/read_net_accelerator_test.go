package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errReadExec is a stub executor that fails every call, for the post-tool
// failure parity tests.
type errReadExec struct{ err error }

func (e errReadExec) Execute(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, e.err
}

func (e errReadExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func runNetAccelerator(t *testing.T, exec ReadExecutor, req NetworkAcceleratorStatusRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(netAcceleratorReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec})
}

func TestNetAcceleratorRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, NetworkAcceleratorStatusRequest{}.MissingFields())
}

// TestNetAcceleratorHandle_IgnoresTargets locks the parity fact that the
// capability makes the same bare, account-scoped call the legacy handler made:
// the resolved targets are decoded for schema symmetry but never forwarded, and
// the tool action is the read-only optimizer status probe.
func TestNetAcceleratorHandle_IgnoresTargets(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"Optimized": true}}

	result := runNetAccelerator(t, exec, NetworkAcceleratorStatusRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a", Source: platform.SourceUserText}},
	})

	assert.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "CheckCompShareNetOptimizer", result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "CheckCompShareNetOptimizer", exec.calls[0].action)
	assert.Empty(t, exec.calls[0].args, "net-accelerator status takes no instance argument")
	assert.Contains(t, result.Reply, "已开通")
}

// TestNetAcceleratorHandle_MissingRegionDoesNotLeakNil is the typed-path twin of
// the legacy TestRenderNetAcceleratorStatusReply_MissingRegionDoesNotLeakNil.
func TestNetAcceleratorHandle_MissingRegionDoesNotLeakNil(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"Info": []any{
		map[string]any{"Optimized": false},
	}}}

	result := runNetAccelerator(t, exec, NetworkAcceleratorStatusRequest{})

	assert.Contains(t, result.Reply, "网络加速")
	assert.Contains(t, result.Reply, "未开通")
	assert.NotContains(t, result.Reply, "<nil>")
}

func TestNetAcceleratorHandle_RegionRowsRendered(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"Info": []any{
		map[string]any{"Optimized": true, "Region": "cn-bj2"},
	}}}

	result := runNetAccelerator(t, exec, NetworkAcceleratorStatusRequest{})

	assert.Contains(t, result.Reply, "cn-bj2 已开通")
}

func TestNetAcceleratorHandle_EmptyPayload(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{}}

	result := runNetAccelerator(t, exec, NetworkAcceleratorStatusRequest{})

	assert.Equal(t, platform.ReadStatusEmpty, result.Status, "no status data is a structured Empty read")
	assert.Contains(t, result.Reply, "未获取到网络加速状态")
}

func TestNetAcceleratorHandle_UpstreamFailure(t *testing.T) {
	result := runNetAccelerator(t, errReadExec{err: errors.New("boom")}, NetworkAcceleratorStatusRequest{})

	assert.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, "CheckCompShareNetOptimizer", result.ToolAction)
	assert.Equal(t, netAcceleratorCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}
