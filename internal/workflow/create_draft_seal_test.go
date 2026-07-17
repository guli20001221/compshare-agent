package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// draftContext is a create context sitting exactly where the resolve step runs:
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

// draftMockExecutor is createMockExecutor with the catalog homing 4090 in a
// NON-default zone, so a zone appearing in the contract can only have come from
// the resolution — never from the user, never from defaultZone.
func draftMockExecutor(zone string) *mockExecutor {
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = zoneTaggedTypes(
		struct{ Name, Zone, Status string }{"4090", zone, "Normal"},
	)
	return executor
}

// runDraftStep runs the create's resolve step through the engine's own
// runResolveStep — not a hand-rolled equivalent — so a unit test's context
// reaches the state the confirm gate actually sees, and so these tests inherit
// the step's no-Params-write check rather than assuming it.
func runDraftStep(t *testing.T, wfCtx *Context) CreateExecutionDraft {
	t.Helper()
	res := &Result{}
	outcome := (&Engine{}).runResolveStep(stepResolveCreateDraft(), 0, 1, wfCtx, res)
	require.Equal(t, toolStepOK, outcome, "resolve step failed: %s", res.Message)
	draft, err := candidateCreateDraft(wfCtx)
	require.NoError(t, err)
	return draft
}

// runConfirmationStep runs the second resolve step — the one that joins the draft
// with the price quoted for it. The card and the promote both read its product, so
// a context that has not run it is not at the gate yet.
func runConfirmationStep(t *testing.T, wfCtx *Context) CreateConfirmationSnapshot {
	t.Helper()
	res := &Result{}
	outcome := (&Engine{}).runResolveStep(stepResolveCreateConfirmation(), 0, 1, wfCtx, res)
	require.Equal(t, toolStepOK, outcome, "confirmation step failed: %s", res.Message)
	snapshot, err := candidateCreateConfirmation(wfCtx)
	require.NoError(t, err)
	return snapshot
}

// runToTheGate runs both resolve steps, leaving the context exactly where the
// confirm gate would find it.
func runToTheGate(t *testing.T, wfCtx *Context) CreateExecutionDraft {
	t.Helper()
	draft := runDraftStep(t, wfCtx)
	runConfirmationStep(t, wfCtx)
	return draft
}

// storedSnapshot returns the ENCODED candidate snapshot, for the few tests that
// must reach through the storage format on purpose (aliasing, tamper).
func storedSnapshot(wfCtx *Context) map[string]any {
	return wfCtx.Result(createConfirmationStepName)
}

// confirmAndSeal mirrors what the engine does the instant a gate passes:
// runConfirmStep calls the step's PromoteOnConfirm, then Run seals. That the
// create's gate really is wired to those is pinned end-to-end by
// TestCreateDraftPutsAutoDerivedValuesInsideTheSeal below; these two lines let
// the function-level tests reach the post-confirm state without a full run.
func confirmAndSeal(t *testing.T, wfCtx *Context) {
	t.Helper()
	require.NoError(t, promoteCreateDraft(wfCtx))
	wfCtx.seal("CreateInstanceWorkflow")
}

// TestCreateDraftPutsAutoDerivedValuesInsideTheSeal is the structural fix, and it
// runs the real workflow because every link in the chain is load-bearing: the
// resolve step must run, its candidate must be promoted into Params when the gate
// passes, and Run must seal what promote wrote. Any one of those missing and the
// confirmed contract would not describe what gets created.
//
// Context.seal hashes Params. When the user names no zone, Params carries no
// "Zone" key at all — so before the draft existed, the zone shown on the card was
// simply NOT in the sealed contract, and neither were CPU, memory, card count,
// image id, charge type, minimal CPU platform, disks or placement. The seal
// guaranteed nothing about any of them.
func TestCreateDraftPutsAutoDerivedValuesInsideTheSeal(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	var card map[string]any
	eng := NewEngine(executor, func(_ string, args map[string]any) bool {
		card = args
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.NotNil(t, result.Contract, "a passed gate must leave a sealed contract")

	// The user named no zone — that is the whole point. Any zone in this contract
	// arrived through the draft or not at all.
	require.NotContains(t, result.Contract.BusinessParams, "Zone")

	stored, ok := result.Contract.BusinessParams[createDraftKey].(map[string]any)
	require.True(t, ok, "the confirmed snapshot must be inside the SEALED business params")
	snapshot, err := ParseCreateConfirmationSnapshot(stored)
	require.NoError(t, err)
	draft := snapshot.Execution
	assert.Equal(t, "cn-sh2-02", draft.Args.Zone, "the auto-derived zone is now inside the sealed contract")
	assert.Equal(t, "img-001", draft.Args.CompShareImageID)
	assert.Equal(t, float64(16), draft.Args.CPU)
	assert.Equal(t, float64(64*1024), draft.Args.Memory)

	// And the card is a projection of that same draft, not a parallel derivation.
	require.NotNil(t, card)
	assert.Equal(t, draft.Args.Zone, card["Zone"])
	assert.Equal(t, draft.Args.CPU, card["CPU"])
	assert.Equal(t, draft.Args.Memory, card["Memory"])
	assert.Equal(t, draft.Args.GPU, card["Gpu"])
	// The name on the card and the id in the request are ONE selection.
	assert.Equal(t, "Ubuntu 22.04 CUDA 12", card["image"])
	assert.Equal(t, card["image"], draft.Image.Name)
	assert.Equal(t, draft.Args.CompShareImageID, draft.Image.ID)

	// What was sealed is what was sent.
	var created map[string]any
	for _, c := range executor.calls {
		if c.action == "CreateCompShareInstance" {
			created = c.args
		}
	}
	require.NotNil(t, created)
	assert.Equal(t, "cn-sh2-02", created["Zone"])
	assert.Equal(t, "img-001", created["CompShareImageId"])
}

// TestTheSealRecordsThePriceTheUserWasShown is the point of the snapshot.
//
// The contract could already prove what the user configured. It could not prove
// what they were QUOTED: the price lived only in StepResults["查询价格"], outside
// everything the digest covers. An audit could say "they approved 4090×1 in
// cn-sh2-02" and not "they approved it at ¥1.58/小时".
//
// The card and the seal read ONE object here. If the card rendered the price and
// promote rebuilt it, they would agree by luck — and the thing that eventually
// diverged would be the number the user believed they were agreeing to.
func TestTheSealRecordsThePriceTheUserWasShown(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	executor.results["GetCompShareInstanceUserPrice"] = livePriceResponse()
	var card map[string]any
	eng := NewEngine(executor, func(_ string, args map[string]any) bool {
		card = args
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.NotNil(t, result.Contract)

	stored, ok := result.Contract.BusinessParams[createDraftKey].(map[string]any)
	require.True(t, ok)
	snapshot, err := ParseCreateConfirmationSnapshot(stored)
	require.NoError(t, err)

	require.NotNil(t, snapshot.EstimatedPrice, "the quote the user approved must be inside the seal")
	assert.Equal(t, 1.58, snapshot.EstimatedPrice.PayableAmount)
	assert.Equal(t, "Postpay", snapshot.EstimatedPrice.ChargeType)
	assert.False(t, snapshot.EstimatedPrice.Locked)
	assert.Equal(t, "886d1c25-df7c-4d97-aee1-41c0da1a5ad1", snapshot.EstimatedPrice.SourceRequestID)

	// The sealed text and the shown text are the same string, not two renderings.
	require.NotNil(t, card)
	assert.Equal(t, snapshot.EstimatedPrice.DisplayText, card["price"])
	assert.Contains(t, card["price"], "预估")
	assert.Equal(t, "最终费用以实际创建和结算结果为准", card["PriceNote"])

	// And the estimate never reaches the API.
	var created map[string]any
	for _, c := range executor.calls {
		if c.action == "CreateCompShareInstance" {
			created = c.args
		}
	}
	require.NotNil(t, created)
	for _, k := range []string{"price", "PriceNote", "estimated_price", "PayableAmount"} {
		assert.NotContains(t, created, k, "the estimate is an audit record, not a create argument")
	}
}

// TestCreateDraftIsNotInParamsBeforeTheGatePasses separates the two facts the
// draft used to conflate. The resolve step runs BEFORE the confirm gate — on the
// guided path it runs while an earlier selection card's seal is still live — so
// its output may not touch Params. Only a passed gate promotes it.
//
// Without this, "the draft is in Params" would again mean nothing more than
// "someone computed one", which is exactly the reading createArgsFromSealedDraft
// exists to refuse.
func TestCreateDraftIsNotInParamsBeforeTheGatePasses(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	before := paramsDigest(wfCtx.Params)

	draft := runDraftStep(t, wfCtx)

	require.NotEmpty(t, draft, "the step must actually have produced a candidate...")
	assert.NotContains(t, wfCtx.Params, createDraftKey,
		"...and putting it in Params would claim the user agreed to it")
	assert.Equal(t, before, paramsDigest(wfCtx.Params),
		"a resolve step may not touch the params the user is being asked to confirm")

	// The confirmation step is a resolve step too, and is bound by the same rule.
	runConfirmationStep(t, wfCtx)
	assert.NotContains(t, wfCtx.Params, createDraftKey,
		"forming the confirmation snapshot is still not the user agreeing to it")
	assert.Equal(t, before, paramsDigest(wfCtx.Params))

	// Promotion is what crosses that line, and only a passed gate performs it.
	require.NoError(t, promoteCreateDraft(wfCtx))
	assert.Contains(t, wfCtx.Params, createDraftKey)
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
func TestCreateExecutesTheConfirmedDraftNotAFreshDerivation(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	runToTheGate(t, wfCtx)

	card, err := buildCreateConfirmArgs(wfCtx)
	require.NoError(t, err)
	require.Equal(t, "cn-sh2-02", card["Zone"])
	confirmAndSeal(t, wfCtx) // the user approved this card

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
	runToTheGate(t, wfCtx)
	confirmAndSeal(t, wfCtx)

	// Someone rewrites the LIVE draft after confirmation, reaching through the
	// encoded form the way a careless future caller would.
	liveArgs := wfCtx.Params[createDraftKey].(map[string]any)[snapshotKeyExecution].(map[string]any)[draftKeyArgs].(map[string]any)
	liveArgs[argsKeyZone] = "cn-wlcb-01"

	args, err := createArgsFromSealedDraft(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", args[argsKeyZone],
		"must read the frozen copy in sealed.BusinessParams — Params is mutable and proves nothing about consent")

	// And the returned map must not alias the frozen record.
	args[argsKeyZone] = "tampered"
	sealedArgs := wfCtx.sealed.BusinessParams[createDraftKey].(map[string]any)[snapshotKeyExecution].(map[string]any)[draftKeyArgs].(map[string]any)
	assert.Equal(t, "cn-sh2-02", sealedArgs[argsKeyZone],
		"the executor must not be able to reach into the record the digest is computed over")
}

// TestPromotedDraftDoesNotAliasTheCandidate: promote re-encodes, so Params never
// aliases StepResults. If it did, a later write to either would diverge the live
// params from the digest and fail-stop a create the user correctly approved.
//
// This test is also the tripwire for storing the draft as a STRUCT in Params:
// deepCopyParams copies a struct by value and shares its inner maps, so the
// "copy" and the original would move together — and verifyDigest would keep
// passing while the sealed record was rewritten underneath it.
func TestPromotedDraftDoesNotAliasTheCandidate(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	runToTheGate(t, wfCtx)
	confirmAndSeal(t, wfCtx)

	candidateArgs := storedSnapshot(wfCtx)[snapshotKeyExecution].(map[string]any)[draftKeyArgs].(map[string]any)
	candidateArgs[argsKeyZone] = "cn-wlcb-01"

	assert.True(t, wfCtx.sealed.verifyDigest(wfCtx.Params),
		"rewriting the candidate must not disturb the promoted copy the digest covers")
	args, err := createArgsFromSealedDraft(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", args["Zone"])
}

// TestCreateCardNameAndExecutedIDAreOneSelection is the typed-image contract. The
// card used to render pickImageName while the create sent pickImageId — two walks
// of the same response. For a THREADED id they could genuinely disagree: the name
// shown was whatever ImageName travelled alongside, so a stale name could be
// displayed over a different image's id. Now the catalog's own name for that id
// wins.
func TestCreateCardNameAndExecutedIDAreOneSelection(t *testing.T) {
	wfCtx := draftContext("cn-wlcb-01")
	// Caller threads an explicit id, paired with a name that no longer describes it.
	wfCtx.Params["CompShareImageId"] = "img-002"
	wfCtx.Params["ImageName"] = "陈旧的名字"
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		map[string]any{"CompShareImageId": "img-002", "Name": "TensorFlow"},
	}}

	runToTheGate(t, wfCtx)
	card, err := buildCreateConfirmArgs(wfCtx)
	require.NoError(t, err)
	confirmAndSeal(t, wfCtx)
	args, err := createArgsFromSealedDraft(wfCtx)
	require.NoError(t, err)

	assert.Equal(t, "img-002", args["CompShareImageId"])
	assert.Equal(t, "TensorFlow", card["image"],
		"the card must name the image that will actually be built, not the stale name threaded beside its id")
	assert.NotEqual(t, "陈旧的名字", card["image"])
}

// TestThreadedCommunityIdResolvesNameFromAnyGroup: an id the user picked from the
// SECOND group is in the catalog, so its name must come from the catalog.
//
// The first version of catalogImageName grew a private community lookup off
// selectCommunityImage, which only reads groups[0]. Any id past the first group was
// therefore treated as "not in the catalog" and fell back to the threaded name —
// the exact failure the lookup exists to prevent, reintroduced by writing a second
// implementation instead of reusing imageNameByID (which already walks every group).
func TestThreadedCommunityIdResolvesNameFromAnyGroup(t *testing.T) {
	result := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "社区-第一组", "Data": []any{map[string]any{"CompShareImageId": "cimg-001"}}},
		map[string]any{"ImageName": "社区-第二组", "Data": []any{map[string]any{"CompShareImageId": "cimg-002"}}},
		map[string]any{"ImageName": "社区-第三组", "Data": []any{map[string]any{"CompShareImageId": "cimg-003"}}},
	}}
	params := map[string]any{
		"ImageSource": "community", "CompShareImageId": "cimg-003", "ImageName": "陈旧的名字",
	}

	selected := selectCreateImage(params, result)

	assert.Equal(t, "cimg-003", selected.ID)
	assert.Equal(t, "社区-第三组", selected.Name,
		"the id IS in the catalog — only groups[0] blindness could make this fall back to the threaded name")
}

// TestCommunityImageIdAndNameComeFromTheSameGroup: the two community pickers read
// different LEVELS of the response — the id from groups[0].Data[0], the name from
// groups[0].ImageName. One selection now reads both off the same group.
func TestCommunityImageIdAndNameComeFromTheSameGroup(t *testing.T) {
	result := map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"ImageName": "社区-ComfyUI",
			"Data":      []any{map[string]any{"CompShareImageId": "cimg-001"}},
		},
		map[string]any{
			"ImageName": "社区-另一个",
			"Data":      []any{map[string]any{"CompShareImageId": "cimg-999"}},
		},
	}}

	selected := selectCreateImage(map[string]any{"ImageSource": "community"}, result)

	assert.Equal(t, "cimg-001", selected.ID)
	assert.Equal(t, "社区-ComfyUI", selected.Name, "name and id must come from the same group")
	assert.Equal(t, "community", selected.Source)
}

// TestCreateRefusesAPromotedButUnconfirmedDraft is the structural guard.
//
// A draft in Params means only "someone put one there". If the create step is ever
// reached before its gate, it must stop — it cannot lean on verifySealedContract,
// which fails OPEN when sealed is nil and would wave an unconfirmed create
// straight through.
func TestCreateRefusesAPromotedButUnconfirmedDraft(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	runToTheGate(t, wfCtx)
	require.NoError(t, promoteCreateDraft(wfCtx)) // draft is in Params...
	require.Contains(t, wfCtx.Params, createDraftKey)
	require.Nil(t, wfCtx.sealed, "...but nothing sealed it")

	_, err := createArgsFromSealedDraft(wfCtx)

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
	runToTheGate(t, wfCtx)
	confirmAndSeal(t, wfCtx)
	require.NotNil(t, wfCtx.sealed)
	require.True(t, wfCtx.sealed.verifyDigest(wfCtx.Params), "freshly sealed params must verify")

	draft := wfCtx.Params[createDraftKey].(map[string]any)
	draft["Zone"] = "cn-wlcb-01"

	assert.False(t, wfCtx.sealed.verifyDigest(wfCtx.Params),
		"rewriting a confirmed draft value must be detectable — that is what the seal is for")
}
