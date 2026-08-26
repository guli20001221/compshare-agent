package engine

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestInstanceOpsAgentSessionResumesOnlyFreshSameTarget(t *testing.T) {
	const sessionID = "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5"
	e := &Engine{sessionState: SessionState{PersistedInstanceOpsAgent: PersistedInstanceOpsAgentSession{
		InstanceID: "uhost-a",
		SessionID:  sessionID,
		Contract:   instanceOpsAgentSessionContract,
		Model:      "gpt-5.6-terra",
		UpdatedAt:  time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
	}}}

	got := e.instanceOpsAgentSessionForRun("uhost-a")
	require.True(t, got.Resume)
	require.Equal(t, sessionID, got.SessionID)
	require.Equal(t, "gpt-5.6-terra", got.Model)

	other := e.instanceOpsAgentSessionForRun("cpod-b")
	require.False(t, other.Resume)
	require.NotEqual(t, sessionID, other.SessionID)
	require.NoError(t, uuid.Validate(other.SessionID))
	require.Empty(t, other.Model)
}

func TestInstanceOpsAgentSessionExpiredOrMalformedStartsFresh(t *testing.T) {
	for _, persisted := range []PersistedInstanceOpsAgentSession{
		{
			InstanceID: "uhost-a", SessionID: "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5",
			Contract:  instanceOpsAgentSessionContract,
			UpdatedAt: time.Now().UTC().Add(-instanceOpsAgentSessionTTL - time.Minute).Format(time.RFC3339Nano),
		},
		{
			InstanceID: "uhost-a", SessionID: "not-a-uuid", Contract: instanceOpsAgentSessionContract,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		{
			InstanceID: "uhost-a", SessionID: "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5",
			Contract: "old-contract", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
	} {
		e := &Engine{sessionState: SessionState{PersistedInstanceOpsAgent: persisted}}
		got := e.instanceOpsAgentSessionForRun("uhost-a")
		require.False(t, got.Resume)
		require.NoError(t, uuid.Validate(got.SessionID))
		require.NotEqual(t, persisted.SessionID, got.SessionID)
	}
}

func TestObserveInstanceOpsAgentSessionPersistsOnlyValidatedCursor(t *testing.T) {
	e := &Engine{}
	e.observeInstanceOpsAgentSession("uhost-a", "bad", instanceOpsAgentSessionContract, "gpt-5.6-terra")
	require.True(t, e.sessionState.PersistedInstanceOpsAgent.IsZero())

	const sessionID = "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5"
	e.observeInstanceOpsAgentSession("uhost-a", sessionID, instanceOpsAgentSessionContract, "gpt-5.6-terra")
	got := e.sessionState.PersistedInstanceOpsAgent
	require.Equal(t, "uhost-a", got.InstanceID)
	require.Equal(t, sessionID, got.SessionID)
	require.Equal(t, "gpt-5.6-terra", got.Model)
	require.Equal(t, SessionStateSchemaCurrent, e.sessionState.SchemaVersion)
	require.NotEmpty(t, got.UpdatedAt)

	// A malformed or cross-contract receipt cannot replace the last proven cursor.
	e.observeInstanceOpsAgentSession("uhost-a", uuid.NewString(), "future-contract", "gpt-5.6-terra")
	require.Equal(t, got, e.sessionState.PersistedInstanceOpsAgent)
}

func TestClientCreatedVersionZeroContextCannotSeedContinuationCursors(t *testing.T) {
	const injected = "4ddf6804-9b0b-4527-b6eb-6cc62f65ead5"
	for _, schema := range []string{SessionStateSchemaV8, SessionStateSchemaV9} {
		e := &Engine{}
		e.SetSessionState(SessionState{
			SchemaVersion: schema,
			PersistedInstanceOpsJob: PersistedInstanceOpsJob{
				InstanceID: "uhost-a", JobID: "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				State: "running", Purpose: "client supplied", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
			PersistedInstanceOpsAgent: PersistedInstanceOpsAgentSession{
				InstanceID: "uhost-a", SessionID: injected, Contract: instanceOpsAgentSessionContract,
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
