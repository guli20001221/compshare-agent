package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectImageCandidatesKeepsSystemImages(t *testing.T) {
	result := SelectImageCandidates(ImageSelectionInput{
		Images: []ImageCandidate{
			{ID: "img-ubuntu", Name: "Ubuntu 22.04", ImageType: ImageTypeSystem, Status: ImageStatusAvailable},
		},
		RequestedGPU: "4090",
		Zone:         ZoneConstraint{Zone: "cn-sh2-02"},
	})

	require.Len(t, result.Viable, 1)
	assert.Equal(t, "img-ubuntu", result.Viable[0].Image.ID)
	assert.Empty(t, result.Rejected)
}

func TestSelectImageCandidatesRejectsVMImagesInPodZone(t *testing.T) {
	result := SelectImageCandidates(ImageSelectionInput{
		Images: []ImageCandidate{
			{ID: "img-vm", Name: "Ubuntu 22.04", ImageType: ImageTypeSystem, Status: ImageStatusAvailable},
			{ID: "img-container", Name: "PyTorch", ImageType: ImageTypeApp, Container: true, Status: ImageStatusAvailable},
		},
		RequestedGPU: "4090",
		Zone:         ZoneConstraint{Zone: "cn-pod-01", IsPod: true},
	})

	require.Len(t, result.Viable, 1)
	assert.Equal(t, "img-container", result.Viable[0].Image.ID)
	require.Len(t, result.Rejected, 1)
	assert.Equal(t, "img-vm", result.Rejected[0].Image.ID)
	assert.Equal(t, RejectPodRequiresContainer, result.Rejected[0].Reason)
}

func TestSelectImageCandidatesRanksSupportedGPUWithoutHardRejectingMismatch(t *testing.T) {
	result := SelectImageCandidates(ImageSelectionInput{
		Images: []ImageCandidate{
			{ID: "img-a800", Name: "PyTorch A800", ImageType: ImageTypeApp, Status: ImageStatusAvailable, SupportedGPUTypes: []string{"A800"}},
			{ID: "img-4090", Name: "PyTorch 4090", ImageType: ImageTypeApp, Status: ImageStatusAvailable, SupportedGPUTypes: []string{"4090"}},
		},
		RequestedGPU: "4090",
		Zone:         ZoneConstraint{Zone: "cn-wlcb-01"},
	})

	require.Len(t, result.Viable, 2)
	assert.Equal(t, "img-4090", result.Viable[0].Image.ID)
	assert.Equal(t, "img-a800", result.Viable[1].Image.ID)
	require.Len(t, result.Viable[1].Warnings, 1)
	assert.Equal(t, WarningSupportedGPUMismatch, result.Viable[1].Warnings[0])
}

func TestNormalizeChargeTypeUsesExplicitPostpayDefault(t *testing.T) {
	assert.Equal(t, ChargeTypePostpay, NormalizeChargeType(""))
	assert.Equal(t, ChargeTypePostpay, NormalizeChargeType("Dynamic"))
	assert.Equal(t, ChargeTypeSpot, NormalizeChargeType("Spot"))
	assert.Equal(t, ChargeTypeMonth, NormalizeChargeType("Month"))
}

func TestBuildCapacityArgsUsesCreatePreflightCoreArgs(t *testing.T) {
	disks := []any{map[string]any{"IsBoot": true, "Type": "CLOUD_RSSD", "Size": 120}}
	args := BuildCapacityArgs(DeploymentDraft{
		Zone:               "cn-wlcb-01",
		GPUType:            "4090",
		CompShareImageID:   "img-pt",
		ChargeType:         "Spot",
		Disks:              disks,
		MinimalCPUPlatform: "Amd/Auto",
	})

	assert.Equal(t, "cn-wlcb-01", args["Zone"])
	assert.Equal(t, "4090", args["GpuType"])
	assert.Equal(t, "G", args["MachineType"])
	assert.Equal(t, "Amd/Auto", args["MinimalCpuPlatform"])
	assert.Equal(t, "img-pt", args["CompShareImageId"])
	assert.Equal(t, ChargeTypeSpot, args["ChargeType"])
	assert.Equal(t, disks, args["Disks"])
}

func TestBuildCapacityArgsOmitsDiskWhenUnknown(t *testing.T) {
	args := BuildCapacityArgs(DeploymentDraft{
		Zone:             "cn-wlcb-01",
		GPUType:          "4090",
		CompShareImageID: "img-pt",
	})

	assert.Equal(t, ChargeTypePostpay, args["ChargeType"])
	assert.Equal(t, "Auto", args["MinimalCpuPlatform"])
	assert.NotContains(t, args, "Disks")
}

func TestApplyCapacityPlacementArgsUsesOnlyZoneIDForPod(t *testing.T) {
	args := ApplyCapacityPlacementArgs(map[string]any{
		"Zone":     "cn-bj2-03",
		"Region":   "cn-bj2",
		"az_group": uint32(3003),
	}, ZonePlacement{
		Zone:    "cn-bj2-03",
		Region:  "cn-bj2",
		ZoneID:  9103,
		AzGroup: 3103,
		IsPod:   true,
	})

	assert.NotContains(t, args, "Zone")
	assert.NotContains(t, args, "Region")
	assert.NotContains(t, args, "az_group")
	assert.Equal(t, uint32(9103), args["zone_id"])
}

func TestApplyPlacementArgsKeepsNormalZoneAndRegion(t *testing.T) {
	placement := ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 2002, AzGroup: 3002}

	capacity := ApplyCapacityPlacementArgs(map[string]any{}, placement)
	assert.Equal(t, "cn-sh2-02", capacity["Zone"])
	assert.Equal(t, "cn-sh2", capacity["Region"])
	assert.Equal(t, uint32(2002), capacity["zone_id"])
	assert.NotContains(t, capacity, "az_group")

	purchase := ApplyPurchasePlacementArgs(map[string]any{}, placement)
	assert.Equal(t, "cn-sh2-02", purchase["Zone"])
	assert.Equal(t, "cn-sh2", purchase["Region"])
	assert.Equal(t, uint32(2002), purchase["zone_id"])
	assert.Equal(t, uint32(3002), purchase["az_group"])
}

func TestApplyPurchasePlacementArgsCarriesPodZoneIDAndAzGroup(t *testing.T) {
	args := ApplyPurchasePlacementArgs(map[string]any{
		"Zone":   "cn-bj2-03",
		"Region": "cn-bj2",
	}, ZonePlacement{
		Zone:    "cn-bj2-03",
		Region:  "cn-bj2",
		ZoneID:  9103,
		AzGroup: 3103,
		IsPod:   true,
	})

	assert.NotContains(t, args, "Zone")
	assert.NotContains(t, args, "Region")
	assert.Equal(t, uint32(9103), args["zone_id"])
	assert.Equal(t, uint32(3103), args["az_group"])
}

func TestClassifyCreateFailureRecognizesAdaptiveImageError(t *testing.T) {
	classified := ClassifyCreateFailure("ActionError: adaptive uhost image id is empty")

	assert.Equal(t, FailureImageZoneNotAdapted, classified.Kind)
	assert.True(t, classified.Recoverable)
}

func TestMemoryGBToMBMatchesUpstreamCreateUnits(t *testing.T) {
	assert.Equal(t, 65536, MemoryGBToMB(64))
}
