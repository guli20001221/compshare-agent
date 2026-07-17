package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// draftContext is a create context sitting exactly where the confirm gate runs:
// the catalog and image queries have returned, nothing is resolved yet, and the
// user named only a GPU — so zone, CPU, memory, card count and image are all
// about to be auto-derived. That is the shape the seal used to be blind to.
func draftContext(zone string) *Context {
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询可用配比"] = zoneTaggedTypes(
		struct{ Name, Zone, Status string }{"4090", zone, "Normal"},
	)
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
	}}
	return wfCtx
}

// TestCreateDraftPutsAutoDerivedValuesInsideTheSeal is the structural fix.
//
// Context.seal hashes Params. When the user names no zone, Params carries no
// "Zone" key at all — so before the draft existed, the zone shown on the card was
// simply NOT in the sealed contract, and neither were CPU, memory, card count,
// image id, charge type, minimal CPU platform, disks or placement. The seal
// guaranteed nothing about any of them.
func TestCreateDraftPutsAutoDerivedValuesInsideTheSeal(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	require.NotContains(t, wfCtx.Params, "Zone", "the user named no zone — that is the whole point")

	card, err := buildCreateConfirmArgs(wfCtx)
	require.NoError(t, err)

	draft, ok := wfCtx.Params[createDraftKey].(map[string]any)
	require.True(t, ok, "the draft must live in Params, because Params is what seal() hashes")
	assert.Equal(t, "cn-sh2-02", draft["Zone"], "the auto-derived zone is now inside the sealed contract")
	assert.Equal(t, "img-001", draft["CompShareImageId"])
	assert.Equal(t, float64(16), draft["CPU"])
	assert.Equal(t, float64(64*1024), draft["Memory"])

	// And the card is a projection of that same draft, not a parallel derivation.
	assert.Equal(t, draft["Zone"], card["Zone"])
	assert.Equal(t, draft["CPU"], card["CPU"])
	assert.Equal(t, draft["Memory"], card["Memory"])
	assert.Equal(t, draft["GPU"], card["Gpu"])
}

// TestCreateExecutesTheConfirmedDraftNotAFreshDerivation is the decisive one: it
// fails on the pre-draft code.
//
// The old stepCreateInstance.BuildArgs called resolveTargetSpec a SECOND time,
// after the gate, reading "查询可用配比" again. The card and the create agreed only
// because that function is pure and its inputs happened to be frozen — an accident
// of the call graph, not a contract. Here the world moves after the user approves
// (the catalog re-homes 4090, the image query returns something else). A
// re-derivation would create in cn-wlcb-01 with img-999; the confirmed contract
// says cn-sh2-02 with img-001.
//
// The seal() call is load-bearing and was missing in this test's first version:
// without it the test proved only "a cached draft is reused", not "the CONFIRMED
// contract is executed" — a weaker claim than its own name made.
func TestCreateExecutesTheConfirmedDraftNotAFreshDerivation(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")

	card, err := buildCreateConfirmArgs(wfCtx)
	require.NoError(t, err)
	require.Equal(t, "cn-sh2-02", card["Zone"])
	wfCtx.seal("CreateInstanceWorkflow") // the user approved this card

	// Afterwards the catalog and the image query both move on.
	wfCtx.StepResults["查询可用配比"] = zoneTaggedTypes(
		struct{ Name, Zone, Status string }{"4090", "cn-wlcb-01", "Normal"},
	)
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-999-different", "Name": "TensorFlow"},
	}}

	args, err := createArgsFromSealedDraft(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", args["Zone"],
		"the create must execute the zone the user confirmed, not one re-derived from a catalog that has since changed")
	assert.Equal(t, "img-001", args["CompShareImageId"],
		"the create must execute the image the user confirmed, not a freshly re-picked one")
}

// TestCreateReadsTheSealedCopyNotTheLiveParams separates "a draft exists" from
// "the user confirmed it". Both a stale live draft and a moved world are present;
// only the frozen copy in sealed.BusinessParams is correct.
func TestCreateReadsTheSealedCopyNotTheLiveParams(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	_, err := buildCreateConfirmArgs(wfCtx)
	require.NoError(t, err)
	wfCtx.seal("CreateInstanceWorkflow")

	// Someone rewrites the LIVE draft after confirmation.
	wfCtx.Params[createDraftKey].(map[string]any)["Zone"] = "cn-wlcb-01"

	args, err := createArgsFromSealedDraft(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", args["Zone"],
		"must read the frozen copy in sealed.BusinessParams — Params is mutable and proves nothing about consent")

	// And the returned map must not alias the frozen record.
	args["Zone"] = "tampered"
	sealedDraft := wfCtx.sealed.BusinessParams[createDraftKey].(map[string]any)
	assert.Equal(t, "cn-sh2-02", sealedDraft["Zone"],
		"the executor must not be able to reach into the record the digest is computed over")
}

// TestCreateRefusesAMaterializedButUnconfirmedDraft is the structural guard.
//
// A draft in Params means only "someone computed one". If the create step is ever
// reached before its gate, it must stop — it cannot lean on verifySealedContract,
// which fails OPEN when sealed is nil and would wave an unconfirmed create
// straight through.
func TestCreateRefusesAMaterializedButUnconfirmedDraft(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	_, err := buildCreateConfirmArgs(wfCtx) // draft exists...
	require.NoError(t, err)
	require.Contains(t, wfCtx.Params, createDraftKey)
	require.Nil(t, wfCtx.sealed, "...but nothing confirmed it")

	_, err = createArgsFromSealedDraft(wfCtx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "拒绝以未经确认的参数创建")
}

// TestCreateRefusesWithoutAConfirmedDraft: no gate ran at all.
func TestCreateRefusesWithoutAConfirmedDraft(t *testing.T) {
	wfCtx := draftContext("cn-wlcb-01") // queries done, but no confirm ran

	_, err := createArgsFromSealedDraft(wfCtx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "拒绝以未经确认的参数创建")
}

// TestCreateDraftIsSealedAndTamperEvident closes the loop with the seal itself:
// once confirmed, rewriting a draft value must break the digest. Before the draft,
// this was unreachable — an auto-derived zone was not in Params, so no rewrite of
// it could ever be detected.
func TestCreateDraftIsSealedAndTamperEvident(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	_, err := buildCreateConfirmArgs(wfCtx)
	require.NoError(t, err)

	wfCtx.seal("CreateInstanceWorkflow")
	require.NotNil(t, wfCtx.sealed)
	require.True(t, wfCtx.sealed.verifyDigest(wfCtx.Params), "freshly sealed params must verify")

	draft := wfCtx.Params[createDraftKey].(map[string]any)
	draft["Zone"] = "cn-wlcb-01"

	assert.False(t, wfCtx.sealed.verifyDigest(wfCtx.Params),
		"rewriting a confirmed draft value must be detectable — that is what the seal is for")
}
