package intent

import (
	"testing"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func syncedRegistry(t *testing.T, total int, truncated bool, ids ...string) *entity.EntityRegistry {
	t.Helper()
	r := entity.NewRegistry()
	r.LastFullSync = time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	r.LastSyncEvent = string(entity.SyncEventSyncRefresh)
	r.Instances = map[string]entity.InstanceSnapshot{}
	for _, id := range ids {
		r.Instances[id] = entity.InstanceSnapshot{UHostId: id}
	}
	r.TotalCount = total
	r.Truncated = truncated
	return r
}

func planTargetingID(id, span string) IntentRoute {
	return IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentDiagnosis,
		Confidence:    0.85,
		Slots: Slots{
			TargetRefs: []TargetRef{{
				Type:       TargetRefUHostIDUserInput,
				Value:      id,
				Source:     SourceUserText,
				SourceSpan: span,
			}},
		},
	}
}

// The turn that motivated this, taken verbatim from real 2026-06-26..07-09 traffic.
//
// The user types their OWN instance id mid-conversation. The router routes it correctly.
// The registry has never heard of that instance — not because it is fake, but because
// DescribeCompShareInstance paged and returned 10 of the account's 20 machines. The old
// validator called that ErrEntityNotFound, killed the plan on every retry, and shipped
// the turn as intent=unknown. The user was told, in effect, that their own instance does
// not exist.
func TestUserOwnInstanceIsNotRefusedJustBecauseTheRegistryPagedPastIt(t *testing.T) {
	const userText = "我的uhost-1exampleaa01扩的是系统盘吧？"
	const theirs = "uhost-1exampleaa01"

	// 20 in the account, 10 fetched -> Truncated. `theirs` is one of the 10 we never saw.
	partial := syncedRegistry(t, 20, true, "uhost-known-a", "uhost-known-b")
	require.False(t, partial.CanAssertAbsence())

	err := ValidateRoute(planTargetingID(theirs, theirs), ValidationContext{
		UserText: userText,
		Resolver: partial,
	})
	assert.NoError(t, err,
		"a registry holding 2 of 20 instances must not be allowed to refuse the user's own machine")
}

// The other side of it. This fix relaxes ONE claim — "this instance exists in the
// account" — and must not touch the hallucination guard, which is a different and
// stronger claim: the id has to appear verbatim in text the USER wrote. If the model
// invents an id, provenance still kills it, truncated registry or not.
func TestTheHallucinationGuardStillFiresOnAPartialRegistry(t *testing.T) {
	partial := syncedRegistry(t, 20, true, "uhost-known-a")
	require.False(t, partial.CanAssertAbsence(), "precondition: the registry has no absence authority")

	// The model produced an id the user never typed.
	err := ValidateRoute(planTargetingID("uhost-invented", "uhost-invented"), ValidationContext{
		UserText: "帮我看看这台机器",
		Resolver: partial,
	})

	require.Error(t, err, "an id the user never wrote must still be rejected")
	var ve *ValidationError
	require.True(t, errorAsValidation(err, &ve))
	assert.Equal(t, ErrAttemptedHallucinatedEntity, ve.Code,
		"it must fail on PROVENANCE (the model made it up), not on registry membership — "+
			"relaxing absence must not open a hallucination hole")
}

// And the guard we kept: when the registry HAS seen the whole account, a user-typed id
// that genuinely is not there is still refused. Without this, the fix would degrade to
// "never check", which is not what was wrong.
func TestACompleteRegistryStillRefusesAnInstanceThatIsGenuinelyNotInTheAccount(t *testing.T) {
	complete := syncedRegistry(t, 2, false, "uhost-a", "uhost-b")
	require.True(t, complete.CanAssertAbsence(), "precondition: it fetched all 2 of 2")

	// The user typed it (provenance is clean) — it simply is not their instance.
	err := ValidateRoute(planTargetingID("uhost-someone-else", "uhost-someone-else"), ValidationContext{
		UserText: "uhost-someone-else 怎么样",
		Resolver: complete,
	})

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errorAsValidation(err, &ve))
	assert.Equal(t, ErrEntityNotFound, ve.Code,
		"a registry that has seen the entire account keeps its authority to say no")
}

// An unhydrated registry is the HTTP path's normal state on an early turn — engine.Init()
// is skipped there. It must abstain, not deny.
func TestAnUnhydratedRegistryAbstainsInsteadOfRefusing(t *testing.T) {
	empty := entity.NewRegistry() // never synced
	require.False(t, empty.CanAssertAbsence())

	err := ValidateRoute(planTargetingID("uhost-1exampleaa03", "uhost-1exampleaa03"), ValidationContext{
		UserText: "uhost-1exampleaa03这台实例不能用ncu啊，是H20显卡",
		Resolver: empty,
	})
	assert.NoError(t, err,
		"a registry that has never looked cannot conclude the instance is not there")
}
