package deployment

import (
	"sort"
	"strings"
)

const (
	ImageTypeSystem    = "System"
	ImageTypeApp       = "App"
	ImageTypeCustom    = "Custom"
	ImageTypeCommunity = "Community"

	ImageStatusAvailable = "Available"

	ChargeTypePostpay = "Postpay"
	ChargeTypeDay     = "Day"
	ChargeTypeMonth   = "Month"
	ChargeTypeSpot    = "Spot"

	MachineTypeGPU         = "G"
	MinimalCPUPlatformAuto = "Auto"
	LoginModeConsole       = "Password"

	RejectImageUnavailable     = "image_unavailable"
	RejectPodRequiresContainer = "pod_requires_container_image"

	WarningSupportedGPUMismatch = "supported_gpu_mismatch"

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

type ImageCandidate struct {
	ID                string
	Name              string
	ImageType         string
	Container         bool
	Status            string
	SupportedGPUTypes []string
}

type ImageSelectionInput struct {
	Images       []ImageCandidate
	RequestedGPU string
	Zone         ZoneConstraint
}

type ImageSelectionResult struct {
	Viable   []ViableImage
	Rejected []RejectedImage
}

type ViableImage struct {
	Image    ImageCandidate
	Warnings []string
	score    int
}

type RejectedImage struct {
	Image  ImageCandidate
	Reason string
}

type ClassifiedFailure struct {
	Kind        string
	Recoverable bool
}

func SelectImageCandidates(input ImageSelectionInput) ImageSelectionResult {
	result := ImageSelectionResult{}
	for _, img := range input.Images {
		if img.Status != "" && img.Status != ImageStatusAvailable {
			result.Rejected = append(result.Rejected, RejectedImage{Image: img, Reason: RejectImageUnavailable})
			continue
		}
		if input.Zone.IsPod && !img.Container {
			result.Rejected = append(result.Rejected, RejectedImage{Image: img, Reason: RejectPodRequiresContainer})
			continue
		}

		viable := ViableImage{Image: img, score: 50}
		if input.RequestedGPU != "" && len(img.SupportedGPUTypes) > 0 {
			if gpuListContains(img.SupportedGPUTypes, input.RequestedGPU) {
				viable.score = 100
			} else {
				viable.score = 10
				viable.Warnings = append(viable.Warnings, WarningSupportedGPUMismatch)
			}
		}
		result.Viable = append(result.Viable, viable)
	}

	sort.SliceStable(result.Viable, func(i, j int) bool {
		if result.Viable[i].score != result.Viable[j].score {
			return result.Viable[i].score > result.Viable[j].score
		}
		return false
	})
	return result
}

func NormalizeChargeType(chargeType string) string {
	switch strings.TrimSpace(chargeType) {
	case "", "Dynamic":
		return ChargeTypePostpay
	default:
		return chargeType
	}
}

// ExplicitChargeTypeFromPhrase maps a bounded product vocabulary from an exact
// user quote to the platform wire value. This is deliberately not a fuzzy
// classifier: provenance may skip the purchase-mode card only when the entire
// quoted phrase has one unambiguous meaning. Anything else stays an Agent
// inference and the user gets the card.
func ExplicitChargeTypeFromPhrase(phrase string) (string, bool) {
	normalized := strings.ToLower(strings.Join(strings.Fields(phrase), ""))
	switch normalized {
	case "按量", "按量付费", "按小时", "按小时付费", "后付费":
		return ChargeTypePostpay, true
	case "包日", "按日", "按天":
		return ChargeTypeDay, true
	case "包月", "按月":
		return ChargeTypeMonth, true
	case "抢占", "抢占式", "竞价", "竞价实例":
		return ChargeTypeSpot, true
	default:
		return "", false
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

func MemoryGBToMB(gb int) int {
	return gb * 1024
}

func gpuListContains(list []string, gpu string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(gpu)) {
			return true
		}
	}
	return false
}
