package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewContext_IsolatesTopLevelParams pins P4 acceptance #1: after NewContext,
// mutating the caller's original map (the engine still owns ResolvedAction.
// Arguments) must not reach the workflow's business Params, and vice versa.
func TestNewContext_IsolatesTopLevelParams(t *testing.T) {
	original := map[string]any{"GpuType": "4090", "Zone": "cn-wlcb-01"}

	wfCtx := NewContext(original)

	// Caller mutates its own map after handing it off.
	original["GpuType"] = "A100"
	original["Injected"] = "x"
	assert.Equal(t, "4090", wfCtx.Params["GpuType"], "a later mutation of the caller's map must not reach sealed-side Params")
	_, injected := wfCtx.Params["Injected"]
	assert.False(t, injected, "a key added to the caller's map after seal must not appear in Params")

	// A step mutating Params must not reach the caller's map.
	wfCtx.Params["GpuType"] = "H20"
	assert.Equal(t, "A100", original["GpuType"], "a step mutating Params must not rewrite the caller's confirmed arguments")
}

// TestNewContext_IsolatesNestedMapsAndSlices pins P4 acceptance #2: a top-level
// copy is not a seal — ResolvedAction.Arguments carries structured values, so
// nested maps and slices must be cloned so a mutation cannot penetrate.
func TestNewContext_IsolatesNestedMapsAndSlices(t *testing.T) {
	original := map[string]any{
		"Disks":         []any{map[string]any{"Size": float64(200)}},
		"ZoneDescribes": map[string]string{"cn-wlcb-01": "华北一C"},
	}

	wfCtx := NewContext(original)

	// Mutate the nested slice element and nested map in the caller's copy.
	original["Disks"].([]any)[0].(map[string]any)["Size"] = float64(999)
	original["ZoneDescribes"].(map[string]string)["cn-wlcb-01"] = "TAMPERED"

	gotDisk := wfCtx.Params["Disks"].([]any)[0].(map[string]any)
	assert.Equal(t, float64(200), gotDisk["Size"], "mutating a nested slice element in the caller's map must not penetrate Params")
	gotZones := wfCtx.Params["ZoneDescribes"].(map[string]string)
	assert.Equal(t, "华北一C", gotZones["cn-wlcb-01"], "mutating a nested map in the caller's map must not penetrate Params")

	// And the reverse: mutating Params' nested containers must not reach the caller.
	wfCtx.Params["Disks"].([]any)[0].(map[string]any)["Size"] = float64(1)
	assert.Equal(t, float64(999), original["Disks"].([]any)[0].(map[string]any)["Size"],
		"mutating Params' nested slice must not reach the caller's map")
}

// TestNewContext_SplitsRuntimeMetadataOutOfBusinessParams pins P4 acceptance #8:
// server-injected identity is not a user business parameter — it must be lifted
// into Runtime and absent from the business Params (and therefore from the
// confirm form and the sealed digest).
func TestNewContext_SplitsRuntimeMetadataOutOfBusinessParams(t *testing.T) {
	wfCtx := NewContext(map[string]any{
		"GpuType":             "4090",
		"top_organization_id": uint32(101),
		"organization_id":     uint32(202),
	})

	_, hasTop := wfCtx.Params["top_organization_id"]
	_, hasOrg := wfCtx.Params["organization_id"]
	assert.False(t, hasTop, "org id must not remain in editable business params")
	assert.False(t, hasOrg, "org id must not remain in editable business params")
	assert.Equal(t, uint32(101), wfCtx.Runtime.TopOrganizationID)
	assert.Equal(t, uint32(202), wfCtx.Runtime.OrganizationID)
	assert.Equal(t, "4090", wfCtx.Params["GpuType"], "business params survive the split")
}

// TestSplitRuntimeMetadata_CoercesNumericForms proves identity coercion works for
// the uint32 the engine injects and the float64 a JSON-decoded arg would carry.
func TestSplitRuntimeMetadata_CoercesNumericForms(t *testing.T) {
	_, rt := splitRuntimeMetadata(map[string]any{
		"top_organization_id": float64(303),
		"organization_id":     uint32(404),
	})
	assert.Equal(t, uint32(303), rt.TopOrganizationID)
	assert.Equal(t, uint32(404), rt.OrganizationID)
}

// TestParamsDigest_DeterministicAndContentSensitive proves the digest is stable
// for equal content (map key order is irrelevant) and changes when a value
// changes — the property the seal's tamper check relies on.
func TestParamsDigest_DeterministicAndContentSensitive(t *testing.T) {
	a := map[string]any{"GpuType": "4090", "Zone": "cn-wlcb-01"}
	b := map[string]any{"Zone": "cn-wlcb-01", "GpuType": "4090"}
	assert.Equal(t, paramsDigest(a), paramsDigest(b), "digest must be independent of map key order")

	c := map[string]any{"GpuType": "A100", "Zone": "cn-wlcb-01"}
	assert.NotEqual(t, paramsDigest(a), paramsDigest(c), "digest must change when a business value changes")
}

// TestSealDraft_FreezesBusinessParams proves the sealed contract is an
// independent snapshot: mutating the draft's params after sealing does not
// change the sealed copy, and the sealed digest still verifies the frozen copy.
func TestSealDraft_FreezesBusinessParams(t *testing.T) {
	draft := ExecutionDraft{
		Operation:      "CreateInstanceWorkflow",
		BusinessParams: map[string]any{"GpuType": "4090", "Disks": []any{map[string]any{"Size": float64(200)}}},
		Runtime:        RuntimeMetadata{TopOrganizationID: 101},
	}

	sealed := sealDraft(draft)

	// Mutate the draft after sealing — sealed copy must be untouched.
	draft.BusinessParams["GpuType"] = "A100"
	draft.BusinessParams["Disks"].([]any)[0].(map[string]any)["Size"] = float64(999)

	assert.Equal(t, "4090", sealed.BusinessParams["GpuType"], "seal must freeze a deep copy")
	assert.Equal(t, float64(200), sealed.BusinessParams["Disks"].([]any)[0].(map[string]any)["Size"])
	assert.Equal(t, uint32(101), sealed.Runtime.TopOrganizationID)
	assert.Equal(t, currentContractVersion, sealed.Version)
	require.True(t, sealed.verifyDigest(sealed.BusinessParams), "the frozen copy must verify against its own digest")
}

// TestSealedContract_VerifyDigestDetectsMutation proves a post-seal rewrite of
// the business params is detectable — the basis for the write-time tamper gate.
func TestSealedContract_VerifyDigestDetectsMutation(t *testing.T) {
	sealed := sealDraft(ExecutionDraft{
		Operation:      "CreateInstanceWorkflow",
		BusinessParams: map[string]any{"GpuType": "4090", "Zone": "cn-wlcb-01"},
	})

	live := map[string]any{"GpuType": "4090", "Zone": "cn-wlcb-01"}
	require.True(t, sealed.verifyDigest(live), "an unchanged copy verifies")

	live["Zone"] = "cn-sh2-02"
	assert.False(t, sealed.verifyDigest(live), "a silently rewritten confirmed field must fail verification")
}
