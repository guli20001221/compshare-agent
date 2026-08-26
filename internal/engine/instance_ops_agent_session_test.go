package engine

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestInstanceOpsAgentSessionResumesOnlyFreshSameTarget(t *testing.T) {
	const sessionID = "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5"
	const anchor = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e := &Engine{sessionState: SessionState{PersistedInstanceOpsAgent: PersistedInstanceOpsAgentSession{
		InstanceID:         "uhost-a",
		SessionID:          sessionID,
		WorkdirID:          sessionID,
		Contract:           instanceOpsAgentSessionContract,
		Model:              "gpt-5.6-terra",
		ConversationAnchor: anchor,
		UpdatedAt:          time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
	}}}

	got := e.instanceOpsAgentSessionForRun("uhost-a")
	require.True(t, got.Resume)
	require.Equal(t, sessionID, got.SessionID)
	require.Equal(t, sessionID, got.WorkdirID)
	require.Equal(t, "gpt-5.6-terra", got.Model)
	require.Equal(t, anchor, got.ConversationAnchor)

	other := e.instanceOpsAgentSessionForRun("cpod-b")
	require.False(t, other.Resume)
	require.NotEqual(t, sessionID, other.SessionID)
	require.NoError(t, uuid.Validate(other.SessionID))
	require.Equal(t, other.SessionID, other.WorkdirID)
	require.Empty(t, other.Model)
}

func TestInstanceOpsAgentSessionExpiredOrMalformedStartsFresh(t *testing.T) {
	for _, persisted := range []PersistedInstanceOpsAgentSession{
		{
			InstanceID: "uhost-a", SessionID: "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5",
			WorkdirID: "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5",
			Contract:  instanceOpsAgentSessionContract,
			UpdatedAt: time.Now().UTC().Add(-instanceOpsAgentSessionTTL - time.Minute).Format(time.RFC3339Nano),
		},
		{
			InstanceID: "uhost-a", SessionID: "not-a-uuid", Contract: instanceOpsAgentSessionContract,
			WorkdirID: "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5",
			UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		{
			InstanceID: "uhost-a", SessionID: "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5",
			WorkdirID: "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5",
			Contract:  "old-contract", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
	} {
		e := &Engine{sessionState: SessionState{PersistedInstanceOpsAgent: persisted}}
		got := e.instanceOpsAgentSessionForRun("uhost-a")
		require.False(t, got.Resume)
		require.NoError(t, uuid.Validate(got.SessionID))
		require.NotEqual(t, persisted.SessionID, got.SessionID)
		require.Equal(t, got.SessionID, got.WorkdirID)
	}
}

func TestObserveInstanceOpsAgentSessionPersistsOnlyValidatedCursor(t *testing.T) {
	e := &Engine{}
	const anchor = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e.observeInstanceOpsAgentSession("uhost-a", "bad", "bad", instanceOpsAgentSessionContract, "gpt-5.6-terra", anchor)
	require.True(t, e.sessionState.PersistedInstanceOpsAgent.IsZero())

	const sessionID = "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5"
	e.observeInstanceOpsAgentSession("uhost-a", sessionID, sessionID, instanceOpsAgentSessionContract, "gpt-5.6-terra", anchor)
	got := e.sessionState.PersistedInstanceOpsAgent
	require.Equal(t, "uhost-a", got.InstanceID)
	require.Equal(t, sessionID, got.SessionID)
	require.Equal(t, sessionID, got.WorkdirID)
	require.Equal(t, "gpt-5.6-terra", got.Model)
	require.Equal(t, anchor, got.ConversationAnchor)
	require.Equal(t, SessionStateSchemaCurrent, e.sessionState.SchemaVersion)
	require.NotEmpty(t, got.UpdatedAt)

	// A malformed or cross-contract receipt cannot replace the last proven cursor.
	e.observeInstanceOpsAgentSession("uhost-a", uuid.NewString(), sessionID, "future-contract", "gpt-5.6-terra", anchor)
	require.Equal(t, got, e.sessionState.PersistedInstanceOpsAgent)

	// A v2 session receipt without proof that the role-complete context reached
	// the model cannot advance the cursor (old harness during a mixed deploy).
	e.observeInstanceOpsAgentSession("uhost-a", uuid.NewString(), sessionID, instanceOpsAgentSessionContract, "gpt-5.6-terra", "")
	require.Equal(t, got, e.sessionState.PersistedInstanceOpsAgent)
}

func TestClientCreatedVersionZeroContextCannotSeedContinuationCursors(t *testing.T) {
	const injected = "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5"
	for _, schema := range []string{SessionStateSchemaV8, SessionStateSchemaV9, SessionStateSchemaV10} {
		e := &Engine{}
		e.SetSessionState(SessionState{
			SchemaVersion: schema,
			PersistedInstanceOpsJob: PersistedInstanceOpsJob{
				InstanceID: "uhost-a", JobID: "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				State: "running", Purpose: "client supplied", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
			PersistedInstanceOpsAgent: PersistedInstanceOpsAgentSession{
				InstanceID: "uhost-a", SessionID: injected, WorkdirID: injected, Contract: instanceOpsAgentSessionContract,
				Model: "gpt-5.6-terra", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		}, 0)

		state, _, _ := e.SessionStateSnapshot()
		require.True(t, state.PersistedInstanceOpsJob.IsZero(), "schema %s seeded a job cursor", schema)
		require.True(t, state.PersistedInstanceOpsAgent.IsZero(), "schema %s seeded an agent cursor", schema)
		got := e.instanceOpsAgentSessionForRun("uhost-a")
		require.False(t, got.Resume)
		require.NotEqual(t, injected, got.SessionID)
	}
}
