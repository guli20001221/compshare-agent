package workflow

import (
	"fmt"

	"github.com/compshare-agent/internal/deployment"
)

// CreateExecutionDraft is the typed business object for one resolved create: what
// WOULD be built, decided exactly once by the 形成执行草稿 resolve step and read by
// stock, price, the confirm card and — once sealed — the create itself.
//
// The type and the storage format are deliberately different things. Params and
// SealedActionContract.BusinessParams hold the ENCODED form (ToContractMap), not
// this struct, and that is a correctness requirement rather than a style choice:
//
//	deepCopyParams walks reflect Kinds and handles Map and Slice. A struct falls to
//	its default arm and is returned by value, which copies the header and SHARES
//	every inner map and slice. Storing this struct in Params directly would mean
//	promoteCreateDraft's deep copy and sealDraft's deep copy were both shallow for
//	Args.Disks and everything else behind a reference — so a later write through the
//	candidate would reach the sealed record. And because both sides move together,
//	verifyDigest would still PASS. Not a detectable break: an undetectable one.
//
// Encoding to plain JSON-shaped values instead keeps the seal layer receiving
// exactly what it already knows how to copy and hash, and leaves the shared
// deepCopyValue primitive — which every workflow's seal depends on — untouched.
//
// The codec is the only crossing. ToContractMap is the only encoder,
// ParseCreateExecutionDraft the only decoder, and no consumer may read the encoded
// map's keys directly; architectureguard enforces that, because two hand-written
// readings of one format is the same defect class this whole convergence removed.
type CreateExecutionDraft struct {
	Args      CreateInstanceArgs
	Image     SelectedImage
	Placement deployment.ZonePlacement
}

// EstimatedPriceSnapshot records what the user was quoted at the moment they were
// asked to approve a create — so the sealed contract can answer "what price did
// they see", not only "what did they configure".
//
// Every field is what upstream actually returns, and the absences are deliberate:
//
//   - There is NO quote id and NO validity, because GetCompShareInstanceUserPrice
//     has neither. Its live response carries only Action, PriceDetails,
//     ListPriceDetails, OriginalPriceDetails, RetCode and request_uuid. Naming a
//     request_uuid "QuoteID" would dress up an HTTP correlation id as a commercial
//     commitment nobody made; it is SourceRequestID, which is all it is.
//   - There is NO Currency, because upstream does not return one. The ¥ on the
//     card is a product display convention and stays a display concern; a
//     structured Currency:"CNY" here would be this repo asserting a platform fact
//     it was never told.
//   - Locked is always false and exists to say so out loud. Upstream cannot hold a
//     price, so this is an estimate, and the card says 预估 rather than implying a
//     commitment the platform has not made.
type EstimatedPriceSnapshot struct {
	ChargeType      string
	PayableAmount   float64
	ListAmount      *float64 // nil when upstream quoted no list/original price
	DisplayText     string
	SourceRequestID string
	Locked          bool
}

// CreateConfirmationSnapshot is what the user is actually shown and what the seal
// actually freezes: the resolved execution plus the estimate quoted for it.
//
// It exists because the draft is formed BEFORE the price is known — stock and
// price are asked about the draft — so the price cannot live on the draft itself.
// A second local computation joins them once both are in hand.
//
// The card renders this and PromoteOnConfirm seals this: one object, read twice.
// Rendering the price text at card time and rebuilding it at promote time would
// put us back to two computations agreeing by luck, which is the defect this whole
// convergence has been removing.
type CreateConfirmationSnapshot struct {
	Execution      CreateExecutionDraft
	EstimatedPrice *EstimatedPriceSnapshot // nil when upstream quoted nothing usable
}

// CreateInstanceArgs is the typed business half of a create request: the values a
// user decides, not the wire shape they are sent in.
//
// Placement is NOT among them. It lives on the draft beside these, because the
// three upstream calls flatten it differently (ApplyCapacityPlacementArgs drops
// Zone/Region/az_group for a pod zone where ApplyPurchasePlacementArgs keeps them),
// so a single flattened form cannot serve all three. Each request shape is built
// from the same typed decision instead.
type CreateInstanceArgs struct {
	Zone               string
	GpuType            string
	GPU                float64
	CPU                float64
	Memory             float64 // MB
	CompShareImageID   string
	ChargeType         string
	MachineType        string
	MinimalCPUPlatform string
	LoginMode          string
	Disks              []any
	Name               string // empty = platform default
}

// Encoded key names. They are private to the codec on purpose: every other reader
// goes through ParseCreateExecutionDraft, so these strings appear exactly twice in
// the package — once in ToContractMap, once in the parser.
const (
	draftKeyArgs      = "args"
	draftKeyImage     = "image"
	draftKeyPlacement = "placement"

	argsKeyZone               = "Zone"
	argsKeyGpuType            = "GpuType"
	argsKeyGPU                = "GPU"
	argsKeyCPU                = "CPU"
	argsKeyMemory             = "Memory"
	argsKeyImageID            = "CompShareImageId"
	argsKeyChargeType         = "ChargeType"
	argsKeyMachineType        = "MachineType"
	argsKeyMinimalCPUPlatform = "MinimalCpuPlatform"
	argsKeyLoginMode          = "LoginMode"
	argsKeyDisks              = "Disks"
	argsKeyName               = "Name"

	imageKeyID     = "id"
	imageKeyName   = "name"
	imageKeySource = "source"

	placementKeyZone    = "zone"
	placementKeyRegion  = "region"
	placementKeyZoneID  = "zone_id"
	placementKeyAzGroup = "az_group"
	placementKeyIsPod   = "is_pod"

	snapshotKeyExecution = "execution"
	snapshotKeyPrice     = "estimated_price"

	priceKeyChargeType      = "charge_type"
	priceKeyPayableAmount   = "payable_amount"
	priceKeyListAmount      = "list_amount"
	priceKeyDisplayText     = "display_text"
	priceKeySourceRequestID = "source_request_id"
	priceKeyLocked          = "locked"
)

// ToContractMap encodes the confirmation snapshot for storage and sealing. Same
// rule as the draft's: plain JSON shapes only.
func (s CreateConfirmationSnapshot) ToContractMap() map[string]any {
	out := map[string]any{
		snapshotKeyExecution: s.Execution.ToContractMap(),
	}
	if s.EstimatedPrice != nil {
		price := map[string]any{
			priceKeyChargeType:      s.EstimatedPrice.ChargeType,
			priceKeyPayableAmount:   s.EstimatedPrice.PayableAmount,
			priceKeyDisplayText:     s.EstimatedPrice.DisplayText,
			priceKeySourceRequestID: s.EstimatedPrice.SourceRequestID,
			priceKeyLocked:          s.EstimatedPrice.Locked,
		}
		// Absent, not zero: upstream quoting no list price is a different fact from
		// quoting a list price of 0, and only one of them means "no discount shown".
		if s.EstimatedPrice.ListAmount != nil {
			price[priceKeyListAmount] = *s.EstimatedPrice.ListAmount
		}
		out[snapshotKeyPrice] = price
	}
	return out
}

// ParseCreateConfirmationSnapshot decodes the stored confirmation snapshot.
//
// Two different things can be true of a snapshot with no usable price, and they
// must not decode alike:
//
//   - The price section is ABSENT. Legal. Upstream quoted nothing usable, so the
//     snapshot honestly records no price and the card shows none.
//   - The price section is PRESENT but incomplete. Corrupt. The only encoder is
//     ToContractMap, which writes all five keys or writes no section at all, so a
//     half-written section cannot have come from a real quote — something else
//     produced it. Decoding it leniently would fill the gaps with zero values and
//     hand back a record claiming the user was quoted ¥0.00, which is the exact
//     "absent must never read as free" rule the encoder goes out of its way to
//     honour. It fails instead.
func ParseCreateConfirmationSnapshot(m map[string]any) (CreateConfirmationSnapshot, error) {
	if len(m) == 0 {
		return CreateConfirmationSnapshot{}, fmt.Errorf("确认快照为空")
	}
	rawExec, ok := m[snapshotKeyExecution].(map[string]any)
	if !ok {
		return CreateConfirmationSnapshot{}, fmt.Errorf("确认快照缺少执行草稿")
	}
	exec, err := ParseCreateExecutionDraft(rawExec)
	if err != nil {
		return CreateConfirmationSnapshot{}, err
	}
	snapshot := CreateConfirmationSnapshot{Execution: exec}

	raw, present := m[snapshotKeyPrice]
	if !present {
		return snapshot, nil
	}
	rawPrice, ok := raw.(map[string]any)
	if !ok {
		return CreateConfirmationSnapshot{}, fmt.Errorf("确认快照的价格记录已损坏：%s 不是一条价格记录", snapshotKeyPrice)
	}
	price, err := parseEstimatedPrice(rawPrice)
	if err != nil {
		return CreateConfirmationSnapshot{}, err
	}
	snapshot.EstimatedPrice = price
	return snapshot, nil
}

// parseEstimatedPrice decodes a present price section, which is all-or-nothing.
//
// It checks PRESENCE and TYPE, never value. A payable amount of 0 is a real quote
// (upstream can price something at zero), Locked is false on every honest record,
// and SourceRequestID is empty whenever upstream returned no request_uuid — so a
// rule like "reject an empty/zero field" would reject records the encoder
// legitimately produces. What cannot legitimately happen is a MISSING or
// wrong-typed key, because ToContractMap writes all five unconditionally.
//
// That is the whole rule: this parser's contract is exactly its encoder's, which
// is why TestEveryKeyTheEncoderWritesIsRequiredBack derives the cases from
// ToContractMap's own output rather than from a hand-written list.
func parseEstimatedPrice(rawPrice map[string]any) (*EstimatedPriceSnapshot, error) {
	chargeType, ok := rawPrice[priceKeyChargeType].(string)
	if !ok {
		return nil, fmt.Errorf("确认快照的价格记录已损坏：缺少 %s", priceKeyChargeType)
	}
	payable, ok := priceNumber(rawPrice[priceKeyPayableAmount])
	if !ok {
		return nil, fmt.Errorf("确认快照的价格记录已损坏：缺少 %s", priceKeyPayableAmount)
	}
	displayText, ok := rawPrice[priceKeyDisplayText].(string)
	if !ok {
		return nil, fmt.Errorf("确认快照的价格记录已损坏：缺少 %s", priceKeyDisplayText)
	}
	sourceRequestID, ok := rawPrice[priceKeySourceRequestID].(string)
	if !ok {
		return nil, fmt.Errorf("确认快照的价格记录已损坏：缺少 %s", priceKeySourceRequestID)
	}
	locked, ok := rawPrice[priceKeyLocked].(bool)
	if !ok {
		return nil, fmt.Errorf("确认快照的价格记录已损坏：缺少 %s", priceKeyLocked)
	}
	price := &EstimatedPriceSnapshot{
		ChargeType:      chargeType,
		PayableAmount:   payable,
		DisplayText:     displayText,
		SourceRequestID: sourceRequestID,
		Locked:          locked,
	}
	// list_amount is the one optional key, so absence is legal here — but a key
	// that IS present and unreadable is corruption like any other, not a discount
	// to quietly drop.
	if rawList, present := rawPrice[priceKeyListAmount]; present {
		n, ok := priceNumber(rawList)
		if !ok {
			return nil, fmt.Errorf("确认快照的价格记录已损坏：%s 无法解析", priceKeyListAmount)
		}
		price.ListAmount = &n
	}
	return price, nil
}

// cloneDiskList returns a disk list that shares no structure with the one given.
//
// Disks is the only part of a draft that lives behind a reference. Every other
// field is a string, a number or a bool, which a plain struct copy already
// separates — so this one list is the whole of the draft's aliasing surface, and
// TestDisksAreTheOnlyAliasableFieldOnADraft fails if that ever stops being true.
//
// copy() or append() would not be enough: the elements are map[string]any, so
// copying the slice duplicates the interface headers while leaving every caller
// pointing at the same inner map. deepCopyValue recurses, which is what makes the
// copy real; it is the same primitive sealDraft already trusts for this data.
func cloneDiskList(disks []any) []any {
	if len(disks) == 0 {
		return nil
	}
	out := make([]any, len(disks))
	for i, disk := range disks {
		out[i] = deepCopyValue(disk)
	}
	return out
}

// ToContractMap encodes the draft into the plain JSON-shaped map that Params,
// StepResults and the sealed contract store.
//
// Every produced value is a string, bool, number, map, slice or nil — nothing that
// deepCopyValue would copy shallowly or that paramsDigest could not marshal
// deterministically. Adding a field here that does not satisfy that is how the
// seal silently stops meaning anything.
//
// The encoding shares no structure with the draft it came from. That is what makes
// "a fresh map every call" true all the way down rather than one level deep: the
// map was always fresh, but until the disks were cloned the list inside it was the
// draft's own, so two encodings of one draft were joined at the disks.
func (d CreateExecutionDraft) ToContractMap() map[string]any {
	args := map[string]any{
		argsKeyZone:               d.Args.Zone,
		argsKeyGpuType:            d.Args.GpuType,
		argsKeyGPU:                d.Args.GPU,
		argsKeyCPU:                d.Args.CPU,
		argsKeyMemory:             d.Args.Memory,
		argsKeyImageID:            d.Args.CompShareImageID,
		argsKeyChargeType:         d.Args.ChargeType,
		argsKeyMachineType:        d.Args.MachineType,
		argsKeyMinimalCPUPlatform: d.Args.MinimalCPUPlatform,
		argsKeyLoginMode:          d.Args.LoginMode,
	}
	// Absent, not empty: an omitted Disks/Name means "platform default" upstream,
	// which is a different request from one carrying an empty value.
	if len(d.Args.Disks) > 0 {
		args[argsKeyDisks] = cloneDiskList(d.Args.Disks)
	}
	if d.Args.Name != "" {
		args[argsKeyName] = d.Args.Name
	}
	return map[string]any{
		draftKeyArgs: args,
		draftKeyImage: map[string]any{
			imageKeyID:     d.Image.ID,
			imageKeyName:   d.Image.Name,
			imageKeySource: d.Image.Source,
		},
		draftKeyPlacement: map[string]any{
			placementKeyZone:    d.Placement.Zone,
			placementKeyRegion:  d.Placement.Region,
			placementKeyZoneID:  d.Placement.ZoneID,
			placementKeyAzGroup: d.Placement.AzGroup,
			placementKeyIsPod:   d.Placement.IsPod,
		},
	}
}

// ParseCreateExecutionDraft decodes the stored form back into the typed object.
//
// It is strict about structure and lenient about number types. Structure, because
// a missing args/image/placement section means the encoding is not a draft and
// guessing at it would hand the create step something nobody resolved. Number
// types, because an encoded draft may have been through JSON — nothing persists a
// sealed contract today, but paramsDigest marshals one and the decoder should not
// be the reason that stays impossible — and uint32 comes back as float64.
//
// The decoded draft shares no structure with the map it was decoded from. This is
// what keeps a request built from a SEALED draft off the sealed record itself:
// without the disk clone, UpstreamCreateArgs handed the executor the very list
// inside wfCtx.sealed, and a write through it would have rewritten the frozen
// record while verifyDigest — which hashes the live Params, not the frozen copy —
// went on passing. Undetectable, exactly as this type's doc comment warns.
func ParseCreateExecutionDraft(m map[string]any) (CreateExecutionDraft, error) {
	if len(m) == 0 {
		return CreateExecutionDraft{}, fmt.Errorf("执行草稿为空")
	}
	rawArgs, ok := m[draftKeyArgs].(map[string]any)
	if !ok || len(rawArgs) == 0 {
		return CreateExecutionDraft{}, fmt.Errorf("执行草稿缺少上游参数")
	}
	rawImage, ok := m[draftKeyImage].(map[string]any)
	if !ok {
		return CreateExecutionDraft{}, fmt.Errorf("执行草稿缺少镜像选择")
	}
	rawPlacement, ok := m[draftKeyPlacement].(map[string]any)
	if !ok {
		return CreateExecutionDraft{}, fmt.Errorf("执行草稿缺少可用区信息")
	}

	zoneID, _ := parseUint32Any(rawPlacement[placementKeyZoneID])
	azGroup, _ := parseUint32Any(rawPlacement[placementKeyAzGroup])
	isPod, _ := rawPlacement[placementKeyIsPod].(bool)
	disks, _ := rawArgs[argsKeyDisks].([]any)

	return CreateExecutionDraft{
		Args: CreateInstanceArgs{
			Zone:               paramStr(rawArgs, argsKeyZone, ""),
			GpuType:            paramStr(rawArgs, argsKeyGpuType, ""),
			GPU:                paramNum(rawArgs, argsKeyGPU, 0),
			CPU:                paramNum(rawArgs, argsKeyCPU, 0),
			Memory:             paramNum(rawArgs, argsKeyMemory, 0),
			CompShareImageID:   paramStr(rawArgs, argsKeyImageID, ""),
			ChargeType:         paramStr(rawArgs, argsKeyChargeType, ""),
			MachineType:        paramStr(rawArgs, argsKeyMachineType, ""),
			MinimalCPUPlatform: paramStr(rawArgs, argsKeyMinimalCPUPlatform, ""),
			LoginMode:          paramStr(rawArgs, argsKeyLoginMode, ""),
			Disks:              cloneDiskList(disks),
			Name:               paramStr(rawArgs, argsKeyName, ""),
		},
		Image: SelectedImage{
			ID:     paramStr(rawImage, imageKeyID, ""),
			Name:   paramStr(rawImage, imageKeyName, ""),
			Source: paramStr(rawImage, imageKeySource, ""),
		},
		Placement: deployment.ZonePlacement{
			Zone:    paramStr(rawPlacement, placementKeyZone, ""),
			Region:  paramStr(rawPlacement, placementKeyRegion, ""),
			ZoneID:  zoneID,
			AzGroup: azGroup,
			IsPod:   isPod,
		},
	}, nil
}

// UpstreamCreateArgs builds the CreateCompShareInstance request from the draft.
//
// Every value comes from a decision already recorded on the draft; this only
// chooses the wire shape, which is why it is safe to run after the seal. It makes
// no choice of its own — there is no catalog read, no default fill, no lookup.
//
// The request shares no structure with the draft, so an executor that normalises
// or rewrites the args it is handed cannot reach back into the decision. The draft
// this is called on may itself have been decoded from the sealed contract, which
// is what makes that a seal-integrity property and not merely tidy.
func (d CreateExecutionDraft) UpstreamCreateArgs() map[string]any {
	args := map[string]any{
		argsKeyZone:               d.Args.Zone,
		argsKeyGpuType:            d.Args.GpuType,
		argsKeyGPU:                d.Args.GPU,
		argsKeyCPU:                d.Args.CPU,
		argsKeyMemory:             d.Args.Memory,
		argsKeyImageID:            d.Args.CompShareImageID,
		argsKeyChargeType:         d.Args.ChargeType,
		argsKeyMachineType:        d.Args.MachineType,
		argsKeyMinimalCPUPlatform: d.Args.MinimalCPUPlatform,
		argsKeyLoginMode:          d.Args.LoginMode,
	}
	if len(d.Args.Disks) > 0 {
		args[argsKeyDisks] = cloneDiskList(d.Args.Disks)
	}
	if d.Args.Name != "" {
		args[argsKeyName] = d.Args.Name
	}
	return deployment.ApplyPurchasePlacementArgs(args, d.Placement)
}

// UpstreamCapacityArgs builds the CheckCompShareResourceCapacity request.
//
// Capacity is not a subset of the create request: it carries no GPU/CPU/Memory,
// and a pod zone flattens placement the other way (Zone/Region/az_group dropped).
// That is why the draft keeps a structured Placement rather than only a flattened
// argument map.
//
// The disks are cloned before they are handed over, not after: BuildCapacityArgs
// is shared with the deploy_model saga and assigns whatever slice it is given
// straight into the request map, so the isolation has to be established on this
// side of the call rather than asked of a function other callers also rely on.
func (d CreateExecutionDraft) UpstreamCapacityArgs() map[string]any {
	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:               d.Args.Zone,
		GPUType:            d.Args.GpuType,
		CompShareImageID:   d.Args.CompShareImageID,
		ChargeType:         d.Args.ChargeType,
		Disks:              cloneDiskList(d.Args.Disks),
		MinimalCPUPlatform: d.Args.MinimalCPUPlatform,
	})
	return deployment.ApplyCapacityPlacementArgs(args, d.Placement)
}

// UpstreamPriceArgs builds the GetCompShareInstanceUserPrice request.
//
// Deliberately narrower than the create request: the draft also carries
// MachineType / MinimalCpuPlatform / LoginMode / Name, which upstream pricing is
// not given today and must not start receiving as a side effect of the two sharing
// a draft. Sharing a resolution is not sharing a request shape.
func (d CreateExecutionDraft) UpstreamPriceArgs() map[string]any {
	args := map[string]any{
		argsKeyZone:       d.Args.Zone,
		argsKeyGpuType:    d.Args.GpuType,
		argsKeyGPU:        d.Args.GPU,
		argsKeyCPU:        d.Args.CPU,
		argsKeyMemory:     d.Args.Memory,
		argsKeyChargeType: d.Args.ChargeType,
		argsKeyImageID:    d.Args.CompShareImageID,
	}
	if len(d.Args.Disks) > 0 {
		args[argsKeyDisks] = cloneDiskList(d.Args.Disks)
	}
	return deployment.ApplyPurchasePlacementArgs(args, d.Placement)
}
