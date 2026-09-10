package workflow

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// The plain (non-guided) confirmation card's option lists, and the image
// lookups both cards share. No stock claim is attached here — stock is only
// asserted by the post-edit capacity re-run, so an option list can never
// promise availability the create then fails to find.

// gpuFormOptions lists selectable GPU types: sellable types from the catalog,
// filtered to the current image's SupportedGpuTypes when declared, current
// type first. No stock claim is attached — stock is only asserted by the
// post-edit 检查库存 re-run.
func gpuFormOptions(catalog map[string]any, supported []string, current string) []ConfirmFormOption {
	if catalog == nil {
		return nil
	}
	opts := []ConfirmFormOption{{Value: current, Label: gpuOptionLabel(catalog, current)}}
	seen := map[string]bool{current: true}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		if len(opts) >= maxFormGPUOptions {
			break
		}
		mt, _ := t.(map[string]any)
		name, _ := mt["Name"].(string)
		if name == "" || seen[name] {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && status != "Normal" {
			continue
		}
		if len(supported) > 0 && !containsFold(supported, name) {
			continue
		}
		seen[name] = true
		opts = append(opts, ConfirmFormOption{Value: name, Label: gpuOptionLabel(catalog, name)})
	}
	return opts
}

// gpuOptionLabel renders "4090（24G显存）" when the catalog carries VRAM info,
// else just the type name.
func gpuOptionLabel(catalog map[string]any, gpuType string) string {
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		gm, _ := mt["GraphicsMemory"].(map[string]any)
		if v, _ := gm["Value"].(float64); v > 0 {
			return fmt.Sprintf("%s（%.0fG显存）", gpuType, v)
		}
		break
	}
	return gpuType
}

// zoneFormOptions builds the confirm-card zone selector, listing the zones the
// current GPU type is sellable in with the current zone first. Each option's
// display label comes from the turn snapshot (via zoneDisplayLabel) — the single
// zone authority — degrading to the bare zone id when no label is known.
func zoneFormOptions(wfCtx *Context, catalog map[string]any, gpuType, current string) []ConfirmFormOption {
	if catalog == nil {
		return nil
	}
	opts := []ConfirmFormOption{{Value: current, Label: zoneDisplayLabel(wfCtx, current)}}
	seen := map[string]bool{current: true}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		z, _ := mt["Zone"].(string)
		if z == "" || seen[z] {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && status != "Normal" {
			continue
		}
		seen[z] = true
		opts = append(opts, ConfirmFormOption{Value: z, Label: zoneDisplayLabel(wfCtx, z)})
	}
	return opts
}

// zoneDisplayLabel is the display-lenient view of workflowZoneEntry: it shows the
// zone's console name, degrading to the bare zone id on any resolution failure
// (an option a form should not have offered — see the Entry gate). It never
// fails, because a label is display-only.
func zoneDisplayLabel(wfCtx *Context, zone string) string {
	if entry, err := workflowZoneEntry(wfCtx, zone); err == nil && strings.TrimSpace(entry.DisplayName) != "" {
		return entry.DisplayName
	}
	return zone
}

// imageFormOptions returns the currently selected image id plus up to
// maxFormImageOptions recommendations from the queried source (no cross-source
// mixing), filtered to images that declare support for the current GPU type
// (or declare no constraint). The current selection is always first.
func imageFormOptions(params map[string]any, images map[string]any, gpuType string, taxonomy *deployment.ImageTaxonomy) (string, []ConfirmFormOption) {
	current := pickImageId(params, images)
	if current == "" || images == nil {
		return "", nil
	}
	snap := formImageCatalog(images, paramStr(params, "ImageSource", "platform"))
	zoneIsPod := paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false)
	ranked := deployment.RankImages(snap, deployment.ImageRequest{
		Name:         paramStr(params, "ImageName", ""),
		RequestedGPU: gpuType,
		Zone:         deployment.ZoneConstraint{Zone: paramStr(params, "Zone", ""), IsPod: zoneIsPod},
	})
	// Narrow by the optional ImageType / ImageTag facets (unset facet = no filter).
	ranked = filterImagesByFacets(snap, ranked, params, taxonomy)
	wantType := strings.TrimSpace(paramStr(params, "ImageType", ""))
	wantTag := strings.TrimSpace(paramStr(params, "ImageTag", ""))

	opts := []ConfirmFormOption{}
	seen := map[string]bool{}
	// Unlike the guided form (which disables mismatches), this list FILTERS out
	// GPU-mismatch / non-container-in-pod images — it only offers selectable ones.
	appendOpt := func(id, label string, supported []string, container bool) {
		if id == "" || seen[id] || len(opts) >= maxFormImageOptions {
			return
		}
		if zoneIsPod && !container {
			return
		}
		if len(supported) > 0 && !containsFold(supported, gpuType) {
			return
		}
		seen[id] = true
		opts = append(opts, ConfirmFormOption{Value: id, Label: label})
	}

	// Current selection first (from the raw result, so a threaded id absent from the
	// ranked pool is still honored) — but only when it survives the active facets.
	if !imageSelectionMatchesFacets(snap, current, wantType, wantTag) {
		current = ""
	} else if entry, ok := snap.ByID(current); ok {
		appendOpt(entry.ID, entry.Name, entry.SupportedGPUTypes, entry.Container)
	} else {
		current = ""
	}
	if current != "" && !seen[current] {
		current = ""
	}
	for _, sel := range ranked {
		entry, _ := snap.ByID(sel.ID)
		appendOpt(sel.ID, sel.Name, entry.SupportedGPUTypes, entry.Container)
	}
	if current == "" && len(opts) > 0 {
		current = opts[0].Value
	}
	return current, opts
}

// currentImageSupportedGPUs returns the SupportedGpuTypes declared by the
// currently selected image (empty = no constraint declared).
func currentImageSupportedGPUs(params map[string]any, images map[string]any) []string {
	if images == nil {
		return nil
	}
	if normalizedImageSource(paramStr(params, "ImageSource", imageSourcePlatform)) != imageSourcePlatform &&
		strings.TrimSpace(paramStr(params, "CompShareImageId", "")) == "" &&
		strings.TrimSpace(paramStr(params, "ImageName", "")) == "" {
		return nil
	}
	if id := pickImageId(params, images); id != "" {
		if s := imageSupportedByID(images, id); len(s) > 0 {
			return s
		}
	}
	return nil
}

// imageNameByID finds the display name for an image id in either result shape
// (platform ImageSet / community CompshareImageGroup). Returns "" when absent.
func imageNameByID(images map[string]any, id string) string {
	if images == nil || id == "" {
		return ""
	}
	if groups, ok := images["CompshareImageGroup"].([]any); ok {
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if gm == nil {
				continue
			}
			data, _ := gm["Data"].([]any)
			for _, d := range data {
				dm, _ := d.(map[string]any)
				if got, _ := dm["CompShareImageId"].(string); got == id {
					name, _ := gm["ImageName"].(string)
					return name
				}
			}
		}
		return ""
	}
	imageSet, _ := images["ImageSet"].([]any)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		if got, _ := img["CompShareImageId"].(string); got == id {
			name, _ := img["Name"].(string)
			return name
		}
	}
	return ""
}

// imageSupportedByID returns the SupportedGpuTypes list of the image with the
// given id, in either result shape.
func imageSupportedByID(images map[string]any, id string) []string {
	if groups, ok := images["CompshareImageGroup"].([]any); ok {
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if gm == nil {
				continue
			}
			data, _ := gm["Data"].([]any)
			for _, d := range data {
				dm, _ := d.(map[string]any)
				if got, _ := dm["CompShareImageId"].(string); got == id {
					return formStringSlice(dm["SupportedGpuTypes"])
				}
			}
		}
		return nil
	}
	imageSet, _ := images["ImageSet"].([]any)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		if got, _ := img["CompShareImageId"].(string); got == id {
			return formStringSlice(img["SupportedGpuTypes"])
		}
	}
	return nil
}

func imageContainerByID(images map[string]any, id string) bool {
	if images == nil || id == "" {
		return false
	}
	if groups, ok := images["CompshareImageGroup"].([]any); ok {
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if gm == nil {
				continue
			}
			data, _ := gm["Data"].([]any)
			for _, d := range data {
				dm, _ := d.(map[string]any)
				if got, _ := dm["CompShareImageId"].(string); got == id {
					return paramBool(dm, "Container", false) || paramBool(dm, "IsContainer", false)
				}
			}
		}
		return false
	}
	imageSet, _ := images["ImageSet"].([]any)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		if got, _ := img["CompShareImageId"].(string); got == id {
			return paramBool(img, "Container", false) || paramBool(img, "IsContainer", false)
		}
	}
	return false
}

// formStringSlice converts a JSON-decoded []any of strings to []string,
// skipping non-string and duplicate entries.
func formStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]bool, len(arr))
	var out []string
	for _, x := range arr {
		s, ok := x.(string)
		if !ok || s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// containsFold reports whether list contains s, case-insensitively.
func containsFold(list []string, s string) bool {
	for _, item := range list {
		if strings.EqualFold(item, s) {
			return true
		}
	}
	return false
}
