package workflow

import (
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullDraft is a draft with every field populated, including the ones that are
// optional upstream. A round-trip test whose fixture leaves fields zero cannot
// tell "carried correctly" from "dropped and re-defaulted to the same zero".
func fullDraft() CreateExecutionDraft {
	return CreateExecutionDraft{
		Args: CreateInstanceArgs{
			Zone:               "cn-sh2-02",
			GpuType:            "4090",
			GPU:                2,
			CPU:                32,
			Memory:             128 * 1024,
			CompShareImageID:   "img-001",
			ChargeType:         "Month",
			MachineType:        deployment.MachineTypeGPU,
			MinimalCPUPlatform: "Amd/Auto",
			LoginMode:          deployment.LoginModeConsole,
			Disks:              []any{map[string]any{"Name": "CLOUD_SSD", "Size": float64(100)}},
			Name:               "my-gpu-server",
		},
		Image:     SelectedImage{ID: "img-001", Name: "PyTorch", Source: "platform"},
		Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 2002, AzGroup: 3002, IsPod: true},
	}
}

// TestCreateDraftCodecRoundTrips is the contract the two-representation design
// rests on: encode → decode must return the same decision. If it does not, the
// card, the seal and the create are reading something the resolver did not say.
func TestCreateDraftCodecRoundTrips(t *testing.T) {
	original := fullDraft()

	back, err := ParseCreateExecutionDraft(original.ToContractMap())

	require.NoError(t, err)
	assert.Equal(t, original, back)
}

// TestCreateDraftCodecRoundTripsThroughJSON is the case that actually bites. A
// sealed contract can be marshalled and rehydrated, which turns every uint32 into
// a float64 and every concrete type into its JSON shadow. A codec that only
// survives an in-memory round-trip would decode ZoneID 2002 as 0 after a restart
// and create in a zone the user never approved.
func TestCreateDraftCodecRoundTripsThroughJSON(t *testing.T) {
	original := fullDraft()

	raw, err := json.Marshal(original.ToContractMap())
	require.NoError(t, err)
	var revived map[string]any
	require.NoError(t, json.Unmarshal(raw, &revived))

	back, err := ParseCreateExecutionDraft(revived)

	require.NoError(t, err)
	assert.Equal(t, original.Args.Zone, back.Args.Zone)
	assert.Equal(t, original.Args.GPU, back.Args.GPU)
	assert.Equal(t, original.Args.Memory, back.Args.Memory)
	assert.Equal(t, original.Args.Name, back.Args.Name)
	assert.Equal(t, original.Image, back.Image)
	assert.Equal(t, uint32(2002), back.Placement.ZoneID, "uint32 must survive returning as float64")
	assert.Equal(t, uint32(3002), back.Placement.AzGroup)
	assert.True(t, back.Placement.IsPod)
}

// TestCreateDraftEncodesOnlySealSafeValues enforces the rule that makes storing
// the encoded form correct in the first place: every value must be something
// deepCopyValue copies properly and paramsDigest can marshal. A struct or pointer
// smuggled in here would be copied shallowly by the seal — and because the
// original and the "copy" would then move together, verifyDigest would keep
// passing. The failure this prevents is silent, which is why it is a test and not
// a comment.
func TestCreateDraftEncodesOnlySealSafeValues(t *testing.T) {
	var walk func(t *testing.T, path string, v any)
	walk = func(t *testing.T, path string, v any) {
		switch tv := v.(type) {
		case nil, string, bool,
			float64, float32,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64:
			return
		case map[string]any:
			for k, inner := range tv {
				walk(t, path+"."+k, inner)
			}
		case []any:
			for i, inner := range tv {
				walk(t, path+"[]", inner)
				_ = i
			}
		default:
			t.Errorf("%s: encoded draft carries %T — only strings, bools, numbers, maps, slices and nil are seal-safe", path, v)
		}
	}
	walk(t, "draft", fullDraft().ToContractMap())
}

// TestParseCreateExecutionDraftRejectsMissingSections: a decode failure must be an
// error, never a zero draft. A zero CreateExecutionDraft is a perfectly valid Go
// value that would create nothing coherent — and createArgsFromSealedDraft would
// happily send it.
func TestParseCreateExecutionDraftRejectsMissingSections(t *testing.T) {
	complete := fullDraft().ToContractMap()

	for _, tc := range []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{"empty", func(m map[string]any) {
			for k := range m {
				delete(m, k)
			}
		}, "执行草稿为空"},
		{"no args", func(m map[string]any) { delete(m, draftKeyArgs) }, "缺少上游参数"},
		{"empty args", func(m map[string]any) { m[draftKeyArgs] = map[string]any{} }, "缺少上游参数"},
		{"args wrong type", func(m map[string]any) { m[draftKeyArgs] = "nope" }, "缺少上游参数"},
		{"no image", func(m map[string]any) { delete(m, draftKeyImage) }, "缺少镜像选择"},
		{"no placement", func(m map[string]any) { delete(m, draftKeyPlacement) }, "缺少可用区信息"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := deepCopyParams(complete)
			tc.mutate(m)

			_, err := ParseCreateExecutionDraft(m)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestParseCreateExecutionDraftKeepsOptionalFieldsAbsent: Disks and Name are
// omitted upstream when unset, and "omitted" is a different request from "empty".
// The codec must not invent them on the way back.
func TestParseCreateExecutionDraftKeepsOptionalFieldsAbsent(t *testing.T) {
	minimal := CreateExecutionDraft{
		Args:      CreateInstanceArgs{Zone: "cn-wlcb-01", GpuType: "4090", GPU: 1, CPU: 16, Memory: 65536, CompShareImageID: "img-001", ChargeType: "Postpay"},
		Image:     SelectedImage{ID: "img-001", Name: "PyTorch", Source: "platform"},
		Placement: deployment.ZonePlacement{Zone: "cn-wlcb-01", Region: "cn-wlcb"},
	}

	encoded := minimal.ToContractMap()
	args, _ := encoded[draftKeyArgs].(map[string]any)
	assert.NotContains(t, args, argsKeyDisks, "an unset Disks must not be encoded — upstream reads its absence as the platform default")
	assert.NotContains(t, args, argsKeyName)

	back, err := ParseCreateExecutionDraft(encoded)
	require.NoError(t, err)
	assert.Equal(t, minimal, back)

	assert.NotContains(t, back.UpstreamCreateArgs(), argsKeyDisks)
	assert.NotContains(t, back.UpstreamCreateArgs(), argsKeyName)
}

// TestUpstreamShapesDifferPerCall pins that one typed decision serves three
// differently-shaped requests, and that none of them leaks the others' fields.
func TestUpstreamShapesDifferPerCall(t *testing.T) {
	d := fullDraft() // IsPod placement

	create := d.UpstreamCreateArgs()
	capacity := d.UpstreamCapacityArgs()
	price := d.UpstreamPriceArgs()

	// Create carries the full request.
	assert.Equal(t, "cn-sh2-02", create[argsKeyZone])
	assert.Equal(t, float64(2), create[argsKeyGPU])
	assert.Equal(t, deployment.LoginModeConsole, create[argsKeyLoginMode])
	assert.Equal(t, uint32(3002), create["az_group"])

	// Capacity asks about placement the other way round, and never about sizing.
	assert.Equal(t, true, capacity["IsPod"])
	assert.NotContains(t, capacity, argsKeyZone, "a pod capacity check drops Zone...")
	assert.NotContains(t, capacity, "az_group")
	assert.Equal(t, uint32(2002), capacity["zone_id"])
	assert.NotContains(t, capacity, argsKeyGPU, "capacity does not take sizing")
	assert.NotContains(t, capacity, argsKeyCPU)

	// Price takes sizing but none of the create-only fields.
	assert.Equal(t, "cn-sh2-02", price[argsKeyZone], "...where a purchase keeps it")
	assert.Equal(t, float64(2), price[argsKeyGPU])
	assert.Equal(t, uint32(3002), price["az_group"])
	for _, k := range []string{argsKeyMachineType, argsKeyLoginMode, argsKeyMinimalCPUPlatform, argsKeyName} {
		assert.NotContains(t, price, k, "%s is a create argument; price was never given it", k)
		assert.Contains(t, create, k, "...and the create must still send it")
	}
}

// livePriceResponse is the REAL upstream shape, field for field:
// GetCompShareInstancePriceResponse returns amounts under "Instance" and carries
// no quote id, no validity and no currency. Every price fixture in this repo says
// "Price" — a key that appears in ZERO live captures — so a snapshot test written
// against fixtures would be testing a response upstream never sends.
//
// Source: eval/reports/real_cli_golden_doubao_lite_runner.md:49-119.
func livePriceResponse() map[string]any {
	return map[string]any{
		"Action": "GetCompShareInstancePriceResponse",
		"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": 1.58},
			map[string]any{"ChargeType": "Month", "Instance": 951.85},
		},
		"ListPriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": 1.66},
			map[string]any{"ChargeType": "Month", "Instance": 1001.95},
		},
		"RetCode":      float64(0),
		"request_uuid": "886d1c25-df7c-4d97-aee1-41c0da1a5ad1",
	}
}

// TestEstimatedPriceRecordsOnlyWhatUpstreamSaid is the anti-fabrication gate.
//
// Upstream cannot lock a price: the live response has no quote id, no validity and
// no currency. The snapshot must therefore say "estimate" and must not dress a
// correlation id up as a commercial one — request_uuid is SourceRequestID, which
// is all it is. Currency is absent rather than "CNY", because a structured
// currency field would be this repo asserting a platform fact it was never told;
// the ¥ on the card is a display convention and stays one.
func TestEstimatedPriceRecordsOnlyWhatUpstreamSaid(t *testing.T) {
	got := extractEstimatedPrice(livePriceResponse(), "Postpay")

	require.NotNil(t, got)
	assert.Equal(t, "Postpay", got.ChargeType)
	assert.Equal(t, 1.58, got.PayableAmount)
	require.NotNil(t, got.ListAmount)
	assert.Equal(t, 1.66, *got.ListAmount)
	assert.Equal(t, "¥1.58/小时（原价 ¥1.66）（预估）", got.DisplayText)
	assert.False(t, got.Locked, "upstream cannot hold a price — saying otherwise would invent a commitment")
	assert.Equal(t, "886d1c25-df7c-4d97-aee1-41c0da1a5ad1", got.SourceRequestID,
		"this is the response's request_uuid and nothing more; calling it a quote id would fabricate one")

	// And what the type does NOT carry is as load-bearing as what it does.
	encoded := CreateConfirmationSnapshot{EstimatedPrice: got}.ToContractMap()
	price, _ := encoded[snapshotKeyPrice].(map[string]any)
	require.NotEmpty(t, price)
	for _, invented := range []string{"currency", "Currency", "quote_id", "QuoteID", "expires_at", "valid_until"} {
		assert.NotContains(t, price, invented,
			"upstream returns no %s — encoding one would be this repo asserting a fact it was never told", invented)
	}
}

// TestEstimatedPriceIsAbsentWhenUpstreamQuotedNothing: no price is shown as no
// price. A zero would read as free.
func TestEstimatedPriceIsAbsentWhenUpstreamQuotedNothing(t *testing.T) {
	assert.Nil(t, extractEstimatedPrice(livePriceResponse(), "Spot"),
		"the live response quotes no Spot price here — inventing 0 would render as free")
	assert.Nil(t, extractEstimatedPrice(nil, "Postpay"))
	assert.Nil(t, extractEstimatedPrice(map[string]any{}, "Postpay"))

	// And the snapshot encodes/decodes that absence rather than a zero price.
	snapshot := CreateConfirmationSnapshot{Execution: fullDraft(), EstimatedPrice: nil}
	back, err := ParseCreateConfirmationSnapshot(snapshot.ToContractMap())
	require.NoError(t, err)
	assert.Nil(t, back.EstimatedPrice)
}

// TestConfirmationSnapshotRoundTrips, including through JSON: a sealed contract
// can be persisted and rehydrated, and an estimate that decodes wrong is an audit
// record that lies about what the user was shown.
func TestConfirmationSnapshotRoundTrips(t *testing.T) {
	list := 1.66
	original := CreateConfirmationSnapshot{
		Execution: fullDraft(),
		EstimatedPrice: &EstimatedPriceSnapshot{
			ChargeType: "Postpay", PayableAmount: 1.58, ListAmount: &list,
			DisplayText: "¥1.58/小时（原价 ¥1.66）（预估）", SourceRequestID: "req-1", Locked: false,
		},
	}

	back, err := ParseCreateConfirmationSnapshot(original.ToContractMap())
	require.NoError(t, err)
	assert.Equal(t, original.Execution, back.Execution)
	require.NotNil(t, back.EstimatedPrice)
	assert.Equal(t, *original.EstimatedPrice, *back.EstimatedPrice)

	raw, err := json.Marshal(original.ToContractMap())
	require.NoError(t, err)
	var revived map[string]any
	require.NoError(t, json.Unmarshal(raw, &revived))
	viaJSON, err := ParseCreateConfirmationSnapshot(revived)
	require.NoError(t, err)
	require.NotNil(t, viaJSON.EstimatedPrice)
	assert.Equal(t, *original.EstimatedPrice, *viaJSON.EstimatedPrice)
	assert.Equal(t, original.Execution.Placement.ZoneID, viaJSON.Execution.Placement.ZoneID)
}

// TestConfirmationSnapshotRejectsAMissingExecution: a snapshot without an
// execution is not a contract. A nil price is fine; a nil execution is not.
func TestConfirmationSnapshotRejectsAMissingExecution(t *testing.T) {
	_, err := ParseCreateConfirmationSnapshot(map[string]any{snapshotKeyPrice: map[string]any{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "缺少执行草稿")

	_, err = ParseCreateConfirmationSnapshot(map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "确认快照为空")
}

// TestSnapshotEncodesOnlySealSafeValues: same rule as the draft's, now including
// the price. *float64 must be dereferenced on the way out — a pointer in Params
// would be copied shallowly by the seal.
func TestSnapshotEncodesOnlySealSafeValues(t *testing.T) {
	list := 1.66
	encoded := CreateConfirmationSnapshot{
		Execution:      fullDraft(),
		EstimatedPrice: &EstimatedPriceSnapshot{ChargeType: "Postpay", PayableAmount: 1.58, ListAmount: &list},
	}.ToContractMap()

	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch tv := v.(type) {
		case nil, string, bool, float64, float32,
			int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return
		case map[string]any:
			for k, inner := range tv {
				walk(path+"."+k, inner)
			}
		case []any:
			for _, inner := range tv {
				walk(path+"[]", inner)
			}
		default:
			t.Errorf("%s: encoded snapshot carries %T — only strings, bools, numbers, maps, slices and nil are seal-safe", path, v)
		}
	}
	walk("snapshot", encoded)
}

// TestUpstreamArgsDoNotAliasTheDraft: every builder must produce a fresh map, or
// the tool executor could mutate the record the seal's digest is computed over.
func TestUpstreamArgsDoNotAliasTheDraft(t *testing.T) {
	d := fullDraft()

	a := d.UpstreamCreateArgs()
	a[argsKeyZone] = "TAMPERED"

	assert.Equal(t, "cn-sh2-02", d.Args.Zone)
	assert.Equal(t, "cn-sh2-02", d.UpstreamCreateArgs()[argsKeyZone])
}
