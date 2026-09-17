package workflow

import "github.com/compshare-agent/internal/deployment"

// buildImageCandidateSet is the parameter-map form of the image picker's
// candidate set, so a test can drive it without assembling a workflow Context.
// zoneIsPod is explicit because the production caller resolves it from the zone
// catalog rather than from the ZoneIsPod param.
func buildImageCandidateSet(params map[string]any, images map[string]any, gpuType string, taxonomy *deployment.ImageTaxonomy, zoneIsPod bool) imageCandidateSet {
	return buildImageCandidateSetForRequest(params, images, taxonomy, deployment.ImageRequest{
		Name:         paramStr(params, "ImageName", ""),
		RequestedGPU: gpuType,
		Zone:         deployment.ZoneConstraint{Zone: paramStr(params, "Zone", ""), IsPod: zoneIsPod},
	})
}

// guidedImageFormOptions is the parameter-map form of
// guidedImageFormOptionsForContext.
func guidedImageFormOptions(params map[string]any, images map[string]any, gpuType string, taxonomy *deployment.ImageTaxonomy, zoneIsPod bool) (string, []ConfirmFormOption, int) {
	if images == nil {
		return "", nil, 0
	}
	set := buildImageCandidateSet(params, images, gpuType, taxonomy, zoneIsPod)
	return guidedImageFormOptionsFromSet(params, images, gpuType, set)
}
