package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/workflow"
)

// The per-turn confirmation overrides are installed on session-scoped fields of
// a pooled engine, so a turn that opts into confirm_form_v1 or guided_create_v1
// has to leave the engine exactly as it found it. Without the restore, the next
// turn from a client that advertised neither shape would still be answered with
// the guided intake form, because e.guidedCreate and e.confirmEditsFn are what
// the proposal path reads to decide that.
func TestPerTurnConfirmationOverridesDoNotOutliveTheirTurn(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)

	_, err := eng.ChatWithOptions(context.Background(), "帮我开一台机器", noopStep, ChatOptions{
		ConfirmFunc: func(string, map[string]any) bool { return true },
		ConfirmEditsFunc: func(string, map[string]any, *workflow.ConfirmForm) workflow.ConfirmResolution {
			return workflow.ConfirmResolution{Confirmed: true}
		},
		GuidedCreate: true,
	})
	require.NoError(t, err)

	require.Nil(t, eng.confirmFn,
		"a turn-supplied ConfirmFunc must not stay installed on the pooled engine")
	require.Nil(t, eng.confirmEditsFn,
		"confirm_form_v1 is opted into per turn; the next turn must not inherit the form gate")
	require.False(t, eng.guidedCreate,
		"guided_create_v1 is opted into per turn; the next turn must not inherit guided intake")
}

// A session engine that already carries overrides keeps them: the restore puts
// back what was there, it does not clear the field.
func TestTurnConfirmationRestoreReturnsTheSessionCallback(t *testing.T) {
	sessionConfirm := ConfirmFunc(func(string, map[string]any) bool { return false })
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, sessionConfirm)
	eng.guidedCreate = true

	_, err := eng.ChatWithOptions(context.Background(), "你好", noopStep, ChatOptions{
		ConfirmFunc:  func(string, map[string]any) bool { return true },
		GuidedCreate: true,
	})
	require.NoError(t, err)

	require.NotNil(t, eng.confirmFn, "the session's own ConfirmFunc must survive the turn")
	require.False(t, eng.confirmFn("CreateInstance", nil),
		"the restored callback must be the session's, not the turn's")
	require.True(t, eng.guidedCreate, "a session already in guided mode stays in it")
}

// The already-paid first-round draft lives on turnState, next to the pending
// flag it is the other half of. Turn entry replaces that whole value, so a draft
// the previous turn never delivered cannot be delivered by this one — which
// would answer the new question with the old turn's text.
func TestDirectAnswerDraftDoesNotOutliveItsTurn(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.turnState = turnState{
		directAnswerToolRetryDraft:   "上一轮没送出去的草稿",
		directAnswerToolRetryPending: true,
		truncatedOutputRecoveries:    3,
		recoverTruncatedOutput:       true,
	}

	reply, err := eng.Chat(context.Background(), "你好", noopStep)
	require.NoError(t, err)
	require.Equal(t, "ok", reply, "the previous turn's draft must not become this turn's reply")
	require.Empty(t, eng.directAnswerToolRetryDraft)
	require.False(t, eng.directAnswerToolRetryPending)
	require.Zero(t, eng.truncatedOutputRecoveries)
	require.False(t, eng.recoverTruncatedOutput)
}
