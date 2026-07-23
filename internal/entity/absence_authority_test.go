package entity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registry answers NOT_FOUND_IN_ACCOUNT for anything it has not seen. That is only
// an honest answer if it has seen everything — and there are three ways it has not.
// Callers turn NOT_FOUND into a hard refusal, so a registry that overstates its own
// knowledge makes the agent tell a user their own instance does not exist.
func TestRegistryWillNotSwearToWhatItHasNotSeen(t *testing.T) {
	synced := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	t.Run("truncated listing", func(t *testing.T) {
		// The real shape of this bug. DescribeCompShareInstance pages: the live test
		// account holds 20 instances and the call returns 10, so Truncated is set
		// (registry.go: Truncated = TotalCount > len(fetched)). The other 10 machines
		// are real, are the user's, and are unknown to this registry.
		r := NewRegistry()
		r.LastFullSync = synced
		r.LastSyncEvent = string(SyncEventSyncRefresh)
		r.Instances = map[string]InstanceSnapshot{"uhost-known": {UHostId: "uhost-known"}}
		r.TotalCount = 20
		r.Truncated = true

		assert.False(t, r.CanAssertAbsence(),
			"it has seen 1 of 20 instances — it is in no position to say the other 19 do not exist")

		// And the raw resolver still says NOT_FOUND, which is exactly why the guard is
		// needed: the status alone cannot be trusted.
		_, res := r.ResolveByID("uhost-1exampleaa01")
		require.Equal(t, ResolveNotFoundInAccount, res.Status,
			"resolver still reports NOT_FOUND — CanAssertAbsence is what makes that safe to ignore")
	})

	t.Run("never synced", func(t *testing.T) {
		// The HTTP path skips engine.Init(), so a session that never lists instances
		// carries an empty registry for its entire life.
		r := NewRegistry()
		assert.False(t, r.CanAssertAbsence(),
			"an empty, never-synced registry knows nothing; 'I have not looked' is not 'it does not exist'")
	})

	t.Run("last sync failed", func(t *testing.T) {
		r := NewRegistry()
		r.LastFullSync = synced
		r.LastSyncEvent = string(SyncEventFailed)
		r.Instances = map[string]InstanceSnapshot{"uhost-stale": {UHostId: "uhost-stale"}}
		r.TotalCount = 1
		assert.False(t, r.CanAssertAbsence(),
			"the sync failed — this view may be arbitrarily stale")
	})
}

// The other half of the contract, and the one that stops this fix from becoming a
// silent hole: a registry that HAS seen the whole account keeps its authority. Without
// these, "never refuse" would pass the tests above just as well as the correct rule.
func TestRegistryKeepsItsAuthorityWhenItHasActuallySeenTheAccount(t *testing.T) {
	synced := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	t.Run("complete listing", func(t *testing.T) {
		r := NewRegistry()
		r.LastFullSync = synced
		r.LastSyncEvent = string(SyncEventSyncRefresh)
		r.Instances = map[string]InstanceSnapshot{
			"uhost-a": {UHostId: "uhost-a"},
			"uhost-b": {UHostId: "uhost-b"},
		}
		r.TotalCount = 2
		r.Truncated = false

		assert.True(t, r.CanAssertAbsence(),
			"it fetched all 2 of 2 — absence here IS a fact, and a bogus id should still be refused")
	})

	t.Run("account genuinely has no instances", func(t *testing.T) {
		r := NewRegistry()
		r.LastFullSync = synced
		r.LastSyncEvent = string(SyncEventSyncRefresh)
		r.TotalCount = 0
		r.Truncated = false

		assert.True(t, r.CanAssertAbsence(),
			"a clean sync of an empty account is authoritative — do not confuse 'no instances' with 'no data'")
	})
}

// The snapshot is what the router validator actually holds (engine.RegistrySnapshot()),
// so it must carry the same rule. A snapshot that forgot it was truncated would reopen
// the whole bug behind an immutable copy.
func TestSnapshotCarriesTheSameAuthorityRuleAsTheRegistry(t *testing.T) {
	r := NewRegistry()
	r.LastFullSync = time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	r.LastSyncEvent = string(SyncEventSyncRefresh)
	r.Instances = map[string]InstanceSnapshot{"uhost-known": {UHostId: "uhost-known"}}
	r.TotalCount = 20
	r.Truncated = true

	snap := r.Snapshot()
	assert.False(t, snap.CanAssertAbsence(), "the snapshot must not be more confident than the registry it copied")
	assert.Equal(t, r.CanAssertAbsence(), snap.CanAssertAbsence())

	// A nil/unwired registry surfaces as SyncEventUnavailable — also no authority.
	assert.False(t, RegistrySnapshot{SyncEvent: string(SyncEventUnavailable)}.CanAssertAbsence())
}
