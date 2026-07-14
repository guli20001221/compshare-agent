package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
)

func pendingCreateDiskFrame() ContextFrame {
	return ContextFrame{
		Version:        1,
		Kind:           ContextFrameKindWorkflowTask,
		Status:         ContextFrameStatusFailedRecoverable,
		Intent:         string(intent.IntentOperationLifecycle),
		Workflow:       "CreateDiskWorkflow",
		Slots:          map[string]string{"instance_id": "uhost-1"},
		SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
		MissingSlots:   []string{"size_gb"},
		Stage:          "missing_slots",
		ProducedAtUnix: time.Now().Unix(),
		TTLSeconds:     ContextFrameTTLSeconds,
	}
}

func plannerResult(i intent.Intent, confidence float64) intent.IntentRouterResult {
	return intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        i,
		Confidence:    confidence,
	}}
}

func diskContinuationExecutor() *mockExecutorFn {
	return &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "State": "Running", "Zone": "cn-wlcb-01",
				"Region": "cn-wlcb", "GpuType": "4090", "GPU": float64(1),
				"CPU": float64(16), "Memory": float64(64),
			}}}, nil
		case "GetCompShareInstancePrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"Disks": float64(0.25)}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
}

func TestPlannerFallbackStillResolvesAndContinuesMissingSlotTaskOnce(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmedAction string
	eng := NewWithDeps(&mockLLM{}, diskContinuationExecutor(), func(action string, _ map[string]any) bool {
		confirmedAction = action
		return false
	})
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, ContextFrame: pendingCreateDiskFrame()}, 1)
	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"size_gb": "200G"},
	}}
	eng.SetContextDecisionLayer(layer)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		plannerResult(intent.IntentOperationLifecycle, 0.4),
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, handled := eng.tryPlannerDispatch(context.Background(), "200G", "", noopStep, nil)

	require.True(t, handled, "路由器没把握时，仍应由上下文裁决层续接缺参任务")
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "CreateDiskWorkflow", confirmedAction)
	require.Len(t, layer.calls, 1, "预裁决和实际续接必须复用同一次判断")
	assert.Equal(t, intent.IntentUnknown, layer.calls[0].RouterIntent,
		"低置信路由只能作为不可信信号，不能给续接任务授权")
}

func TestPlannerErrorStillReturnsResolverClarificationAndPreservesTask(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, ContextFrame: pendingCreateDiskFrame()}, 1)
	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionClarify,
		Clarify:  "要给原来的实例增加多大的数据盘？",
	}}
	eng.SetContextDecisionLayer(layer)
	eng.SetIntentPlanner(&scriptedIntentPlanner{err: errors.New("router unavailable")},
		IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, handled := eng.tryPlannerDispatch(context.Background(), "继续", "", noopStep, nil)

	require.True(t, handled)
	assert.Equal(t, "要给原来的实例增加多大的数据盘？", reply)
	require.Len(t, layer.calls, 1)
	assert.Equal(t, intent.IntentUnknown, layer.calls[0].RouterIntent)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, ContextFrameKindWorkflowTask, state.ContextFrame.Kind)
}

func TestConfidentTerminalRouteDoesNotClearTaskWithoutResolverNewDecision(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaCurrent,
		PendingDeployModel: "DeepSeek R1",
		ContextFrame:       liveDeployFrame(),
	}, 1)
	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionAnswerFollowup,
		Target:   ContextDecisionTargetKnowledge,
	}}
	eng.SetContextDecisionLayer(layer)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		plannerResult(intent.IntentBillingAccountUnsupported, 0.95),
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentBillingAccountUnsupported}})

	_, handled := eng.tryPlannerDispatch(context.Background(), "账户余额呢", "", noopStep, nil)

	require.True(t, handled, "账号账务安全出口仍应正常工作")
	require.Len(t, layer.calls, 1)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, ContextFrameKindDeploy, state.ContextFrame.Kind,
		"成功路由本身不是用户放弃旧任务的证据")
	assert.Equal(t, "DeepSeek R1", state.PendingDeployModel)
}

func TestFallbackClearsTaskOnlyWhenResolverExplicitlySaysNewOrClear(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	for _, decision := range []string{ContextDecisionNewTask, ContextDecisionClearContext} {
		t.Run(decision, func(t *testing.T) {
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetSessionState(SessionState{
				SchemaVersion:      SessionStateSchemaCurrent,
				PendingDeployModel: "DeepSeek R1",
				ContextFrame:       liveDeployFrame(),
			}, 1)
			layer := &fakeContextDecisionLayer{decision: &ContextDecision{Decision: decision}}
			eng.SetContextDecisionLayer(layer)
			eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
				plannerResult(intent.IntentOperationLifecycle, 0.4),
			}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

			_, handled := eng.tryPlannerDispatch(context.Background(), "换个问题", "", noopStep, nil)

			assert.False(t, handled)
			require.Len(t, layer.calls, 1)
			state, _, _ := eng.SessionStateSnapshot()
			assert.Empty(t, state.ContextFrame.Kind)
			assert.Empty(t, state.PendingDeployModel)
		})
	}
}

func TestResolverFailurePreservesTaskAndFallsBackWithReadOnlyTools(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	client := &mockLLM{responses: []llm.ChatResponse{{Content: "我先确认你要继续哪一步。"}}}
	eng := NewWithDeps(client, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, ContextFrame: pendingCreateDiskFrame()}, 1)
	layer := &fakeContextDecisionLayer{err: errors.New("resolver unavailable")}
	eng.SetContextDecisionLayer(layer)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		plannerResult(intent.IntentOperationLifecycle, 0.95),
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, err := eng.Chat(context.Background(), "200G", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "我先确认你要继续哪一步。", reply)
	require.Len(t, layer.calls, 1)
	require.NotEmpty(t, client.calls)
	reactReq := client.calls[len(client.calls)-1]
	for _, action := range []string{
		"CreateDiskWorkflow", "ResizeDiskWorkflow", "StopInstanceWorkflow", "RebootInstanceWorkflow",
	} {
		assert.False(t, toolListContainsFunction(reactReq.Tools, action),
			"上下文裁决失败时不得把写操作交给模型: %s", action)
	}
	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, ContextFrameKindWorkflowTask, state.ContextFrame.Kind)
}

func TestExpiredTaskMemoryCanInformButCannotResumeAWrite(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	exec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	expired := pendingCreateDiskFrame()
	expired.Freshness = ContinuityFreshnessExpired
	expired.ProducedAtUnix = time.Now().Add(-2 * time.Hour).Unix()
	expired.SlotSources = nil
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame:  expired,
		TaskSnapshot: TaskSnapshot{
			Goal:          "给 uhost-1 增加 200G 数据盘",
			Workflow:      "CreateDiskWorkflow",
			Stage:         "missing_slots",
			MissingSlots:  []string{"size_gb"},
			Status:        TaskSnapshotStatusExpired,
			Freshness:     ContinuityFreshnessExpired,
			UpdatedAtUnix: time.Now().Add(-2 * time.Hour).Unix(),
		},
	}, 1)
	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"size_gb": "200G"},
	}}
	eng.SetContextDecisionLayer(layer)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		plannerResult(intent.IntentOperationLifecycle, 0.4),
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	_, handled := eng.tryPlannerDispatch(context.Background(), "继续", "", noopStep, nil)

	assert.False(t, handled)
	require.Len(t, layer.calls, 1)
	assert.Contains(t, layer.calls[0].TaskSnapshot, "status: expired")
	assert.NotContains(t, exec.calls, "CreateDiskWorkflow",
		"过期的语义记忆可以帮助理解，但绝不能重新获得执行权限")
}

func TestContextDecisionCacheResetsAtEveryChatTurn(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, ContextFrame: pendingCreateDiskFrame()}, 1)
	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionClarify,
		Clarify:  "请补充数据盘大小。",
	}}
	eng.SetContextDecisionLayer(layer)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		plannerResult(intent.IntentOperationLifecycle, 0.4),
		plannerResult(intent.IntentOperationLifecycle, 0.4),
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	for range 2 {
		msg := "继续"
		reply, err := eng.Chat(context.Background(), msg, noopStep)
		require.NoError(t, err)
		assert.Equal(t, "请补充数据盘大小。", reply)
	}

	assert.Len(t, layer.calls, 2, "缓存只能在一轮内复用，下一轮必须读取最新状态后重新裁决")
}

func TestContextDecisionTraceNamesReadSetAndPlannedStateDelta(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.sessionFactContextEnabled = true
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame:  pendingCreateDiskFrame(),
		TaskSnapshot: TaskSnapshot{
			Goal:          "增加数据盘",
			Status:        TaskSnapshotStatusActive,
			Freshness:     ContinuityFreshnessFresh,
			UpdatedAtUnix: time.Now().Unix(),
		},
		RecentFacts: []ToolFact{{
			Kind:           FactKindPriceQuote,
			SubjectID:      "uhost-1",
			Payload:        map[string]any{"price": 1.25},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     300,
		}},
	}, 1)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{Decision: ContextDecisionClearContext}})
	var traces []ContextDecisionTrace
	eng.SetContextDecisionObserver(func(trace ContextDecisionTrace) { traces = append(traces, trace) })

	_, _, err := eng.resolveTurnContextDecision(context.Background(), "算了", intent.IntentUnknown)

	require.NoError(t, err)
	require.Len(t, traces, 1)
	assert.Subset(t, traces[0].ReadSet, []string{"user_text", "router_intent", "active_task", "task_snapshot", "recent_facts"})
	assert.Contains(t, traces[0].StateDelta, "task:clear")
}
