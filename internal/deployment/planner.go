package deployment

import "strings"

const (
	ImageTypeSystem    = "System"
	ImageTypeApp       = "App"
	ImageTypeCustom    = "Custom"
	ImageTypeCommunity = "Community"

	ImageStatusAvailable = "Available"
	ImageStatusReviewing = "Reviewing"

	ChargeTypePostpay = "Postpay"
	ChargeTypeDay     = "Day"
	ChargeTypeMonth   = "Month"
	ChargeTypeSpot    = "Spot"

	MachineTypeGPU         = "G"
	MinimalCPUPlatformAuto = "Auto"
	LoginModeConsole       = "Password"

	FailureUnknown             = "unknown"
	FailureImageZoneNotAdapted = "image_zone_not_adapted"
	FailureCapacityNotEnough   = "capacity_not_enough"
)

type DeploymentDraft struct {
	Zone               string
	GPUType            string
	CompShareImageID   string
	ChargeType         string
	Disks              []any
	MinimalCPUPlatform string
}

type ZonePlacement struct {
	Zone    string
	Region  string
	ZoneID  uint32
	AzGroup uint32
	IsPod   bool
}

type ZoneConstraint struct {
	Zone  string
	IsPod bool
}

type ClassifiedFailure struct {
	Kind        string
	Recoverable bool
}

func NormalizeChargeType(chargeType string) string {
	switch strings.TrimSpace(chargeType) {
	case "", "Dynamic":
		return ChargeTypePostpay
	default:
		return chargeType
	}
}

func BuildCapacityArgs(draft DeploymentDraft) map[string]any {
	args := map[string]any{
		"Zone":               draft.Zone,
		"GpuType":            draft.GPUType,
		"MachineType":        MachineTypeGPU,
		"MinimalCpuPlatform": MinimalCPUPlatform(draft.MinimalCPUPlatform),
		"CompShareImageId":   draft.CompShareImageID,
		"ChargeType":         NormalizeChargeType(draft.ChargeType),
	}
	if len(draft.Disks) > 0 {
		args["Disks"] = draft.Disks
	}
	return args
}

func MinimalCPUPlatform(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return MinimalCPUPlatformAuto
	}
	return value
}

func ApplyCapacityPlacementArgs(args map[string]any, placement ZonePlacement) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	if placement.IsPod {
		delete(args, "Zone")
		delete(args, "Region")
		delete(args, "az_group")
		args["IsPod"] = true
		if placement.ZoneID != 0 {
			args["zone_id"] = placement.ZoneID
		}
		return args
	}
	delete(args, "IsPod")
	if placement.Zone != "" {
		args["Zone"] = placement.Zone
	}
	if placement.Region != "" {
		args["Region"] = placement.Region
	}
	if placement.ZoneID != 0 {
		args["zone_id"] = placement.ZoneID
	}
	return args
}

func ApplyPurchasePlacementArgs(args map[string]any, placement ZonePlacement) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	if placement.IsPod {
		args["IsPod"] = true
		if placement.Zone != "" {
			args["Zone"] = placement.Zone
		}
		if placement.Region != "" {
			args["Region"] = placement.Region
		}
		if placement.ZoneID != 0 {
			args["zone_id"] = placement.ZoneID
		}
		if placement.AzGroup != 0 {
			args["az_group"] = placement.AzGroup
		}
		return args
	}
	delete(args, "IsPod")
	if placement.Zone != "" {
		args["Zone"] = placement.Zone
	}
	if placement.Region != "" {
		args["Region"] = placement.Region
	}
	if placement.ZoneID != 0 {
		args["zone_id"] = placement.ZoneID
	}
	if placement.AzGroup != 0 {
		args["az_group"] = placement.AzGroup
	}
	return args
}

func ClassifyCreateFailure(message string) ClassifiedFailure {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "adaptive uhost image id is empty"):
		return ClassifiedFailure{Kind: FailureImageZoneNotAdapted, Recoverable: true}
	case strings.Contains(lower, "resourcenotenough") || strings.Contains(lower, "resource not enough"):
		return ClassifiedFailure{Kind: FailureCapacityNotEnough, Recoverable: true}
	default:
		return ClassifiedFailure{Kind: FailureUnknown}
	}
}
