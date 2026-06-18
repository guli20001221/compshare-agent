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
	ChargeTypeMonth   = "Month"

	RejectImageUnavailable     = "image_unavailable"
	RejectPodRequiresContainer = "pod_requires_container_image"

	WarningSupportedGPUMismatch = "supported_gpu_mismatch"

	FailureUnknown             = "unknown"
	FailureImageZoneNotAdapted = "image_zone_not_adapted"
	FailureCapacityNotEnough   = "capacity_not_enough"
)

var DefaultSystemDisk = []any{
	map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": 60},
}

type DeploymentDraft struct {
	Zone             string
	GPUType          string
	CompShareImageID string
	ChargeType       string
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

func BuildCapacityArgs(draft DeploymentDraft) map[string]any {
	return map[string]any{
		"Zone":               draft.Zone,
		"GpuType":            draft.GPUType,
		"MachineType":        "G",
		"MinimalCpuPlatform": "Auto",
		"CompShareImageId":   draft.CompShareImageID,
		"ChargeType":         NormalizeChargeType(draft.ChargeType),
		"Disks":              DefaultSystemDisk,
	}
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
