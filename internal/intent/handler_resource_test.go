package intent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
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

func TestResourceInfoHandler_UnresolvedUserIDFallsBackBeforeTool(t *testing.T) {
	resolver := resourceTestSnapshot(t)
	exec := &mockHandlerExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{{
			Type:       TargetRefUHostIDUserInput,
			Value:      "uhost-missing",
			Source:     SourceUserText,
			SourceSpan: "uhost-missing",
		}}),
		Resolver: resolver,
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackUnresolvedTarget, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

func TestResourceInfoHandler_NoTargetListsInstances(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"TotalCount": float64(16),
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
	require.NotNil(t, result.Envelope)
	assertComputedFact(t, *result.Envelope, "total_count", "16")
}

func TestResourceInfoHandler_DedupesResolvedTargets(t *testing.T) {
	resolver := resourceTestSnapshot(t)
	exec := &mockHandlerExecutor{result: describeResult("uhost-a", "train-a")}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{
			{Type: TargetRefUHostIDUserInput, Value: "uhost-a", Source: SourceUserText, SourceSpan: "uhost-a"},
			{Type: TargetRefName, Value: "train-a", Source: SourceUserText, SourceSpan: "train-a"},
		}),
		Resolver: resolver,
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
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

func TestResourceInfoHandler_FilterByRunningState(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"TotalCount": float64(150),
		"UHostSet": []any{
			instanceRowWith("uhost-running-a", "run-a", "Running", "4090"),
			instanceRowWith("uhost-stopped", "stop-a", "Stopped", "4090"),
			instanceRowWith("uhost-running-b", "run-b", "Running", "V100S"),
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{{
			Type:  TargetRefFilter,
			Value: "state=running",
		}}),
		Resolver: resourceTestSnapshot(t),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, 100, exec.calls[0].args["Limit"])
	assert.Contains(t, result.Reply, "run-a")
	assert.Contains(t, result.Reply, "run-b")
	assert.NotContains(t, result.Reply, "stop-a")
	require.NotNil(t, result.Envelope)
	require.Len(t, result.Envelope.Subjects, 2)
	assert.Equal(t, "uhost-running-a", result.Envelope.Subjects[0].ID)
	assert.Equal(t, "uhost-running-b", result.Envelope.Subjects[1].ID)
	assertComputedFact(t, *result.Envelope, "filter_applied", "state=running")
	assertComputedFact(t, *result.Envelope, "matched_count", "2")
	assertComputedFact(t, *result.Envelope, "total_count", "150")
}

func TestResourceInfoHandler_FilterByStoppedStateAlias(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			instanceRowWith("uhost-running", "run-a", "Running", "4090"),
			instanceRowWith("uhost-stopped", "stop-a", "Stopped", "4090"),
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{{
			Type:  TargetRefFilter,
			Value: "all_stopped",
		}}),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "stop-a")
	assert.NotContains(t, result.Reply, "run-a")
	assertComputedFact(t, *result.Envelope, "filter_applied", "state=stopped")
}

func TestResourceInfoHandler_FilterByStateAndGPUTypeUsesAND(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"TotalCount": float64(4),
		"UHostSet": []any{
			instanceRowWith("uhost-a", "running-4090", "Running", "4090"),
			instanceRowWith("uhost-b", "running-4090-48g", "Running", "4090_48G"),
			instanceRowWith("uhost-c", "running-v100", "Running", "V100S"),
			instanceRowWith("uhost-d", "stopped-4090", "Stopped", "4090"),
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{
			{Type: TargetRefFilter, Value: "state=running"},
			{Type: TargetRefFilter, Value: "gpu_type=4090"},
		}),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "running-4090")
	assert.Contains(t, result.Reply, "running-4090-48g")
	assert.NotContains(t, result.Reply, "running-v100")
	assert.NotContains(t, result.Reply, "stopped-4090")
	assertComputedFact(t, *result.Envelope, "filter_applied", "state=running,gpu_type=4090")
	assertComputedFact(t, *result.Envelope, "matched_count", "2")
}

func TestResourceInfoHandler_FilterKeepsDuplicateNamesByID(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"TotalCount": float64(3),
		"UHostSet": []any{
			instanceRowWith("uhost-a", "same-name", "Running", "4090"),
			instanceRowWith("uhost-b", "same-name", "Running", "4090"),
			instanceRowWith("uhost-c", "same-name", "Stopped", "4090"),
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan([]TargetRef{{Type: TargetRefFilter, Value: "state=running"}}),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.NotNil(t, result.Envelope)
	require.Len(t, result.Envelope.Subjects, 2)
	assert.Equal(t, "uhost-a", result.Envelope.Subjects[0].ID)
	assert.Equal(t, "uhost-b", result.Envelope.Subjects[1].ID)
	assertComputedFact(t, *result.Envelope, "matched_count", "2")
}

func TestResourceInfoHandler_InvalidOrMixedFilterFallsBackBeforeTool(t *testing.T) {
	cases := []struct {
		name string
		refs []TargetRef
	}{
		{
			name: "conflicting filters",
			refs: []TargetRef{
				{Type: TargetRefFilter, Value: "state=running"},
				{Type: TargetRefFilter, Value: "state=stopped"},
			},
		},
		{
			name: "filter with explicit target",
			refs: []TargetRef{
				{Type: TargetRefFilter, Value: "state=running"},
				{Type: TargetRefName, Value: "train-a", Source: SourceUserText, SourceSpan: "train-a"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &mockHandlerExecutor{}
			handler := NewDemoHandler(exec)

			result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
				Plan:     resourceInfoPlan(tc.refs),
				Resolver: resourceTestSnapshot(t),
			})

			assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
			assert.Equal(t, FallbackValidation, result.FallbackReason)
			assert.Empty(t, exec.calls)
		})
	}
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
	assert.Equal(t, "DescribeCompShareInstance", result.ToolAction)
	assert.Equal(t, 100, result.ToolArgs["Limit"])
}

func TestResourceInfoHandler_ParseFailureReturnsFriendlyFailure(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{"InvalidShape": true}}
	handler := NewDemoHandler(exec)

	result := handler.HandleResourceInfo(context.Background(), HandlerRequest{
		Plan: resourceInfoPlan(nil),
	})

	assert.Equal(t, HandlerStatusFailureAfterTool, result.Status)
	assert.Equal(t, CutoverStatusFailureAfterTool, result.CutoverStatus)
	assert.Contains(t, result.Reply, FriendlyToolFailureReply)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", result.ToolAction)
	assert.Equal(t, 100, result.ToolArgs["Limit"])
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
	return instanceRowWith(id, name, "Running", "4090")
}

func instanceRowWith(id, name, state, gpuType string) map[string]any {
	return map[string]any{
		"UHostId":   id,
		"Name":      name,
		"State":     state,
		"GpuType":   gpuType,
		"GPU":       float64(1),
		"CPU":       float64(8),
		"Memory":    float64(64),
		"ImageType": "Ubuntu",
	}
}

func assertComputedFact(t *testing.T, env envelope.Envelope, key string, want any) {
	t.Helper()
	for _, fact := range env.Computed {
		if fact.Key == key {
			assert.Equal(t, want, fact.Value)
			assert.Equal(t, envelope.FactSourceComputed, fact.Source)
			return
		}
	}
	t.Fatalf("missing computed fact key=%s in %#v", key, env.Computed)
}

func indexOf(t *testing.T, s, substr string) int {
	t.Helper()
	idx := strings.Index(s, substr)
	require.NotEqual(t, -1, idx, "expected %q in %q", substr, s)
	return idx
}
