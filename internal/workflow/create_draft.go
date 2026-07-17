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
// A missing price section decodes to a nil EstimatedPrice rather than an error:
// upstream returning nothing usable is a real outcome (the card then shows no
// price), and it must not be confused with a corrupted contract.
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

	rawPrice, ok := m[snapshotKeyPrice].(map[string]any)
	if !ok {
		return snapshot, nil
	}
	price := &EstimatedPriceSnapshot{
		ChargeType:      paramStr(rawPrice, priceKeyChargeType, ""),
		PayableAmount:   paramNum(rawPrice, priceKeyPayableAmount, 0),
		DisplayText:     paramStr(rawPrice, priceKeyDisplayText, ""),
		SourceRequestID: paramStr(rawPrice, priceKeySourceRequestID, ""),
	}
	price.Locked, _ = rawPrice[priceKeyLocked].(bool)
	if raw, present := rawPrice[priceKeyListAmount]; present {
		if n, ok := priceNumber(raw); ok {
			price.ListAmount = &n
		}
	}
	snapshot.EstimatedPrice = price
	return snapshot, nil
}

// ToContractMap encodes the draft into the plain JSON-shaped map that Params,
// StepResults and the sealed contract store.
//
// Every produced value is a string, bool, number, map, slice or nil — nothing that
// deepCopyValue would copy shallowly or that paramsDigest could not marshal
// deterministically. Adding a field here that does not satisfy that is how the
// seal silently stops meaning anything.
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
		args[argsKeyDisks] = d.Args.Disks
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
// types, because an encoded draft may have been through JSON — a sealed contract
// can be persisted and rehydrated — and uint32 comes back as float64.
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
			Disks:              disks,
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
		args[argsKeyDisks] = d.Args.Disks
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
func (d CreateExecutionDraft) UpstreamCapacityArgs() map[string]any {
	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:               d.Args.Zone,
		GPUType:            d.Args.GpuType,
		CompShareImageID:   d.Args.CompShareImageID,
		ChargeType:         d.Args.ChargeType,
		Disks:              d.Args.Disks,
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
		args[argsKeyDisks] = d.Args.Disks
	}
	return deployment.ApplyPurchasePlacementArgs(args, d.Placement)
}
