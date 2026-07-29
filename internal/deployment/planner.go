package deployment

import (
	"sort"
	"strings"

	"github.com/compshare-agent/internal/platform"
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

var explicitChargeTypeVocabulary = []struct {
	Phrase string
	Value  string
}{
	{"按量付费", ChargeTypePostpay}, {"按小时付费", ChargeTypePostpay},
	{"按量", ChargeTypePostpay}, {"按小时", ChargeTypePostpay}, {"后付费", ChargeTypePostpay},
	{"包日", ChargeTypeDay}, {"按日", ChargeTypeDay}, {"按天", ChargeTypeDay},
	{"包月", ChargeTypeMonth}, {"按月", ChargeTypeMonth},
	// 抢占式 leads its group because it is the product's own label for the mode
	// (it is what the purchase-mode card shows), and ExplicitChargeTypePhrase
	// hands the first entry to user-facing copy. Order does not affect parsing:
	// ExplicitChargeTypeFromPhrase scans every entry and decides on the resolved
	// VALUE being unique, so this is presentation only.
	{"抢占式", ChargeTypeSpot}, {"竞价实例", ChargeTypeSpot},
	{"抢占", ChargeTypeSpot}, {"竞价", ChargeTypeSpot},
}

// ExplicitChargeTypePhrase returns the canonical phrase a user can type to ask
// for this purchase mode: the first vocabulary entry that maps to it.
//
// It exists so that UI copy suggesting "say X to change the billing mode" draws
// X from the same table the server parses. A hand-written suggestion can drift
// out of the vocabulary, and the failure is silent — the user retypes the
// request using the wording we printed, the phrase no longer resolves, and they
// get the purchase-mode card they were told they could skip.
func ExplicitChargeTypePhrase(value string) (string, bool) {
	for _, item := range explicitChargeTypeVocabulary {
		if item.Value == value {
			return item.Phrase, true
		}
	}
	return "", false
}

// ExplicitChargeTypeFromPhrase maps a bounded product vocabulary from a user
// quote to the platform wire value. This is deliberately not a fuzzy
// classifier: provenance may skip the purchase-mode card only when the quote
// names exactly one purchase mode. No product term, or two different ones,
// stays an Agent inference and the user gets the card.
//
// The vocabulary term must be a literal span of the quote rather than the whole
// quote, because where the quote ends is the Agent's choice and requiring
// equality made an explicit choice depend on that: live over 6 runs,
// "抢占式创建一台 4090" skipped the card only 2 times — the Agent quoted
// "抢占式创建" — against 6/6 for "按量". The failure was in the safe direction,
// an extra card rather than a wrong mode, but it made the skip unreliable.
//
// Enumerating the vocabulary in the tool schema instead was measured and is
// WORSE: the span check still requires the term the user actually typed, so an
// Agent offered every synonym picks 包日 for a user who wrote 按天 and loses the
// pin (live: day and month each missed once in 5 runs, against zero here).
//
// Uniqueness is judged on the resolved VALUE, not the phrase, so a quote
// spanning two entries that mean the same thing ("按量付费" also contains "按量")
// stays one unambiguous choice, while a quote spanning a comparison
// ("包月和按量") resolves to two and refuses.
func ExplicitChargeTypeFromPhrase(phrase string) (string, bool) {
	value := ""
	for _, item := range explicitChargeTypeVocabulary {
		// platform.ContainsLiteralSpan is the repo's single reviewed primitive
		// for "is this a literal span of that" (architectureguard/scanner.go
		// exempts it by name). Using it here keeps this a provenance check
		// rather than a new string-heuristic site, and it already applies the
		// shared whitespace/case folding.
		if !platform.ContainsLiteralSpan(phrase, item.Phrase) {
			continue
		}
		if value != "" && value != item.Value {
			return "", false
		}
		value = item.Value
	}
	return value, value != ""
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
