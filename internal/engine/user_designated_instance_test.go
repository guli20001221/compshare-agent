package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// twoInstanceRegistry gives the engine a fresh, complete registry holding two
// running instances. Two, not one, so account-single can never supply the proof
// and every assertion below is about the user's own designation.
func twoInstanceRegistry(t *testing.T, eng *Engine) {
	t.Helper()
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2), "UHostSet": []any{
			map[string]any{"UHostId": "cpod-typed-1", "Name": "zonereach-wlcb-03", "State": "Running"},
			map[string]any{"UHostId": "uhost-other-2", "Name": "trainer-b", "State": "Running"},
		},
	}, "test"))
}

// rehydrate round-trips the session state through the persisted JSON envelope the
// HTTP lease rebuilds on every request, so these tests prove the PERSISTED
// designation rather than an accidental in-memory carry.
func rehydrate(t *testing.T, eng *Engine) {
	t.Helper()
	state, version, hydrated := eng.SessionStateSnapshot()
	require.True(t, hydrated)
	raw, err := json.Marshal(PersistedContext{AgentSessionState: state})
	require.NoError(t, err)
	persisted, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	eng.ClearSessionState()
	eng.SetSessionState(persisted.AgentSessionState, version+1)
}

// A current typed ID enters without a card and becomes the fresh selection for
// a later pronoun follow-up.
func TestTypedInstanceIDAuthorizesEntryAndCarriesAcrossTurns(t *testing.T) {
	const instanceID = "cpod-typed-1"
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-typed-1","Task":"排查 ComfyUI 打不开"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-typed-1","Task":"继续排查 ComfyUI"}`)}},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	twoInstanceRegistry(t, eng)

	_, err := eng.ChatWithOptions(context.Background(),
		"排查 cpod-typed-1 上的 ComfyUI", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, runner.calls)

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, instanceID, state.SelectedInstanceID)
	require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)

	rehydrate(t, eng)
	_, err = eng.ChatWithOptions(context.Background(), "继续排查", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, runner.calls)
	require.Equal(t, instanceID, runner.lastReq.InstanceID)
}

// The same designation, made the other two ways the server can verify against the
// user's literal text.
func TestUserDesignationAlsoCarriesFromANameAndFromAShownOrdinal(t *testing.T) {
	t.Run("unique exact instance name", func(t *testing.T) {
		eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
		eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
		twoInstanceRegistry(t, eng)

		eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
			eng, "zonereach-wlcb-03 上的服务起不来", "turn-name", time.Now())
		eng.turnContextViewReady = true
		eng.recordUserDesignatedInstance()

		state, _, _ := eng.SessionStateSnapshot()
		require.Equal(t, "cpod-typed-1", state.SelectedInstanceID)
		require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
	})

	t.Run("ordinal against a list the server displayed", func(t *testing.T) {
		eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
		eng.SetSessionState(SessionState{
			SchemaVersion:        SessionStateSchemaCurrent,
			PendingSelectionKind: "instance",
			PendingSelectionItems: []PendingSelectionItem{
				{Index: 1, ID: "cpod-typed-1", Name: "zonereach-wlcb-03"},
				{Index: 2, ID: "uhost-other-2", Name: "trainer-b"},
			},
			PendingSelectionProducedAtUnix: time.Now().Unix(),
			PendingSelectionTTLSeconds:     600,
		}, 1)
		twoInstanceRegistry(t, eng)

		eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
			eng, "第2台", "turn-ordinal", time.Now())
		eng.turnContextViewReady = true
		eng.recordUserDesignatedInstance()

		state, _, _ := eng.SessionStateSnapshot()
		require.Equal(t, "uhost-other-2", state.SelectedInstanceID,
			"a pick from a list the SERVER showed is the user designating, not the model choosing")
		require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
	})
}

// The negative controls. Each of these is a way the target could be established
// WITHOUT the user pointing at it, and none of them may become a designation —
// this is the #546 boundary, and widening the writer must not quietly widen it.
func TestOnlyTheUsersOwnWordsBecomeADesignation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		question string
		setup    func(*testing.T, *Engine)
		wantID   string
		wantSrc  string
	}{
		{
			// A read observed an instance. Observed is not chosen.
			name:     "an observed instance stays observed",
			question: "服务起不来",
			setup: func(t *testing.T, eng *Engine) {
				eng.recordObservedInstanceID("cpod-typed-1", "zonereach-wlcb-03")
			},
			wantID:  "cpod-typed-1",
			wantSrc: SelectedInstanceSourceObserved,
		},
		{
			// An id the user did not type cannot be designated by anyone else —
			// including the model, whose tool arguments this path never reads.
			name:     "a target named by nobody records nothing",
			question: "ComfyUI 打不开",
			wantID:   "",
			wantSrc:  "",
		},
		{
			// Explicit but unbound: the user pointed at something the server cannot
			// resolve. Recording a guess here would answer a reference the user made
			// to something else.
			name:     "a mistyped id records nothing",
			question: "排查 cpod-typo-9zz",
			wantID:   "",
			wantSrc:  "",
		},
		{
			name:     "an out-of-range ordinal records nothing",
			question: "第 7 台还是连不上",
			wantID:   "",
			wantSrc:  "",
		},
		{
			// Two references that disagree are a conflict, never a pick-one.
			name:     "two conflicting references record nothing",
			question: "cpod-typed-1 和 uhost-other-2 都要看",
			wantID:   "",
			wantSrc:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
			twoInstanceRegistry(t, eng)
			if tc.setup != nil {
				tc.setup(t, eng)
			}

			eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
				eng, tc.question, "turn-negative", time.Now())
			eng.turnContextViewReady = true
			eng.recordUserDesignatedInstance()

			state, _, _ := eng.SessionStateSnapshot()
			require.Equal(t, tc.wantID, state.SelectedInstanceID)
			require.Equal(t, tc.wantSrc, state.SelectedInstanceSource)
		})
	}
}

func TestConflictingOrUnresolvedCurrentTargetClearsCarriedSelection(t *testing.T) {
	for _, question := range []string{
		"排查 cpod-typed-1 和 cpod-does-not-exist",
		"排查 cpod-does-not-exist",
		"第 7 台还是连不上",
	} {
		t.Run(question, func(t *testing.T) {
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetSessionState(SessionState{
				SchemaVersion:          SessionStateSchemaCurrent,
				SelectedInstanceID:     "cpod-typed-1",
				SelectedInstanceName:   "old",
				SelectedInstanceSource: SelectedInstanceSourceUser,
				SelectedInstanceAtUnix: time.Now().Add(-2 * time.Hour).Unix(),
			}, 1)
			twoInstanceRegistry(t, eng)
			eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
				eng, question, "turn-clear-old", time.Now())
			eng.turnContextViewReady = true

			eng.recordUserDesignatedInstance()
			state, _, _ := eng.SessionStateSnapshot()
			require.Empty(t, state.SelectedInstanceID,
				"a later vague turn must not resurrect the old target after an explicit miss or conflict")

			view := (ContextCompiler{}).CompileForTurn(eng, "继续排查", "turn-after-clear", time.Now())
			require.False(t, eng.bindInstanceTarget(view).bound())
		})
	}
}

// The account's sole instance is a deterministic BINDING, and deliberately not a
// designation. Tier B produces it for a message that names nothing, so a writer
// that accepted any bound id would stamp "the user chose this" onto a turn where
// the user chose nothing — and then keep that stamp after a second instance is
// created and the account stops being single.
func TestTheAccountsSoleInstanceIsNotADesignation(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1), "UHostSet": []any{
			map[string]any{"UHostId": "cpod-only-1", "Name": "solo", "State": "Running"},
		},
	}, "test"))

	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, "服务起不来", "turn-account-single", time.Now())
	eng.turnContextViewReady = true
	eng.recordUserDesignatedInstance()

	state, _, _ := eng.SessionStateSnapshot()
	require.Empty(t, state.SelectedInstanceSource,
		"being the only instance is not the user picking it")
	require.Empty(t, state.SelectedInstanceID)
}

// Carrying a designation must not rewrite when the user selected it. The binding
// is conversation-scoped, while the timestamp remains truthful observability.
func TestCarriedDesignationDoesNotRefreshItsOwnClock(t *testing.T) {
	then := time.Now().Add(-20 * time.Minute).Unix()
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "cpod-typed-1",
		SelectedInstanceName:   "zonereach-wlcb-03",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: then,
	}, 1)
	twoInstanceRegistry(t, eng)

	// The user says something that names no target at all.
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, "还是不行", "turn-carried", time.Now())
	eng.turnContextViewReady = true
	eng.recordUserDesignatedInstance()

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, then, state.SelectedInstanceAtUnix,
		"carrying a selection is not the user re-making it")
}

// The refusal is two strings for two audiences. The console renders this lane's
// step Message verbatim, so anything model-facing in it reaches the customer.
func TestTargetRefusalKeepsModelInstructionsOffTheUsersScreen(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "must not run"}}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, "ComfyUI 打不开", "turn-refusal", time.Now())
	eng.turnContextViewReady = true

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "cpod-not-designated", "Task": "排查",
	}, captureSteps(&steps))

	require.Zero(t, runner.calls)
	require.Len(t, steps, 1)
	user := steps[0].Message
	for _, leaked := range []string{"不能自行从实例列表挑选", "请让用户", "仅仅读到过的实例"} {
		require.NotContains(t, user, leaked,
			"an instruction addressed to the model must never be rendered to the customer")
	}
	require.Contains(t, user, "实例 ID", "the user needs to be told what to send next")

	// The model half carries the constraint AND the exits — the old single string
	// said only what not to do, so the agent invented recoveries that cannot work.
	require.Contains(t, out, "不能自行从实例列表挑选")
	require.Contains(t, out, "确认", "the model must be told that a bare 确认 will not lift this")
	require.Contains(t, out, "控制台", "…and neither will selecting the row in the console")
	require.False(t, strings.HasPrefix(out, finalReplyPrefix), "a refused target is not terminal")
}
