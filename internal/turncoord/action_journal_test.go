package turncoord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testActionJournalKey() []byte {
	return bytes.Repeat([]byte{0x6b}, 32)
}

type journalStoreStub struct {
	reserveCalls int
	startCalls   int
	recordCalls  int
	reserveErr   error
	startErr     error
	reserveInput []store.ReserveActionInput
	lastStatus   store.ActionStatus
	lastResult   json.RawMessage
	lastCode     *string
}

func (s *journalStoreStub) ListTurnActions(context.Context, store.Owner, string) ([]store.TurnAction, error) {
	return nil, nil
}

func (s *journalStoreStub) AbandonUnstartedActions(context.Context, store.Owner, store.ConversationLease) error {
	return nil
}

func (s *journalStoreStub) ReserveAction(_ context.Context, _ store.Owner, _ store.ConversationLease, in store.ReserveActionInput) (store.TurnAction, bool, error) {
	s.reserveCalls++
	s.reserveInput = append(s.reserveInput, in)
	if s.reserveErr != nil {
		return store.TurnAction{}, false, s.reserveErr
	}
	return store.TurnAction{TurnID: "turn", Index: 0, ExecutionToken: "token", Status: store.ActionStatusReserved}, true, nil
}

func (s *journalStoreStub) StartAction(context.Context, store.Owner, store.ConversationLease, string) (store.TurnAction, error) {
	s.startCalls++
	if s.startErr != nil {
		return store.TurnAction{}, s.startErr
	}
	return store.TurnAction{TurnID: "turn", Index: 0, ExecutionToken: "token", Status: store.ActionStatusReserved, InFlight: true}, nil
}

func (s *journalStoreStub) RecordActionWithContext(_ context.Context, _ store.Owner, _ string, status store.ActionStatus, result json.RawMessage, code *string, _ *string, _ json.RawMessage) (store.TurnAction, error) {
	s.recordCalls++
	s.lastStatus = status
	s.lastResult = append(s.lastResult[:0], result...)
	if code != nil {
		copied := *code
		s.lastCode = &copied
	}
	return store.TurnAction{}, nil
}

func TestActionJournal_IndexesAreStableAndMonotonicWithinTurn(t *testing.T) {
	actions := &journalStoreStub{}
	journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})
	call := func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": 0}, nil
	}
	_, err := journal.Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "u-1"}, call)
	require.NoError(t, err)
	_, err = journal.Execute(context.Background(), "StartCompShareInstance", map[string]any{"UHostId": "u-2"}, call)
	require.NoError(t, err)
	require.Len(t, actions.reserveInput, 2)
	assert.Equal(t, 0, actions.reserveInput[0].Index)
	assert.Equal(t, 1, actions.reserveInput[1].Index)
}

func TestActionJournal_RefusesCredentialBearingActionWithoutKeyedIdentity(t *testing.T) {
	actions := &journalStoreStub{}
	journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})
	called := false
	_, err := journal.Execute(context.Background(), "ResetCompShareInstancePassword", map[string]any{
		"UHostId": "u-1", "Password": "Aa12" + "3456!",
	}, func(context.Context, string, map[string]any) (map[string]any, error) {
		called = true
		return map[string]any{"RetCode": 0}, nil
	})
	require.Error(t, err)
	assert.False(t, called)
	assert.Zero(t, actions.reserveCalls)
}

func TestActionJournal_RefusesNestedCredentialWithoutKeyedIdentity(t *testing.T) {
	actions := &journalStoreStub{}
	journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})
	called := false
	_, err := journal.Execute(context.Background(), "ExternalWrite", map[string]any{
		"Items": []map[string]any{{"Password": "Aa12" + "3456!"}},
	}, func(context.Context, string, map[string]any) (map[string]any, error) {
		called = true
		return nil, nil
	})
	require.Error(t, err)
	assert.False(t, called)
	assert.Zero(t, actions.reserveCalls)
}

func TestActionJournal_NonDefiniteExternalErrorsBecomeAmbiguous(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "context cancellation", err: context.Canceled},
		{name: "network", err: &net.OpError{Op: "write", Net: "tcp", Err: errors.New("reset")}},
		{name: "parse", err: &json.SyntaxError{Offset: 3}},
		{name: "unknown", err: fmt.Errorf("unexpected executor failure")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actions := &journalStoreStub{}
			journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})
			_, err := journal.Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "u-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
				return nil, tc.err
			})
			require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
			assert.Equal(t, store.ActionStatusAmbiguous, actions.lastStatus)
		})
	}
}

func TestActionJournal_UpstreamBusinessErrorIsRecordedAsDefiniteFailure(t *testing.T) {
	actions := &journalStoreStub{}
	journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})

	_, err := journal.Execute(context.Background(), "CreateCFS", map[string]any{"Zone": "cn-bj2-03"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		return nil, tools.NewUpstreamAPIError(230, "CFS already exists")
	})

	var stored map[string]any
	require.Error(t, err)
	require.NoError(t, journal.Err(), "a completed RetCode response must not poison the whole turn")
	require.Equal(t, store.ActionStatusFailed, actions.lastStatus)
	require.NotNil(t, actions.lastCode)
	assert.Equal(t, "upstream_api:230", *actions.lastCode)
	require.NoError(t, json.Unmarshal(actions.lastResult, &stored))
	assert.Equal(t, float64(230), stored["code"])
	assert.Equal(t, "CFS already exists", stored["message"])
}

func TestActionJournal_PoisonsTurnAfterUnknownOutcome(t *testing.T) {
	actions := &journalStoreStub{}
	journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})
	upstreamCalls := 0
	call := func(context.Context, string, map[string]any) (map[string]any, error) {
		upstreamCalls++
		return nil, context.DeadlineExceeded
	}
	_, err := journal.Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "u-1"}, call)
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	require.ErrorIs(t, journal.Err(), tools.ErrActionOutcomeUncertain)
	_, err = journal.Execute(context.Background(), "StartCompShareInstance", map[string]any{"UHostId": "u-2"}, call)
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	assert.Equal(t, 1, upstreamCalls)
	assert.Equal(t, 1, actions.reserveCalls, "poisoned journal must not claim another action index")
	assert.Equal(t, 1, actions.startCalls)
	assert.Equal(t, 1, actions.recordCalls)
}

func TestActionJournal_HealthIsVisibleToCommitCoordinator(t *testing.T) {
	actions := &journalStoreStub{}
	journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})
	require.NoError(t, journal.Err())
	_, err := journal.Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "u-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		return nil, errors.New("unknown write outcome")
	})
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	require.ErrorIs(t, journal.Err(), tools.ErrActionOutcomeUncertain)
}

func TestActionJournal_PoisonsTurnAfterReservationStoreError(t *testing.T) {
	actions := &journalStoreStub{reserveErr: errors.New("commit acknowledgement lost")}
	journal := NewActionJournal(actions, store.Owner{}, store.ConversationLease{TurnID: "turn"})
	upstreamCalls := 0
	call := func(context.Context, string, map[string]any) (map[string]any, error) {
		upstreamCalls++
		return nil, nil
	}
	_, err := journal.Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "u-1"}, call)
	require.Error(t, err)
	require.ErrorIs(t, journal.Err(), tools.ErrActionOutcomeUncertain)
	_, err = journal.Execute(context.Background(), "StartCompShareInstance", map[string]any{"UHostId": "u-2"}, call)
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	assert.Zero(t, upstreamCalls)
	assert.Equal(t, 1, actions.reserveCalls)
}
