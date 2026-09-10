package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// Placement turns a zone name into the concrete facts a create needs: the
// catalog entry, the numeric zone id, the pool that charge type buys from,
// the CPU platform and the disks. The turn's zone catalog snapshot is the
// sole authority — a missing snapshot, or a zone it does not carry, is a hard
// failure, because guessing a placement is how an instance lands somewhere
// the user did not ask for.

// workflowZoneEntry is THE single zone read: every zone consumer (create
// validation, net-optimizer, form labels, pod meta, zone_id args) resolves a zone
// through here, so the snapshot-vs-map decision lives in ONE place instead of a
// re-implemented branch at each site.
//
// The turn's zone catalog snapshot is the sole authority. It must be present and
// available: a missing/unavailable snapshot, or a zone it does not carry, is a hard
// failure — the create refuses rather than guessing a placement. (Before the zone
// convergence a nil snapshot fell back to per-zone param maps for unmigrated tests;
// those maps are gone, and Available() is nil-safe so a nil snapshot simply reports
// unavailable.)
func workflowZoneEntry(wfCtx *Context, zone string) (deployment.ZoneCatalogEntry, error) {
	cat := wfCtx.ZoneCatalog()
	if !cat.Available() {
		return deployment.ZoneCatalogEntry{}, fmt.Errorf("可用区目录当前不可用，无法安全创建，请稍后重试")
	}
	entry, ok := cat.Entry(zone)
	if !ok {
		return deployment.ZoneCatalogEntry{}, fmt.Errorf("可用区 %s 不在当前可用区目录中，无法安全创建", zone)
	}
	return entry, nil
}

// workflowZoneIDIndex maps numeric zone id → zone id string for the turn's zones,
// so a numeric-keyed payload (the GPU inventory) can be decoded to zone names, from
// the single authoritative snapshot. An absent/unavailable snapshot yields an empty
// index (nil-safe), never a per-zone param map.
func workflowZoneIDIndex(wfCtx *Context) map[uint32]string {
	out := map[uint32]string{}
	cat := wfCtx.ZoneCatalog()
	if !cat.Available() {
		return out
	}
	for _, zone := range cat.Zones() {
		if p, ok := cat.Placement(zone); ok && p.ZoneID != 0 {
			out[p.ZoneID] = zone
		}
	}
	return out
}

// workflowZonePlacement resolves a zone to its full placement — one record whose
// ZoneID/Region/AzGroup/IsPod cannot disagree — through the single workflowZoneEntry.
func workflowZonePlacement(wfCtx *Context, zone string) (deployment.ZonePlacement, error) {
	entry, err := workflowZoneEntry(wfCtx, zone)
	if err != nil {
		return deployment.ZonePlacement{}, err
	}
	return entry.Placement, nil
}

func validateCreatePlacement(wfCtx *Context, placement deployment.ZonePlacement, purchase bool) error {
	chargeType := createChargeType(wfCtx.Params)
	if placement.IsPod && placement.ZoneID == 0 {
		return fmt.Errorf("未获取到 %s 的内部可用区编号，无法安全创建。请稍后重试或到控制台确认可用区", zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if placement.IsPod && purchase && placement.AzGroup == 0 {
		return fmt.Errorf("未获取到 %s 的内部地域编号，无法安全创建。请稍后重试或到控制台确认可用区", zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if !purchase {
		return nil
	}
	pool := createInventoryPool(chargeType)
	// Only a KNOWN unsupported mode refuses. An unknown one does not: the same
	// rule that stops a zero inventory count from becoming "sold out" applies
	// here symmetrically — a missing observation is not a negative observation,
	// and refusing on it would turn one flaky read of a supplementary API into a
	// blocked create. The create API stays the authority for what it will accept.
	if supported, known := createInventoryPoolSupport(wfCtx, placement, pool); known && !supported {
		return fmt.Errorf("%s 的 %s 当前不支持%s购买方式，请选择其他计费方式或可用区",
			zoneDisplayLabel(wfCtx, placement.Zone), paramStr(wfCtx.Params, "GpuType", "该机型"), createInventoryPoolLabel(pool))
	}
	return nil
}

func createInventoryPool(chargeType string) string {
	if strings.EqualFold(chargeType, deployment.ChargeTypeSpot) {
		return deployment.GPUInventoryPoolSpot
	}
	return deployment.GPUInventoryPoolExclusive
}

func createInventoryPoolLabel(pool string) string {
	if pool == deployment.GPUInventoryPoolSpot {
		return "抢占式"
	}
	return "独占"
}

// createInventoryPoolSupport is the SINGLE purchase-mode fact for this workflow.
// Both create flows run the inventory steps, so the guided cards, the plain
// confirm card and the authoritative create gate all read this one answer —
// which is the whole point: a card must never offer a mode the gate will refuse,
// nor hide one the gate would allow.
//
// A missing snapshot is unknown, not evidence that a purchase pool is supported.
func createInventoryPoolSupport(wfCtx *Context, placement deployment.ZonePlacement, pool string) (bool, bool) {
	return createInventoryPoolSupportFor(wfCtx, placement, paramStr(wfCtx.Params, "GpuType", ""), pool)
}

// createInventoryPoolSupportFor is the same fact for a model the caller names
// explicitly. The GPU card needs it: it is deciding BETWEEN models, so the one
// in Params is not yet the one being judged.
func createInventoryPoolSupportFor(wfCtx *Context, placement deployment.ZonePlacement, gpuType, pool string) (bool, bool) {
	supported, known := deployment.InventoryPoolSupportFromResult(
		wfCtx.Result(createGPUInventoryStep), placement, gpuType, pool,
	)
	if known {
		return supported, true
	}
	// The official product contract always offers Postpay/Day/Month. Spot and
	// Pod pool membership depend on the live inventory metadata and must not be
	// guessed when that metadata is unavailable.
	if !placement.IsPod && pool == deployment.GPUInventoryPoolExclusive {
		return true, true
	}
	return false, false
}

func validateSelectedImageCompatibility(wfCtx *Context, imageID string, placement deployment.ZonePlacement) error {
	images := createImageResult(wfCtx)
	image := imageMapByID(images, imageID)
	name := imageNameByID(images, imageID)
	if name == "" {
		name = "所选镜像"
	}
	// The container/VM verdict comes from imageZoneCompatibility — the same call
	// the zone card makes — so the two cannot drift into different rules. This gate
	// is stricter by design on one verdict only: Unverifiable refuses here and does
	// not disable there, because refusing on missing evidence is the gate's job.
	switch imageContainerFitForZone(images, imageID, placement) {
	case imageContainerFitUnverifiable:
		return fmt.Errorf("未能确认 %s 是可用于 %s 的容器镜像，请刷新后重新选择", name, zoneDisplayLabel(wfCtx, placement.Zone))
	case imageContainerFitNeedsContainerImage:
		return fmt.Errorf("%s 不是容器镜像，不能用于 %s，请更换镜像或可用区", name, zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if image == nil {
		// Community image searches can return a different page/order on a second
		// query. Keep the exact selected id for normal zones; the upstream capacity
		// preflight validates that id, its status, and its adaptive UHost image.
		return nil
	}
	zoneEntry, err := workflowZoneEntry(wfCtx, placement.Zone)
	if err != nil {
		return err
	}
	imageType := strings.TrimSpace(paramStr(image, "ImageType", ""))
	if !zoneEntry.SupportsImageType(imageType) {
		return fmt.Errorf("%s 当前不支持 %s 类型镜像，请更换镜像或可用区", zoneDisplayLabel(wfCtx, placement.Zone), imageType)
	}
	if status := strings.TrimSpace(paramStr(image, "Status", "")); !deployment.ImageStatusUsable(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform), status) {
		return fmt.Errorf("%s 当前不可用，请更换镜像", name)
	}
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	supported := imageSupportedByID(images, imageID)
	if gpuType == "" || len(supported) == 0 || containsFold(supported, gpuType) {
		return nil
	}
	return fmt.Errorf("%s 不支持当前 GPU %s，请更换镜像或卡型", name, gpuType)
}

func workflowCreateDisks(wfCtx *Context, imageID, zone, gpuType string, placement deployment.ZonePlacement) ([]any, error) {
	requestedSystemSize, _ := parseUint32Any(wfCtx.Params["SystemDiskSize"])
	disks := deployment.ResolveBootDisk(
		createImageResult(wfCtx), wfCtx.Result("查询可用配比"), imageID, gpuType, zone, requestedSystemSize,
	)
	requestedDataSize, hasDataDisk := parseUint32Any(wfCtx.Params["DataDiskSize"])
	if !hasDataDisk {
		return disks, nil
	}
	if placement.IsPod {
		return nil, fmt.Errorf("容器区不支持随实例创建普通数据盘，请改选虚机区或取消数据盘")
	}
	rangeSpec, ok := deployment.CatalogDataDiskRange(
		wfCtx.Result("查询可用配比"), gpuType, zone, deployment.DiskTypeCloudSSD,
	)
	if !ok {
		return nil, fmt.Errorf("当前可用区和机型不支持 SSD 云数据盘")
	}
	if rangeSpec.MinimumGB > 0 && requestedDataSize < rangeSpec.MinimumGB {
		return nil, fmt.Errorf("数据盘容量不能小于 %dGB", rangeSpec.MinimumGB)
	}
	if rangeSpec.MaximumGB > 0 && requestedDataSize > rangeSpec.MaximumGB {
		return nil, fmt.Errorf("数据盘容量不能大于 %dGB", rangeSpec.MaximumGB)
	}
	return append(disks, map[string]any{
		"IsBoot": false,
		"Type":   rangeSpec.Type,
		"Size":   requestedDataSize,
	}), nil
}

func workflowMinimalCPUPlatform(wfCtx *Context, gpuType, zone string) string {
	if v := strings.TrimSpace(paramStr(wfCtx.Params, "MinimalCpuPlatform", "")); v != "" {
		if strings.EqualFold(v, deployment.MinimalCPUPlatformAuto) {
			if first := workflowFirstCPUPlatform(wfCtx.Result("查询可用配比"), gpuType, zone); first != "" {
				return first + "/Auto"
			}
		}
		return v
	}
	if first := workflowFirstCPUPlatform(wfCtx.Result("查询可用配比"), gpuType, zone); first != "" {
		return first + "/Auto"
	}
	return deployment.MinimalCPUPlatformAuto
}

func workflowFirstCPUPlatform(catalog map[string]any, gpuType, zone string) string {
	entry := workflowCatalogEntry(catalog, gpuType, zone)
	if entry == nil {
		return ""
	}
	raw, _ := entry["CpuPlatforms"].(map[string]any)
	if len(raw) == 0 {
		return ""
	}
	if _, ok := raw["Amd"]; ok {
		return "Amd"
	}
	if _, ok := raw["Intel"]; ok {
		return "Intel"
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func workflowCatalogEntry(catalog map[string]any, gpuType, zone string) map[string]any {
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
