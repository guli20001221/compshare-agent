package entity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func syncedSnapshot(at time.Time, ids ...string) RegistrySnapshot {
	instances := map[string]InstanceSnapshot{}
	for _, id := range ids {
		instances[id] = InstanceSnapshot{UHostId: id}
	}
	return RegistrySnapshot{
		Instances:    instances,
		LastFullSync: at,
		SyncEvent:    string(SyncEventSyncRefresh),
		TotalCount:   len(ids),
	}
}

// A fresh, complete registry answers existence from the snapshot alone — a hit is
// Verified and a miss is authoritative NotFound, and NEITHER spends a point-query.
func TestVerifyExactTargetFreshRegistryNeedsNoQuery(t *testing.T) {
	now := time.Now()
	snap := syncedSnapshot(now, "uhost-1", "uhost-2")
	describeCalls := 0
	describe := func(string) (map[string]any, bool) { describeCalls++; return nil, true }

	assert.Equal(t, ExistenceVerified, VerifyExactTarget(snap, "uhost-1", now, describe))
	assert.Equal(t, ExistenceNotFound, VerifyExactTarget(snap, "uhost-ghost", now, describe))
	assert.Zero(t, describeCalls, "a fresh, complete registry must not point-query")
}

// A stale registry that still holds the id is NOT a fresh existence proof (bug:
// a released instance can linger in an hour-old cache) — it must point-query, and
// the response deciding the verdict.
func TestVerifyExactTargetStaleRegistryPointQueries(t *testing.T) {
	now := time.Now()
	stale := syncedSnapshot(now.Add(-time.Hour), "uhost-1")

	echoed := func(id string) (map[string]any, bool) {
		return map[string]any{"UHostSet": []any{map[string]any{"UHostId": id}}}, true
	}
	assert.Equal(t, ExistenceVerified, VerifyExactTarget(stale, "uhost-1", now, echoed),
		"a stale hit is only confirmed by a point-query that echoes the id")

	empty := func(string) (map[string]any, bool) { return map[string]any{"UHostSet": []any{}}, true }
	assert.Equal(t, ExistenceNotFound, VerifyExactTarget(stale, "uhost-1", now, empty),
		"a stale hit whose point-query returns empty is really gone")

	mismatched := func(string) (map[string]any, bool) {
		return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-other"}}}, true
	}
	assert.Equal(t, ExistenceNotFound, VerifyExactTarget(stale, "uhost-1", now, mismatched),
		"a point-query that echoes a DIFFERENT id is not existence for the requested one")

	failed := func(string) (map[string]any, bool) { return nil, false }
	assert.Equal(t, ExistenceUnavailable, VerifyExactTarget(stale, "uhost-1", now, failed),
		"a point-query that could not complete is Unavailable, never a false NotFound")
}

// An invalidated registry (a successful state-changing action ran after the sync)
// is untrusted even while fresh by clock — it must re-verify upstream.
func TestVerifyExactTargetInvalidatedRegistryPointQueries(t *testing.T) {
	now := time.Now()
	snap := syncedSnapshot(now, "uhost-1")
	snap.Invalidated = true
	describeCalls := 0
	describe := func(id string) (map[string]any, bool) {
		describeCalls++
		return map[string]any{"UHostSet": []any{map[string]any{"UHostId": id}}}, true
	}
	assert.Equal(t, ExistenceVerified, VerifyExactTarget(snap, "uhost-1", now, describe))
	assert.Equal(t, 1, describeCalls, "an invalidated snapshot must not answer existence from itself")
}

// A truncated listing is incomplete — a miss is "unverified", so it point-queries
// rather than assert absence.
func TestVerifyExactTargetTruncatedRegistryPointQueries(t *testing.T) {
	now := time.Now()
	snap := syncedSnapshot(now, "uhost-1")
	snap.Truncated = true
	snap.TotalCount = 40
	describeCalls := 0
	describe := func(string) (map[string]any, bool) { describeCalls++; return map[string]any{"UHostSet": []any{}}, true }
	assert.Equal(t, ExistenceNotFound, VerifyExactTarget(snap, "uhost-missing", now, describe))
	assert.Equal(t, 1, describeCalls, "a truncated registry cannot assert absence without a point-query")
}

// A cold registry with no describe channel cannot verify existence: Unavailable,
// not a false NotFound.
func TestVerifyExactTargetColdRegistryNoChannelIsUnavailable(t *testing.T) {
	cold := RegistrySnapshot{SyncEvent: string(SyncEventUnavailable)}
	assert.Equal(t, ExistenceUnavailable, VerifyExactTarget(cold, "uhost-1", time.Now(), nil))
}

func TestSnapshotFreshnessMethods(t *testing.T) {
	now := time.Now()
	fresh := syncedSnapshot(now, "uhost-1")
	assert.False(t, fresh.NeedsRefreshAt(now))
	assert.True(t, fresh.FreshAndCompleteAt(now))
	assert.True(t, fresh.CanAssertAbsenceAt(now))

	stale := syncedSnapshot(now.Add(-time.Hour), "uhost-1")
	assert.True(t, stale.NeedsRefreshAt(now))
	assert.False(t, stale.FreshAndCompleteAt(now))
	assert.False(t, stale.CanAssertAbsenceAt(now), "a stale registry cannot assert absence")

	truncated := syncedSnapshot(now, "uhost-1")
	truncated.Truncated = true
	assert.False(t, truncated.FreshAndCompleteAt(now))
	assert.False(t, truncated.CanAssertAbsenceAt(now))

	emptyAccount := RegistrySnapshot{LastFullSync: now, SyncEvent: string(SyncEventSyncRefresh), TotalCount: 0}
	assert.True(t, emptyAccount.CanAssertAbsenceAt(now), "a cleanly-synced empty account authoritatively has none")

	invalidated := syncedSnapshot(now, "uhost-1")
	invalidated.Invalidated = true
	assert.True(t, invalidated.NeedsRefreshAt(now))
	assert.False(t, invalidated.CanAssertAbsenceAt(now))
}
