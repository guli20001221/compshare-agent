package capability

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests guard ONE rule, stated in entity.CanAssertAbsence and until now
// enforced nowhere: a registry without standing to assert absence must not turn
// a lookup into a refusal.
//
// It was violated on the name branch, and the failure was not theoretical.
// Live, 2026-07-29, N=5: 「host-不要删除验证七天回收 这台实例现在是什么状态」 — an
// instance that exists, uniquely named in the account, named by the user in the
// same sentence — answered 「暂时无法按这个名称定位到实例」 5/5, with ZERO upstream
// calls. The same instance addressed by id answered normally, and the same name
// against a warm registry resolved to exactly one match. The matcher was fine;
// the registry was empty and refused anyway.
//
// That empty registry is the PRODUCTION DEFAULT, not an edge case: the HTTP/WS
// path never calls engine.Init(), so nothing warms the registry for the life of
// the session.

const coldNameProbeID = "uhost-1t09vtnm0qyj"
const coldNameProbeName = "host-不要删除验证七天回收"

func coldNameTarget(name string) []platform.TargetRef {
	return []platform.TargetRef{{Type: platform.TargetRefName, Value: name, Source: platform.SourceUserText}}
}

// warmRegistrySnapshot is a complete, freshly-synced registry: it HAS standing
// to assert absence, so a miss against it is a fact about the account.
func warmRegistrySnapshot(t *testing.T, rows ...map[string]any) entity.RegistrySnapshot {
	t.Helper()
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(describeFixture(rows...), "test"))
	snap := reg.Snapshot()
	require.True(t, snap.CanAssertAbsenceAt(time.Now()),
		"fixture must model a registry that MAY assert absence, or the test proves nothing")
	return snap
}

// TestColdRegistryNameWarmsUpInsteadOfRefusing is the regression for the live
// 5/5 failure above. An id survives a cold registry by pass-through; a name
// cannot (DescribeCompShareInstance takes ids), so it must sync a listing and
// re-resolve rather than refuse.
func TestColdRegistryNameWarmsUpInsteadOfRefusing(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		resourceInfoAction: describeFixture(instanceRowMap(coldNameProbeID, coldNameProbeName, "Running")),
	}}

	result := runResource(t, exec, coldRegistrySnapshot(), ResourceInfoRequest{
		Targets: coldNameTarget(coldNameProbeName),
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status,
		"a uniquely-named instance upstream would have returned must not be refused because the local cache is empty")
	assert.Contains(t, result.Reply, coldNameProbeID)
	require.GreaterOrEqual(t, len(exec.calls), 2, "warm-up listing, then the point query")
	assert.Nil(t, exec.calls[0].args["UHostIds"], "the warm-up must list the account, not query an id it does not have")
	assert.Equal(t, []string{coldNameProbeID}, exec.calls[len(exec.calls)-1].args["UHostIds"],
		"the resolved name must reach upstream as the id it resolved to")
}

// Production cases 063/124: the model selected resource_info but mislabelled a
// real cpod ID / ingress hostname. The canonical transcript for 063 passed the
// exact cpod ID as `name`; 124 passed the whole ingress hostname as `filter`.
// The production HTTP registry starts cold, so the full account listing must
// recover the unique ID and drive a point query.
func TestColdRegistryWrappedInstanceIDMislabelledStillResolves(t *testing.T) {
	const shellID = "cpod-1uhoorruxg8r"
	const domainID = "cpod-1uilwcei63de"
	listing := describeFixture(
		instanceRowMap(shellID, "shell-instance", "Running"),
		instanceRowMap(domainID, "upload-instance", "Running"),
	)

	for _, tc := range []struct {
		name    string
		refType platform.TargetRefType
		value   string
		want    string
	}{
		{name: "063 exact cpod id mislabelled name", refType: platform.TargetRefName,
			value: shellID, want: shellID},
		{name: "124 access hostname mislabelled filter", refType: platform.TargetRefFilter,
			value: "8188-" + domainID + "-s1.pod.compshare.cn", want: domainID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &mapReadExec{results: map[string]map[string]any{resourceInfoAction: listing}}
			result := runResource(t, exec, coldRegistrySnapshot(), ResourceInfoRequest{
				Targets: []platform.TargetRef{{
					Type: tc.refType, Value: tc.value, Source: platform.SourceUserText,
				}},
			})

			require.Equal(t, platform.ReadStatusHandled, result.Status)
			require.GreaterOrEqual(t, len(exec.calls), 2, "full account listing, then exact point query")
			assert.Nil(t, exec.calls[0].args["UHostIds"])
			assert.Equal(t, []string{tc.want}, exec.calls[len(exec.calls)-1].args["UHostIds"])
			assert.Contains(t, result.Reply, tc.want)
		})
	}
}

// Recovery above is not a general reinterpretation of invalid filter syntax.
// Mixed targets retain the existing validation contract even when each token
// happens to resolve to a real account instance.
func TestInvalidFilterRecoveryRejectsMixedTargets(t *testing.T) {
	const id = "cpod-1uilwcei63de"
	exec := &mapReadExec{}
	resolver := warmRegistrySnapshot(t,
		instanceRowMap(id, "upload-instance", "Running"),
		instanceRowMap("cpod-second", "train-b", "Running"),
	)

	result := runResource(t, exec, resolver, ResourceInfoRequest{Targets: []platform.TargetRef{
		{Type: platform.TargetRefFilter, Value: "8188-" + id + "-s1.pod.compshare.cn", Source: platform.SourceUserText},
		{Type: platform.TargetRefName, Value: "train-b", Source: platform.SourceUserText},
	}})

	assert.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackValidation, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

// A malformed filter which merely equals an instance display name must not be
// silently converted into a name lookup. Recovery requires the resolved account
// ID itself to occur in the original value, as it does in cases 063/124.
func TestInvalidFilterRecoveryRejectsDisplayNameOnlyMatch(t *testing.T) {
	exec := &mapReadExec{}
	resolver := warmRegistrySnapshot(t,
		instanceRowMap("cpod-real-id", "not-filter-syntax", "Running"),
	)

	result := runResource(t, exec, resolver, ResourceInfoRequest{Targets: []platform.TargetRef{
		{Type: platform.TargetRefFilter, Value: "not-filter-syntax", Source: platform.SourceUserText},
	}})

	assert.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackValidation, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

// TestWarmRegistryMissStaysAuthoritativeAndSilent is the other half of the rule,
// and the guard against over-correcting it: a registry that HAS seen everything
// is allowed to say "not in your account", and must do so without spending an
// upstream call to second-guess itself.
func TestWarmRegistryMissStaysAuthoritativeAndSilent(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		resourceInfoAction: describeFixture(instanceRowMap("uhost-real", "train-real", "Running")),
	}}
	resolver := warmRegistrySnapshot(t, instanceRowMap("uhost-real", "train-real", "Running"))

	result := runResource(t, exec, resolver, ResourceInfoRequest{
		Targets: coldNameTarget("完全不存在的实例"),
	})

	assert.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Empty(t, exec.calls, "a complete registry needs no warm-up; absence is already a fact")
}

// TestWarmupFailureFallsBackToTheOriginalRefusal: the warm-up is an attempt to
// answer better. When upstream will not list, the turn must land exactly where
// it did before — an unresolved-target fallback the agent can explain — and not
// be upgraded into a read failure the user reads as "the platform is broken".
func TestWarmupFailureFallsBackToTheOriginalRefusal(t *testing.T) {
	exec := &mapReadExec{errs: map[string]error{resourceInfoAction: errors.New("upstream 503")}}

	result := runResource(t, exec, coldRegistrySnapshot(), ResourceInfoRequest{
		Targets: coldNameTarget(coldNameProbeName),
	})

	assert.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackUnresolvedTarget, result.FallbackReason)
	assert.Len(t, exec.calls, 1, "one attempt, no retry storm")
}

// TestTruncatedRegistryAmbiguityCollapsesAfterWarmup covers the third way a
// registry loses standing (CanAssertAbsence: never-synced / failed / TRUNCATED).
// A partial view makes a DIFFERENT wrong claim than a miss: the exact match is
// the row that got cut, so the surviving rows fuzzy-match and the user is asked
// "which one?" about an unambiguous name. A conflict is a refusal to answer too.
func TestTruncatedRegistryAmbiguityCollapsesAfterWarmup(t *testing.T) {
	visible := []map[string]any{
		instanceRowMap("uhost-backup", "train-a-backup", "Running"),
		instanceRowMap("uhost-old", "train-a-old", "Stopped"),
	}
	cut := instanceRowMap("uhost-exact", "train-a", "Running")

	truncatedFixture := describeFixture(visible...)
	truncatedFixture["TotalCount"] = float64(len(visible) + 1) // the exact match did not fit
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(truncatedFixture, "test"))
	truncated := reg.Snapshot()
	require.False(t, truncated.CanAssertAbsenceAt(time.Now()), "fixture must model a truncated listing")

	// Pre-condition, asserted so this cannot quietly stop testing the collapse:
	// on the partial view alone the name IS ambiguous.
	matches, res := truncated.ResolveByName("train-a")
	require.Equal(t, entity.ResolveAmbiguous, res.Status)
	require.Len(t, matches, 2)

	exec := &mapReadExec{results: map[string]map[string]any{
		resourceInfoAction: describeFixture(append(append([]map[string]any{}, visible...), cut)...),
	}}
	result := runResource(t, exec, truncated, ResourceInfoRequest{Targets: coldNameTarget("train-a")})

	require.Equal(t, platform.ReadStatusHandled, result.Status,
		"the full listing has one exact name match — asking the user to disambiguate it is a manufactured question")
	assert.Contains(t, result.Reply, "uhost-exact")
	assert.NotContains(t, result.Reply, "uhost-backup")
}

// TestColdRegistryNameReachesEveryCapability: the hole was in shared resolution,
// so the fix has to hold for every capability that resolves targets — including
// refund, which used to refuse a cold EXACT ID as well. Per-capability tests
// elsewhere cover their own rendering; this one asserts only that none of them
// answers "I cannot find it" about an instance the account has.
func TestColdRegistryNameReachesEveryCapability(t *testing.T) {
	listing := describeFixture(instanceRowMap(coldNameProbeID, coldNameProbeName, "Running"))
	nameRef := coldNameTarget(coldNameProbeName)

	cases := []struct {
		capability string
		run        func(exec *mapReadExec) ReadResult
	}{
		{"resource_info", func(exec *mapReadExec) ReadResult {
			return runResource(t, exec, coldRegistrySnapshot(), ResourceInfoRequest{Targets: nameRef})
		}},
		{"monitor", func(exec *mapReadExec) ReadResult {
			return runMonitorCurrent(t, exec, coldRegistrySnapshot(), "", MonitorCurrentRequest{Targets: nameRef})
		}},
		{"instance_access", func(exec *mapReadExec) ReadResult {
			return runInstanceAccess(t, exec, InstanceAccessRequest{Targets: nameRef, AccessType: accessTypeSSH})
		}},
		{"refund_estimate", func(exec *mapReadExec) ReadResult {
			return runRefund(t, exec, coldRegistrySnapshot(), RefundEstimateRequest{Targets: nameRef})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.capability, func(t *testing.T) {
			exec := &mapReadExec{results: map[string]map[string]any{
				resourceInfoAction: listing,
				monitorAction:      monitorFixture(coldNameProbeID),
				refundAction: {"RefundPriceSet": []any{
					map[string]any{"UHostId": coldNameProbeID, "RefundPrice": float64(12.5)},
				}},
			}}

			result := tc.run(exec)

			assert.NotEqual(t, platform.ReadStatusFallbackBeforeTool, result.Status,
				"%s refused before any upstream call: %q", tc.capability, result.Reply)
			assert.NotEqual(t, platform.ReadStatusConflict, result.Status,
				"%s asked to disambiguate a unique name: %q", tc.capability, result.Reply)
			assert.NotEmpty(t, exec.calls, "%s made no upstream call at all", tc.capability)
		})
	}
}

// TestRefundLabelsRowsFromTheResponseNotTheRegistry pins the premise that let
// refund join the rule above. Its old comment claimed the reply was labelled
// from pre-query snapshots, so a cold id had to be refused; in fact the rows are
// iterated from RefundPriceSet and snapshots only upgrade a label. With no
// snapshot the row still renders, keyed by id — degraded, never fabricated.
func TestRefundLabelsRowsFromTheResponseNotTheRegistry(t *testing.T) {
	withName := renderRefundEstimateReply(
		map[string]any{"RefundPriceSet": []any{map[string]any{"UHostId": "uhost-a", "RefundPrice": float64(12.5)}}},
		[]entity.InstanceSnapshot{{UHostId: "uhost-a", Name: "train-a"}})
	withoutName := renderRefundEstimateReply(
		map[string]any{"RefundPriceSet": []any{map[string]any{"UHostId": "uhost-a", "RefundPrice": float64(12.5)}}},
		nil)

	assert.Contains(t, withName, "train-a（uhost-a）")
	assert.Contains(t, withoutName, "uhost-a", "the row survives without a snapshot")
	assert.NotContains(t, withoutName, "train-a", "and invents no name it was never given")
	assert.Contains(t, withoutName, "12.50", "the price still comes from the response")
}

// TestWarmupSyncsTheSessionRegistry: the listing the warm-up paid for is handed
// to the session registry, so the SECOND name-addressed turn resolves from cache
// instead of re-listing. Without this the production path (Init() never runs)
// pays one extra full listing on every such turn, forever.
func TestWarmupSyncsTheSessionRegistry(t *testing.T) {
	session := entity.NewRegistry()
	require.False(t, session.CanAssertAbsence(), "session starts cold, as it does over HTTP")

	exec := &mapReadExec{results: map[string]map[string]any{
		resourceInfoAction: describeFixture(instanceRowMap(coldNameProbeID, coldNameProbeName, "Running")),
	}}
	reg := NewReadCapability(resourceReadSpec())
	result := reg.Run(context.Background(), ResourceInfoRequest{Targets: coldNameTarget(coldNameProbeName)},
		ReadRuntime{
			Executor:     exec,
			Resolver:     session.Snapshot(),
			SyncRegistry: func(raw map[string]any) { _ = session.SyncFromDescribe(raw, "test") },
		})
	require.Equal(t, platform.ReadStatusHandled, result.Status)

	matches, res := session.Snapshot().ResolveByName(coldNameProbeName)
	require.Equal(t, entity.ResolveHit, res.Status, "the next turn must resolve this name without another listing")
	require.Len(t, matches, 1)
	assert.Equal(t, coldNameProbeID, matches[0].UHostId)
}

// TestWarmRegistryNameStillResolvesExactly is the control that keeps the
// diagnosis honest: it separates "the registry was empty" from "name matching is
// broken". Seventeen instances share the prefix `host`, the real account's shape
// and the reason a fuzzy fallback could plausibly go ambiguous instead of hitting.
func TestWarmRegistryNameStillResolvesExactly(t *testing.T) {
	rows := []map[string]any{instanceRowMap(coldNameProbeID, coldNameProbeName, "Running")}
	for i := 0; i < 17; i++ {
		rows = append(rows, instanceRowMap(fmt.Sprintf("uhost-filler-%02d", i), "host", "Running"))
	}
	matches, res := warmRegistrySnapshot(t, rows...).ResolveByName(coldNameProbeName)

	require.Equal(t, entity.ResolveHit, res.Status)
	require.Len(t, matches, 1)
	assert.Equal(t, coldNameProbeID, matches[0].UHostId)
}
