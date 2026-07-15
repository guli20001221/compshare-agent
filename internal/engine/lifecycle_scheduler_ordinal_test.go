package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDirectLifecycleResolvesOrdinalFromDisplayedList verifies lifecycle gains
// deterministic ordinal target resolution ("关机第2台") against the persisted
// candidate list — a capability the pre-existing deterministic path (explicit
// ID / unique name / pronoun-via-SelectedInstanceID) lacked. No model call.
func TestDirectLifecycleResolvesOrdinalFromDisplayedList(t *testing.T) {

	var stoppedID string
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
				map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "StopCompShareInstance":
			stoppedID, _ = args["UHostId"].(string)
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.userTurn = 3
	eng.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "alpha", "Running"),
		testInstance("uhost-b", "beta", "Running"),
	}, intent.IntentResourceInfo, "我有哪些实例", 2, false)
	eng.lastUserMsg = "关机第2台" // live-turn user message == the arg passed below

	reply, handled := eng.tryDirectLifecycleFromUserText(context.Background(), "关机第2台", noopStep)

	require.True(t, handled, "ordinal lifecycle should be handled deterministically")
	assert.Equal(t, "uhost-b", stoppedID, "第2台 resolves to the second displayed instance")
	assert.Contains(t, reply, "执行关机")
}

// TestDirectStopSchedulerResolvesOrdinalFromDisplayedList verifies scheduler
// gains the same deterministic ordinal resolution.
func TestDirectStopSchedulerResolvesOrdinalFromDisplayedList(t *testing.T) {

	var scheduledID string
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
				map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "UpdateCompShareStopScheduler":
			scheduledID, _ = args["UHostId"].(string)
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.userTurn = 3
	eng.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "alpha", "Running"),
		testInstance("uhost-b", "beta", "Running"),
	}, intent.IntentResourceInfo, "我有哪些实例", 2, false)
	eng.lastUserMsg = "第2台30分钟后关机"

	reply, handled := eng.tryDirectStopSchedulerFromUserText(context.Background(), "第2台30分钟后关机", noopStep)

	require.True(t, handled, "ordinal scheduler should be handled deterministically")
	assert.Equal(t, "uhost-b", scheduledID, "第2台 resolves to the second displayed instance")
	assert.NotEmpty(t, reply)
}

// TestDirectLifecycleOrdinalNeverPoisonsTrustGuard is the anti-regression pin
// for the poisoning vector the model-based approach would have introduced: an
// instance name that is NOT in the user's literal text (and matches only via a
// free-registry lookup) must NOT be resolved by the ordinal path, so it can
// never be recorded as the user-selected instance. ordinalTargetFromPending
// only trusts the user-displayed candidate list matched against the literal
// user text — never a free-registry name — so a bare "关机" cannot bind any
// instance and SelectedInstanceID stays clean.
func TestDirectLifecycleOrdinalNeverPoisonsTrustGuard(t *testing.T) {

	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running", "Zone": "cn-wlcb-01"},
		}}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.userTurn = 3
	eng.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "alpha", "Running"),
		testInstance("uhost-b", "beta", "Running"),
	}, intent.IntentResourceInfo, "我有哪些实例", 2, false)
	// Bare "关机": no ordinal, no name, no ID in the literal user text.
	eng.lastUserMsg = "关机"

	_, handled := eng.tryDirectLifecycleFromUserText(context.Background(), "关机", noopStep)

	// The ordinal path must not bind anything from a bare "关机".
	assert.False(t, handled, "bare 关机 with a pending list must not deterministically dispatch a stop")
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.SelectedInstanceID, "no instance may be recorded from an unresolved bare 关机")
}
