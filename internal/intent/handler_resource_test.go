package intent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceInfoHandler_TargetByNameCallsDescribe(t *testing.T) {
	resolver := resourceTestSnapshot(t)
	exec := &mockHandlerExecutor{result: describeResult("uhost-a", "train-a")}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{{
			Type:       TargetRefName,
			Value:      "train-a",
			Source:     SourceUserText,
			SourceSpan: "train-a",
		}}),
		Resolver: resolver,
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	assert.Equal(t, CutoverStatusDispatched, result.CutoverStatus)
	assert.Contains(t, result.Reply, "train-a")
	assert.NotContains(t, result.Reply, "train-b")
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", exec.calls[0].action)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Equal(t, "DescribeCompShareInstance", result.ToolAction)
	assert.Equal(t, []string{"uhost-a"}, result.ToolArgs["UHostIds"])
}

func TestResourceInfoHandler_TargetByUserIDCallsDescribe(t *testing.T) {
	resolver := resourceTestSnapshot(t)
	exec := &mockHandlerExecutor{result: describeResult("uhost-b", "train-b")}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{{
			Type:       TargetRefUHostIDUserInput,
			Value:      "uhost-b",
			Source:     SourceUserText,
			SourceSpan: "uhost-b",
		}}),
		Resolver: resolver,
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-b"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "train-b")
}

func TestResourceInfoHandler_NoTargetListsInstances(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			instanceRow("uhost-b", "train-b"),
			instanceRow("uhost-a", "train-a"),
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan(nil),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, 100, exec.calls[0].args["Limit"])
	assert.Contains(t, result.Reply, "train-a")
	assert.Contains(t, result.Reply, "train-b")
	assert.Less(t, indexOf(t, result.Reply, "uhost-a"), indexOf(t, result.Reply, "uhost-b"))
}

func TestResourceInfoHandler_AmbiguousNameFallsBackBeforeTool(t *testing.T) {
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			instanceRow("uhost-a", "same-name"),
			instanceRow("uhost-b", "same-name"),
		},
	}, "test"))
	exec := &mockHandlerExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{{
			Type:       TargetRefName,
			Value:      "same-name",
			Source:     SourceUserText,
			SourceSpan: "same-name",
		}}),
		Resolver: reg.Snapshot(),
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackAmbiguousTarget, result.FallbackReason)
	assert.Equal(t, CutoverStatusFallbackUnresolvedTarget, result.CutoverStatus)
	assert.Empty(t, exec.calls)
}

func TestResourceInfoHandler_APIFailureReturnsFriendlyFailure(t *testing.T) {
	exec := &mockHandlerExecutor{err: errors.New("raw provider secret error")}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan(nil),
	})

	assert.Equal(t, HandlerStatusFailureAfterTool, result.Status)
	assert.Equal(t, CutoverStatusFailureAfterTool, result.CutoverStatus)
	assert.Contains(t, result.Reply, FriendlyToolFailureReply)
	assert.NotContains(t, result.Reply, "raw provider secret error")
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", exec.calls[0].action)
}

type mockHandlerExecutor struct {
	result map[string]any
	err    error
	calls  []handlerExecCall
}

type handlerExecCall struct {
	action string
	args   map[string]any
}

func (m *mockHandlerExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, handlerExecCall{action: action, args: copyArgs(args)})
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func resourceTestSnapshot(t *testing.T) entity.RegistrySnapshot {
	t.Helper()
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			instanceRow("uhost-a", "train-a"),
			instanceRow("uhost-b", "train-b"),
		},
	}, "test"))
	return reg.Snapshot()
}

func resourceInfoPlan(refs []TargetRef) Plan {
	return Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentResourceInfo,
		Slots:         Slots{TargetRefs: refs},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.8,
	}
}

func describeResult(id, name string) map[string]any {
	return map[string]any{
		"TotalCount": float64(1),
		"UHostSet":   []any{instanceRow(id, name)},
	}
}

func instanceRow(id, name string) map[string]any {
	return map[string]any{
		"UHostId":   id,
		"Name":      name,
		"State":     "Running",
		"GpuType":   "4090",
		"GPU":       float64(1),
		"CPU":       float64(8),
		"Memory":    float64(64),
		"ImageType": "Ubuntu",
	}
}

func indexOf(t *testing.T, s, substr string) int {
	t.Helper()
	idx := strings.Index(s, substr)
	require.NotEqual(t, -1, idx, "expected %q in %q", substr, s)
	return idx
}
