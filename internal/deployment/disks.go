package deployment

import (
	"strconv"
	"strings"
)

// ResolveBootDisk derives the boot disk to send on a create/capacity-check
// request from live catalog + image data, instead of a hardcoded size/type:
// its size is the largest of the user's request, the image's declared size and
// the gpuType+zone catalog minimum. Returns nil when
// neither source yields a usable size or the catalog has no boot disk type
// for this gpuType+zone — callers should omit the Disks arg entirely rather
// than guess, exactly like the pre-existing zero-value/omitempty contract on
// deployment.DeploymentDraft.Disks.
//
// images is a DescribeCompShareImages result (ImageSet or CompshareImageGroup
// shape); catalog is a DescribeAvailableCompShareInstanceTypes result.
// requestedSizeGB is the user's explicit size, or zero to derive the default.
func ResolveBootDisk(images, catalog map[string]any, imageID, gpuType, zone string, requestedSizeGB uint32) []any {
	sizeGB := requestedSizeGB
	if imageGB := imageSizeGB(images, imageID); imageGB > sizeGB {
		sizeGB = imageGB
	}
	if minimumGB := catalogBootDiskMinGB(catalog, gpuType, zone); minimumGB > sizeGB {
		sizeGB = minimumGB
	}
	if sizeGB == 0 {
		return nil
	}
	diskType := catalogBootDiskType(catalog, gpuType, zone)
	if diskType == "" {
		return nil
	}
	return []any{map[string]any{
		"IsBoot": true,
		"Type":   diskType,
		"Size":   sizeGB,
	}}
}

func imageSizeGB(images map[string]any, imageID string) uint32 {
	img := imageMapByID(images, imageID)
	if img == nil {
		return 0
	}
	for _, key := range []string{"Size", "ImageSize"} {
		if gb := ceilMBToGB(img[key]); gb > 0 {
			return gb
		}
	}
	return 0
}

func imageMapByID(images map[string]any, id string) map[string]any {
	if images == nil || id == "" {
		return nil
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
					return dm
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
			return img
		}
	}
	return nil
}

func catalogEntry(catalog map[string]any, gpuType, zone string) map[string]any {
	if catalog == nil || gpuType == "" {
		return nil
	}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	var fallback map[string]any
	for _, item := range types {
		entry, _ := item.(map[string]any)
		if entry == nil {
			continue
		}
		if name, _ := entry["Name"].(string); name != gpuType {
			continue
		}
		if fallback == nil {
			fallback = entry
		}
		entryZone, _ := entry["Zone"].(string)
		if zone == "" || entryZone == "" || strings.EqualFold(entryZone, zone) {
			return entry
		}
	}
	return fallback
}

func catalogBootDiskType(catalog map[string]any, gpuType, zone string) string {
	entry := catalogEntry(catalog, gpuType, zone)
	if entry == nil {
		return ""
	}
	for _, disk := range diskMaps(entry["Disks"]) {
		for _, boot := range diskMaps(disk["BootDisk"]) {
			if name := strings.TrimSpace(stringFieldAny(boot["Name"])); name != "" {
				return name
			}
			if name := strings.TrimSpace(stringFieldAny(boot["Type"])); name != "" {
				return name
			}
		}
	}
	return ""
}

func catalogBootDiskMinGB(catalog map[string]any, gpuType, zone string) uint32 {
	entry := catalogEntry(catalog, gpuType, zone)
	if entry == nil {
		return 0
	}
	for _, disk := range diskMaps(entry["Disks"]) {
		for _, boot := range diskMaps(disk["BootDisk"]) {
			for _, key := range []string{"MinimalSize", "MinSize", "Size"} {
				if n, ok := positiveFloatAny(boot[key]); ok {
					return uint32(n)
				}
			}
		}
	}
	return 0
}

func diskMaps(v any) []map[string]any {
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

func stringFieldAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func positiveFloatAny(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, x > 0
	case float32:
		return float64(x), x > 0
	case int:
		return float64(x), x > 0
	case int64:
		return float64(x), x > 0
	case uint32:
		return float64(x), x > 0
	case uint64:
		return float64(x), x > 0
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n, err == nil && n > 0
	default:
		return 0, false
	}
}

func ceilMBToGB(v any) uint32 {
	n, ok := positiveFloatAny(v)
	if !ok {
		return 0
	}
	gb := n / 1024
	out := uint32(gb)
	if gb > float64(out) {
		out++
	}
	if out == 0 {
		out = 1
	}
	return out
}
