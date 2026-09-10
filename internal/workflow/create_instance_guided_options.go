package workflow

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// The option lists behind the guided cards, built from the live inventory
// probe rather than from the catalog alone: an option the catalog sells but
// no zone can currently create is offered disabled with the stock note that
// says so. guidedInventory is the per-turn probe result those lists read.

type guidedInventory struct {
	counts        map[string]map[string]map[string]float64
	preferredPool string
}

func guidedInventoryFrom(wfCtx *Context, result map[string]any) guidedInventory {
	zoneByID := workflowZoneIDIndex(wfCtx)
	if len(zoneByID) == 0 || result == nil {
		return guidedInventory{}
	}
	rawInv, _ := result["GpuInventory"].(map[string]any)
	if rawInv == nil {
		return guidedInventory{}
	}
	preferredPool := deployment.GPUInventoryPoolExclusive
	if strings.EqualFold(createChargeType(wfCtx.Params), "Spot") {
		preferredPool = deployment.GPUInventoryPoolSpot
	}
	counts := map[string]map[string]map[string]float64{}
	for _, poolName := range []string{deployment.GPUInventoryPoolExclusive, deployment.GPUInventoryPoolSpot} {
		rawPool, _ := rawInv[poolName].(map[string]any)
		for rawZoneID, rawGPUCounts := range rawPool {
			id, ok := parseUint32Any(rawZoneID)
			if !ok {
				continue
			}
			zone := zoneByID[id]
			if zone == "" {
				continue
			}
			gpuCounts, _ := rawGPUCounts.(map[string]any)
			if gpuCounts == nil {
				continue
			}
			if counts[zone] == nil {
				counts[zone] = map[string]map[string]float64{}
			}
			if counts[zone][poolName] == nil {
				counts[zone][poolName] = map[string]float64{}
			}
			for gpuType, rawCount := range gpuCounts {
				counts[zone][poolName][gpuType] = anyFloat(rawCount)
			}
		}
	}
	if len(counts) == 0 {
		return guidedInventory{}
	}
	return guidedInventory{counts: counts, preferredPool: preferredPool}
}

// addZoneRegionAndID stamps the read-probe query with the zone's Region and
// internal id taken from ONE catalog record, so the two fields can never come
// from different sources. On a present-but-unresolvable snapshot (unavailable,
// or the zone absent) it stamps nothing rather than string-guessing a Region for
// a zone the authority rejected — the create refuses downstream anyway.
func addZoneRegionAndID(wfCtx *Context, args map[string]any, zone string) map[string]any {
	entry, err := workflowZoneEntry(wfCtx, zone)
	if err != nil {
		return args
	}
	if r := strings.TrimSpace(entry.Placement.Region); r != "" {
		args["Region"] = r
	}
	if entry.Placement.ZoneID != 0 {
		args["zone_id"] = entry.Placement.ZoneID
	}
	return args
}

func syncGuidedZoneMeta(wfCtx *Context, zone string) {
	if wfCtx == nil || strings.TrimSpace(zone) == "" {
		return
	}
	// The pod flag is the snapshot record's. An unresolvable zone (unavailable
	// snapshot, or a zone the catalog does not carry) writes nothing.
	entry, err := workflowZoneEntry(wfCtx, zone)
	if err != nil {
		return
	}
	wfCtx.Params["ZoneIsPod"] = entry.Placement.IsPod
	wfCtx.Params["IsPodZone"] = entry.Placement.IsPod
}

func parseUint32Any(v any) (uint32, bool) {
	switch x := v.(type) {
	case uint32:
		return x, x != 0
	case uint64:
		if x == 0 || x > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(x), true
	case int:
		if x <= 0 {
			return 0, false
		}
		return uint32(x), true
	case int64:
		if x <= 0 || x > int64(^uint32(0)) {
			return 0, false
		}
		return uint32(x), true
	case float64:
		if x <= 0 || x > float64(^uint32(0)) || x != float64(uint32(x)) {
			return 0, false
		}
		return uint32(x), true
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(x), 10, 32)
		if err != nil || n == 0 {
			return 0, false
		}
		return uint32(n), true
	default:
		return 0, false
	}
}

func anyFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	default:
		return 0
	}
}

func (inv guidedInventory) count(zone, gpuType string) (float64, bool) {
	if inv.counts == nil || zone == "" || gpuType == "" {
		return 0, false
	}
	pools, ok := inv.counts[zone]
	if !ok {
		return 0, false
	}
	if gpus, ok := pools[inv.preferredPool]; ok {
		if count, present := gpus[gpuType]; present {
			return count, true
		}
	}
	// Pod zones may expose only Spot inventory even before the user reaches the
	// charge-type form. Use the other explicitly returned pool rather than
	// presenting the zone as unknown; this is still a real backend observation.
	for _, pool := range []string{deployment.GPUInventoryPoolExclusive, deployment.GPUInventoryPoolSpot} {
		if pool == inv.preferredPool {
			continue
		}
		if gpus, ok := pools[pool]; ok {
			if count, present := gpus[gpuType]; present {
				return count, true
			}
		}
	}
	return 0, false
}

func (inv guidedInventory) total(zones []string, gpuType string) (float64, bool) {
	var total float64
	known := false
	for _, zone := range zones {
		if count, ok := inv.count(zone, gpuType); ok {
			known = true
			total += count
		}
	}
	return total, known
}

// guidedStockNote renders the raw DescribeCompShareGpuInventory reading. That source is
// a SNAPSHOT and is not authoritative — real creatability comes from
// CheckCompShareResourceCapacity (which needs a GPU type + zone as input, so it cannot
// gate this earlier step) and finally from 检查库存 on the sealed config. The wording
// must therefore never read as a promise: a card that said a sold-out GPU was available
// is exactly the reported failure.
func guidedStockNote(count float64) string {
	if count <= 0 {
		return "库存快照为 0，待确认"
	}
	return fmt.Sprintf("库存快照约 %.0f 张 GPU，待确认", count)
}

func guidedStockFitNote(free, requested float64) string {
	if free >= requested {
		return "当前库存可满足"
	}
	if free <= 0 {
		return "库存快照为 0，待确认"
	}
	return fmt.Sprintf("库存快照仅剩 %.0f 张 GPU，待确认", free)
}

func firstEnabledValue(opts []ConfirmFormOption) string {
	for _, opt := range opts {
		if !opt.Disabled {
			return opt.Value
		}
	}
	return ""
}

func guidedGPUFormOptions(wfCtx *Context, catalog map[string]any, supported []string, current string, locked bool, params map[string]any, inventoryResult map[string]any) (string, []ConfirmFormOption) {
	if catalog == nil {
		return current, nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	candidateOrder, candidateSet := guidedCandidateGPUSet(params)
	if locked {
		candidateOrder, candidateSet = nil, nil
	}
	reasons := guidedGPUReasons(params)
	type gpuChoice struct {
		name    string
		normal  bool
		vramGB  float64
		zones   []string
		current bool
	}
	choices := map[string]*gpuChoice{}
	order := []string{}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		name, _ := mt["Name"].(string)
		if name == "" {
			continue
		}
		zone, _ := mt["Zone"].(string)
		zone, zoneSupported := guidedExecutableZone(wfCtx, zone)
		if !zoneSupported {
			continue
		}
		if locked && current != "" && !guidedGPUIntentMatches(current, name) &&
			(len(supported) == 0 || !containsFold(supported, name)) {
			continue
		}
		if len(candidateSet) > 0 && !candidateSet[strings.ToLower(name)] && !strings.EqualFold(name, current) {
			continue
		}
		ch, ok := choices[name]
		if !ok {
			ch = &gpuChoice{name: name}
			choices[name] = ch
			order = append(order, name)
		}
		if name == current {
			ch.current = true
		}
		status, _ := mt["Status"].(string)
		if status == "" || strings.EqualFold(status, "Normal") {
			ch.normal = true
		}
		if !containsFold(ch.zones, zone) {
			ch.zones = append(ch.zones, zone)
		}
		if gm, _ := mt["GraphicsMemory"].(map[string]any); gm != nil {
			if v, _ := gm["Value"].(float64); v > 0 && ch.vramGB == 0 {
				ch.vramGB = v
			}
		}
	}
	if current != "" {
		if _, ok := choices[current]; !ok {
			choices[current] = &gpuChoice{name: current, current: true}
			order = append([]string{current}, order...)
		}
	}
	if len(candidateOrder) > 0 {
		var filtered []string
		seen := map[string]bool{}
		for _, name := range candidateOrder {
			if choices[name] == nil || seen[strings.ToLower(name)] {
				continue
			}
			filtered = append(filtered, name)
			seen[strings.ToLower(name)] = true
		}
		order = filtered
	}
	// Real creatability for the exact image / disk / charge type this create will
	// send, per (model, zone). A combination the probe could not answer is absent
	// and reads as unknown — never as a refusal.
	combos := comboCreatability(wfCtx.Result(zoneCapacityStepName))
	// Same escape hatch the zone card has: the gate STEERS, it does not refuse.
	// If nothing is creatable there is nothing to steer toward, and graying out
	// every model leaves a card that offers nothing — ensureGuidedGPUType turns
	// that into a dead end raised before the draft exists, losing both the
	// candidate draft and the typed capacity_sold_out reason the sold-out reply is
	// built from. That authoritative negative belongs to 检查库存.
	anyModelCreatable := false
	for _, name := range order {
		if ch := choices[name]; ch != nil {
			if ok, known := gpuModelCreatable(combos, ch.name, ch.zones); ok && known {
				anyModelCreatable = true
				break
			}
		}
	}
	var opts []ConfirmFormOption
	appendChoice := func(name string) {
		ch := choices[name]
		if ch == nil {
			return
		}
		label := ch.name
		if ch.vramGB > 0 {
			label = fmt.Sprintf("%s（%.0fG显存）", ch.name, ch.vramGB)
		}
		noteParts := []string{}
		if reason := reasons[strings.ToLower(ch.name)]; reason != "" {
			noteParts = append(noteParts, reason)
		}
		if ch.vramGB > 0 {
			noteParts = append(noteParts, fmt.Sprintf("%.0fG 显存", ch.vramGB))
		}
		stock, stockKnown := inventory.total(ch.zones, ch.name)
		imageUnsupported := len(supported) > 0 && !containsFold(supported, ch.name)
		poolUnsupported, poolReason := poolUnsupportedEverywhere(wfCtx, ch.zones, ch.name)
		canCreate, creatabilityKnown := gpuModelCreatable(combos, ch.name, ch.zones)
		soldOut := creatabilityKnown && !canCreate && anyModelCreatable
		disabled := !ch.normal || imageUnsupported || poolUnsupported || soldOut
		// A disabled option's reason goes in Reason ONLY, never also into the note.
		// The client renders [Note, Disabled && Reason] joined (MessageItem.jsx), so
		// a reason present in both is printed twice — live: "4090 · 该可用区不支持独占
		// 购买方式 · 该可用区不支持独占购买方式". Note carries the neutral context,
		// Reason carries the why.
		disabledReason := ""
		if imageUnsupported {
			disabledReason = "镜像不支持当前 GPU"
		}
		if poolUnsupported && disabledReason == "" {
			// Suppresses the stock line rather than joining it: a card that says both
			// "不支持抢占式" and "库存快照约 3 张" is telling the user to try anyway.
			disabledReason = poolReason
			stockKnown = false
		}
		if soldOut && disabledReason == "" {
			disabledReason = "当前配置在所有可用区都无可创建库存"
			stockKnown = false
		}
		if ch.normal && !poolUnsupported && !soldOut {
			switch {
			case creatabilityKnown && canCreate:
				// The authoritative answer, for the exact image / disk / charge type
				// this create will send. It outranks the snapshot count, which reports
				// 0 for zones that are in fact selling.
				noteParts = append(noteParts, "当前可创建")
			case stockKnown:
				noteParts = append(noteParts, guidedStockNote(stock))
			default:
				// Status==Normal means the model is ON SALE in the catalog — it is NOT
				// evidence that stock exists right now, and no capacity reading is
				// available. Saying "可售" turned that catalog fact into an availability
				// promise, which is how a sold-out GPU was offered as available. State
				// the catalog fact and defer the verdict.
				noteParts = append(noteParts, "在售，库存以最终确认为准")
			}
		}
		if !ch.normal && disabledReason == "" {
			disabledReason = "暂不可售"
		}
		if len(ch.zones) > 0 {
			noteParts = append(noteParts, "可用区 "+strings.Join(ch.zones, "、"))
		}
		opt := ConfirmFormOption{
			Value:    ch.name,
			Label:    label,
			Note:     strings.Join(noteParts, " · "),
			Reason:   disabledReason,
			Disabled: disabled,
			Meta: map[string]string{
				"Sellable": strconv.FormatBool(ch.normal),
			},
		}
		if stockKnown {
			opt.Meta["StockKnown"] = "true"
			opt.Meta["StockFree"] = fmt.Sprintf("%.0f", stock)
		}
		opts = append(opts, opt)
	}
	if current != "" {
		appendChoice(current)
	}
	for _, name := range order {
		if name == current {
			continue
		}
		appendChoice(name)
	}
	selected := current
	if selected == "" || !enabledOptionExists(opts, selected) {
		if value := firstEnabledValue(opts); value != "" {
			selected = value
		} else if selected == "" && len(opts) > 0 {
			selected = opts[0].Value
		}
	}
	return selected, opts
}

func guidedCandidateGPUSet(params map[string]any) ([]string, map[string]bool) {
	raw, ok := params["GuidedCandidateGPUs"]
	if !ok || raw == nil {
		return nil, nil
	}
	var order []string
	switch v := raw.(type) {
	case []string:
		order = append(order, v...)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	}
	if len(order) == 0 {
		return nil, nil
	}
	set := map[string]bool{}
	var cleaned []string
	for _, name := range order {
		name = strings.TrimSpace(name)
		if name == "" || set[strings.ToLower(name)] {
			continue
		}
		set[strings.ToLower(name)] = true
		cleaned = append(cleaned, name)
	}
	return cleaned, set
}

func guidedGPUReasons(params map[string]any) map[string]string {
	out := map[string]string{}
	raw, ok := params["GuidedGpuReasons"]
	if !ok || raw == nil {
		return out
	}
	switch v := raw.(type) {
	case map[string]string:
		for k, val := range v {
			if strings.TrimSpace(k) != "" && strings.TrimSpace(val) != "" {
				out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(val)
			}
		}
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok && strings.TrimSpace(k) != "" && strings.TrimSpace(s) != "" {
				out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(s)
			}
		}
	}
	return out
}

func guidedGPUIntentMatches(intent, candidate string) bool {
	return strings.EqualFold(strings.TrimSpace(intent), strings.TrimSpace(candidate))
}

// guidedZoneFormOptions returns the zone card's selection, its options, and
// whether the gates stood down — i.e. whether every zone failed some rule and the
// options were re-enabled to keep the flow moving. The caller needs that flag
// because a stood-down card must say so; silently offering everything is how the
// user ends up choosing a zone the create gate will refuse.
func guidedZoneFormOptions(wfCtx *Context, catalog map[string]any, gpuType, current string, params map[string]any, inventoryResult map[string]any) (string, []ConfirmFormOption, bool) {
	if catalog == nil || gpuType == "" {
		return "", nil, false
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	// Real creatability per zone, when the probe managed to establish it. A zone
	// missing from this map remains unknown —
	// the raw GPU inventory count is NOT a substitute (it reports 0 for zones
	// that are in fact selling, which is why it may inform the note but never
	// disable an option).
	candidateZones := guidedExecutableCandidateZones(wfCtx, catalog, gpuType)
	creatable := zoneCreatabilityFor(
		comboCreatability(wfCtx.Result(zoneCapacityStepName)), gpuType, candidateZones)
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	for _, zone := range candidateZones {
		if seen[zone] {
			continue
		}
		seen[zone] = true
		zoneLabel := zoneDisplayLabel(wfCtx, zone)
		count, stockKnown := inventory.count(zone, gpuType)
		note := fmt.Sprintf("%s 可用", gpuType)
		disabled := false
		disabledReason := ""
		if stockKnown {
			note = fmt.Sprintf("%s · %s", gpuType, guidedStockNote(count))
		}
		if ok, known := creatable[zone]; known {
			if ok {
				note = fmt.Sprintf("%s · 当前可创建", gpuType)
			} else {
				disabled = true
				disabledReason = "该可用区当前无可创建库存"
			}
		}
		// The charge type is settled before this card, so the purchase-mode
		// constraint runs in its natural direction: a zone the billing mode cannot
		// use is grayed out here rather than refused at the create gate. It reads
		// the same fact the gate reads, so it can never deny a zone the gate would
		// have accepted; the converse does not hold — see the stand-down below.
		if unsupported, reason := poolUnsupportedInZone(wfCtx, zone, gpuType); !disabled && unsupported {
			disabled = true
			disabledReason = reason
		}
		// The image is settled several cards before this one, so the container/VM
		// constraint runs in its natural direction here too: a zone that cannot boot
		// the chosen image is grayed out while the user can still act on it.
		//
		// "Never offered a zone the create gate will refuse" is NOT what this
		// establishes, and saying so would be a lie the next reader would trust:
		// an image id this catalog page does not carry passes here and is refused
		// there (imageZoneUnverifiable), and the stand-down below deliberately
		// re-enables everything when nothing is left.
		if rejects, reason := zoneRejectsSelectedImage(wfCtx, zone); !disabled && rejects {
			disabled = true
			disabledReason = reason
		}
		// A disabled zone must not also advertise stock. "4090 · 库存约 8 张" beside
		// "该可用区不支持独占购买方式" reads as an invitation to try anyway; the
		// reason is the only thing worth saying. It lives in Reason alone — the
		// client joins [Note, Disabled && Reason], so repeating it here printed it
		// twice.
		if disabled {
			note = gpuType
		}
		opt := ConfirmFormOption{
			Value:    zone,
			Label:    zoneLabel,
			Note:     note,
			Reason:   disabledReason,
			Disabled: disabled,
			Meta:     map[string]string{"Zone": zone, "ZoneLabel": zoneLabel},
		}
		if stockKnown {
			opt.Meta["StockKnown"] = "true"
			opt.Meta["StockFree"] = fmt.Sprintf("%.0f", count)
		}
		if zone == current {
			opts = append([]ConfirmFormOption{opt}, opts...)
		} else {
			opts = append(opts, opt)
		}
	}
	// ONE stand-down over the COMBINED verdict, not one per rule.
	//
	// The gates steer; they do not refuse. Graying out every zone leaves a card
	// that offers nothing, and ensureGuidedZone turns that into "暂无可选可用区" —
	// a dead end raised BEFORE the draft exists, so the failure record would lose
	// both the candidate draft and the typed capacity_sold_out reason the sold-out
	// reply is built from. When nothing is selectable there is nothing to steer
	// toward, and the authoritative negative belongs to 检查库存 / the create gate,
	// which raise it with a complete record. See stepCheckCapacity.
	//
	// What this is NOT: a fix for the user's situation. Re-enabling the options is a
	// downgrade that keeps the flow moving, and one of the known-bad zones then
	// becomes the default. The confirm protocol has no back operation
	// (ConfirmResolution is confirmed/denied plus overrides — types.go), so from
	// here the user can only cancel the whole create and start over, or continue and
	// be stopped by 检查库存 / the create gate. The notes and the stood-down
	// description below are what make that choice an informed one; they are not a
	// substitute for a zone-step structured conflict that could send them back to
	// the image card.
	stoodDown := false
	if firstEnabledValue(opts) == "" && len(opts) > 0 {
		stoodDown = true
		for i := range opts {
			foldReasonIntoNote(&opts[i])
			opts[i].Disabled = false
		}
	}
	selected := current
	if selected == "" || !seen[selected] || !enabledOptionExists(opts, selected) {
		selected = firstEnabledValue(opts)
	}
	return selected, opts, stoodDown
}

// foldReasonIntoNote moves a disabled option's Reason into its Note.
//
// The client renders the reason ONLY while the option is disabled —
// `[o.Note, o.Disabled && o.Reason].filter(Boolean).join(' · ')` — so re-enabling
// an option without folding would silently drop the one line that explained it.
// This is the whole reason Note and Reason are kept disjoint at every producer:
// they can be concatenated safely exactly once, here.
func foldReasonIntoNote(opt *ConfirmFormOption) {
	if opt.Reason == "" {
		return
	}
	if opt.Note == "" {
		opt.Note = opt.Reason
	} else {
		opt.Note = opt.Note + " · " + opt.Reason
	}
	opt.Reason = ""
}

// guidedZoneStandDownDescription replaces the zone card's normal copy when every
// zone failed some rule. The normal copy ("建议优先选择有现货的可用区") is actively
// wrong then — there is no good choice to steer toward — and a card whose options
// each carry a warning but whose heading still recommends picking one reads as a
// rendering glitch rather than as the conflict it is.
func guidedZoneStandDownDescription() string {
	return "当前没有同时满足库存、购买方式和所选镜像的可用区，下面每个区都标注了原因。" +
		"继续确认不会跳过后续检查——真正的创建仍会被拦下。" +
		"建议取消本次创建，改用其他镜像、GPU 型号或计费方式后重新开始。"
}

func guidedGPUCountFormOptions(wfCtx *Context, catalog map[string]any, gpuType, zone string, current float64, params map[string]any, inventoryResult map[string]any) (float64, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" || zone == "" {
		return 0, nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	capSpecs := parseCapacitySpecs(wfCtx.Result(capacitySpecsStepName))
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && !strings.EqualFold(status, "Normal") {
			continue
		}
		entryZone, _ := mt["Zone"].(string)
		if entryZone == "" {
			continue
		}
		if entryZone != zone {
			continue
		}
		sizes, _ := mt["MachineSizes"].([]any)
		for _, s := range sizes {
			size, _ := s.(map[string]any)
			gpu, _ := size["Gpu"].(float64)
			if gpu <= 0 {
				continue
			}
			value := fmt.Sprintf("%.0f", gpu)
			if seen[value] {
				continue
			}
			seen[value] = true
			free, stockKnown := inventory.count(zone, gpuType)
			zoneLabel := zoneDisplayLabel(wfCtx, zone)
			note := fmt.Sprintf("%s · %s", gpuType, zoneLabel)
			disabled := false
			disabledReason := ""
			if stockKnown {
				fit := guidedStockFitNote(free, gpu)
				note = fmt.Sprintf("%s · %s · %s", gpuType, zoneLabel, fit)
			}
			// Gate by real creatability: disable a card count only when the capacity
			// check enumerated it and found it short. Counts capacity never evaluated
			// stay enabled — the final 检查库存 re-check is the authoritative negative.
			if capacityHasSignal(capSpecs) && capacityKnowsGPUCount(capSpecs, int(gpu)) && !capacityGPUCountEnough(capSpecs, int(gpu)) {
				disabled = true
				disabledReason = "该卡数当前无可创建库存"
			}
			opt := ConfirmFormOption{
				Value:    value,
				Label:    fmt.Sprintf("%.0f 张 GPU", gpu),
				Note:     note,
				Reason:   disabledReason,
				Disabled: disabled,
				Meta:     map[string]string{"GPU": value, "Zone": zone, "ZoneLabel": zoneLabel},
			}
			if stockKnown {
				opt.Meta["StockKnown"] = "true"
				opt.Meta["StockFree"] = fmt.Sprintf("%.0f", free)
			}
			if current == gpu {
				opts = append([]ConfirmFormOption{opt}, opts...)
			} else {
				opts = append(opts, opt)
			}
		}
	}
	// A capacity gate must never dead-end the flow: if it disabled every count,
	// keep them selectable and let the authoritative 检查库存 report the shortage.
	if firstEnabledValue(opts) == "" {
		for i := range opts {
			opts[i].Disabled = false
			opts[i].Reason = ""
		}
	}
	selected := current
	if selected == 0 || !seen[fmt.Sprintf("%.0f", selected)] || !enabledOptionExists(opts, fmt.Sprintf("%.0f", selected)) {
		if value := firstEnabledValue(opts); value != "" {
			selected, _ = strconv.ParseFloat(value, 64)
		} else {
			selected = 0
		}
	}
	return selected, opts
}

func guidedCpuMemoryFormOptions(wfCtx *Context, catalog map[string]any, gpuType, zone string, gpuCount float64, params map[string]any, inventoryResult map[string]any) (string, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" || zone == "" || gpuCount <= 0 {
		return "", nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	capSpecs := parseCapacitySpecs(wfCtx.Result(capacitySpecsStepName))
	current := ""
	if _, hasCPU := params["Cpu"]; hasCPU {
		if _, hasMem := params["Memory"]; hasMem {
			current = formatGuidedSpecKey(zone, gpuCount, paramNum(params, "Cpu", 0), paramNum(params, "Memory", 0))
		}
	}
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && !strings.EqualFold(status, "Normal") {
			continue
		}
		entryZone, _ := mt["Zone"].(string)
		if entryZone == "" {
			continue
		}
		if entryZone != zone {
			continue
		}
		sizes, _ := mt["MachineSizes"].([]any)
		for _, s := range sizes {
			size, _ := s.(map[string]any)
			gpu, _ := size["Gpu"].(float64)
			if gpu != gpuCount {
				continue
			}
			collection, _ := size["Collection"].([]any)
			for _, c := range collection {
				col, _ := c.(map[string]any)
				cpu, _ := col["Cpu"].(float64)
				if cpu <= 0 {
					continue
				}
				mems, _ := col["Memory"].([]any)
				for _, m := range mems {
					memGB, _ := m.(float64)
					if memGB <= 0 {
						continue
					}
					memMB := memGB * 1024
					key := formatGuidedSpecKey(zone, gpu, cpu, memMB)
					if seen[key] {
						continue
					}
					seen[key] = true
					if current == "" {
						current = key
					}
					free, stockKnown := inventory.count(zone, gpuType)
					zoneLabel := zoneDisplayLabel(wfCtx, zone)
					note := fmt.Sprintf("%s · %.0f 张 GPU · %s", gpuType, gpu, zoneLabel)
					disabled := false
					disabledReason := ""
					if stockKnown {
						fit := guidedStockFitNote(free, gpu)
						note = fmt.Sprintf("%s · %.0f 张 GPU · %s · %s", gpuType, gpu, zoneLabel, fit)
					}
					// Gate by real creatability: disable a CPU/内存 combo only when the
					// capacity check enumerated it and found it short; combos it never
					// evaluated stay enabled (the final 检查库存 is authoritative).
					if capacityHasSignal(capSpecs) && capacityKnowsCombo(capSpecs, int(gpu), int(cpu), int(memGB)) && !capacityCPUMemEnough(capSpecs, int(gpu), int(cpu), int(memGB)) {
						disabled = true
						disabledReason = "该规格当前无可创建库存"
					}
					opts = append(opts, ConfirmFormOption{
						Value:    key,
						Label:    fmt.Sprintf("%.0f 核 CPU · %.0fGB 内存", cpu, memGB),
						Note:     note,
						Reason:   disabledReason,
						Disabled: disabled,
						Meta: map[string]string{
							"Zone":      zone,
							"ZoneLabel": zoneLabel,
							"GPU":       fmt.Sprintf("%.0f", gpu),
							"CPU":       fmt.Sprintf("%.0f", cpu),
							"MemoryGB":  fmt.Sprintf("%.0f", memGB),
						},
					})
					if stockKnown {
						opts[len(opts)-1].Meta["StockKnown"] = "true"
						opts[len(opts)-1].Meta["StockFree"] = fmt.Sprintf("%.0f", free)
					}
				}
			}
		}
	}
	// A capacity gate must never dead-end the flow: if it disabled every combo,
	// keep them selectable and let the authoritative 检查库存 report the shortage.
	if firstEnabledValue(opts) == "" {
		for i := range opts {
			opts[i].Disabled = false
			opts[i].Reason = ""
		}
	}
	if current != "" && (!seen[current] || !enabledOptionExists(opts, current)) {
		current = firstEnabledValue(opts)
	}
	if current == "" {
		current = firstEnabledValue(opts)
	}
	return current, opts
}

func formatGuidedSpecKey(zone string, gpu, cpu, memoryMB float64) string {
	return fmt.Sprintf("%s|%.0f|%.0f|%.0f", zone, gpu, cpu, memoryMB)
}

func parseGuidedSpecKey(key string) (zone string, gpu, cpu, memoryMB float64, err error) {
	parts := strings.Split(key, "|")
	if len(parts) != 4 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	gpu, err = strconv.ParseFloat(parts[1], 64)
	if err != nil || gpu <= 0 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	cpu, err = strconv.ParseFloat(parts[2], 64)
	if err != nil || cpu <= 0 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	memoryMB, err = strconv.ParseFloat(parts[3], 64)
	if err != nil || memoryMB <= 0 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	zone = strings.TrimSpace(parts[0])
	if zone == "" {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	return zone, gpu, cpu, memoryMB, nil
}

// guidedImageFormOptions returns the picker's current value, its options and the
// TOTAL number of candidates those options are a page of. The total is returned
// rather than inferred because the options are capped at maxGuidedImageOptions:
// the caller states "共 N 个" from the same set the options came from, so the card
// can no longer advertise a population it does not show.
func guidedImageFamilyFormOptionsForContext(wfCtx *Context) (string, []ConfirmFormOption, int) {
	if wfCtx == nil {
		return "", nil, 0
	}
	return guidedImageFamilyFormOptions(
		wfCtx.Params,
		createImageFamilies(wfCtx),
		paramStr(wfCtx.Params, "GpuType", ""),
	)
}

func guidedImageFamilyFormOptions(params map[string]any, families []deployment.ImageFamily, gpuType string) (string, []ConfirmFormOption, int) {
	current := strings.TrimSpace(paramStr(params, "ImageFamily", ""))
	total := 0
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	for _, family := range families {
		if family.Key == "" || seen[family.Key] {
			continue
		}
		name := strings.TrimSpace(family.Name)
		if name == "" && len(family.Variants) > 0 {
			name = family.Variants[0].FamilyLabel()
		}
		if name == "" {
			continue
		}
		seen[family.Key] = true
		total++
		disabled := false
		reason := ""
		if gpuType != "" && len(family.Variants) > 0 {
			anySupported := false
			for _, variant := range family.Variants {
				if len(variant.SupportedGPUTypes) == 0 || containsFold(variant.SupportedGPUTypes, gpuType) {
					anySupported = true
					break
				}
			}
			if !anySupported {
				disabled = true
				reason = "该系列没有支持当前 GPU 的版本"
			}
		}
		if len(opts) >= maxGuidedImageOptions {
			continue
		}
		opts = append(opts, ConfirmFormOption{
			Value:    family.Key,
			Label:    name,
			Note:     fmt.Sprintf("%d 个可选版本", len(family.Variants)),
			Reason:   reason,
			Disabled: disabled,
			Meta: map[string]string{
				"ImageFamily":  family.Key,
				"VariantCount": strconv.Itoa(len(family.Variants)),
			},
		})
	}
	if current == "" || !enabledOptionExists(opts, current) {
		current = firstEnabledValue(opts)
	}
	return current, opts, total
}

func selectedImageFamily(wfCtx *Context) (deployment.ImageFamily, bool) {
	if wfCtx == nil {
		return deployment.ImageFamily{}, false
	}
	key := strings.TrimSpace(paramStr(wfCtx.Params, "ImageFamily", ""))
	if key == "" {
		return deployment.ImageFamily{}, false
	}
	for _, family := range createImageFamilies(wfCtx) {
		if family.Key == key {
			return family, true
		}
	}
	return deployment.ImageFamily{}, false
}

func guidedImageFormOptions(params map[string]any, images map[string]any, gpuType string, taxonomy *deployment.ImageTaxonomy, zoneIsPod bool) (string, []ConfirmFormOption, int) {
	if images == nil {
		return "", nil, 0
	}
	set := buildImageCandidateSet(params, images, gpuType, taxonomy, zoneIsPod)
	return guidedImageFormOptionsFromSet(params, images, gpuType, set)
}

// guidedImageFormOptionsForContext reads the same structured request and selected
// facets as the other image cards.
func guidedImageFormOptionsForContext(wfCtx *Context, gpuType string) (string, []ConfirmFormOption, int) {
	if wfCtx == nil {
		return "", nil, 0
	}
	images := createImageResult(wfCtx)
	if images == nil {
		return "", nil, 0
	}
	// A concrete image is confirmed before hardware. Keep that choice available
	// even when a prefilled GPU is incompatible; the following GPU card owns the
	// compatible hardware choice. Status and zone/container filters still apply.
	if strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
		gpuType = ""
	}
	return guidedImageFormOptionsFromSet(
		wfCtx.Params,
		images,
		gpuType,
		createImageCandidates(wfCtx),
	)
}

func guidedImageFormOptionsFromSet(params map[string]any, images map[string]any, gpuType string, set imageCandidateSet) (string, []ConfirmFormOption, int) {
	snap := set.snap
	ranked := set.final
	wantType := strings.TrimSpace(paramStr(params, "ImageType", ""))
	wantTag := strings.TrimSpace(paramStr(params, "ImageTag", ""))

	// total counts every DISTINCT candidate this card is a page of, including the
	// threaded current selection when it is not one of the ranked rows. It is
	// counted here rather than from len(opts) because opts stops at the page size —
	// which is the whole bug this returns a total to close.
	total := 0
	counted := map[string]bool{}
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	appendOpt := func(id, label string, supported []string) {
		if id == "" {
			return
		}
		if !counted[id] {
			counted[id] = true
			total++
		}
		if seen[id] || len(opts) >= maxGuidedImageOptions {
			return
		}
		seen[id] = true
		note, reason := "", ""
		disabled := false
		// A GPU-recommendation mismatch is shown DISABLED (not hidden), so the user
		// sees why an image they might name is not selectable for this card.
		// Set only Reason because the client joins Note and disabled Reason.
		if gpuType != "" && len(supported) > 0 && !containsFold(supported, gpuType) {
			reason = "镜像不支持当前 GPU"
			disabled = true
		}
		opts = append(opts, ConfirmFormOption{
			Value: id, Label: label, Note: note, Reason: reason, Disabled: disabled,
			Meta: map[string]string{"ImageId": id},
		})
	}

	// Membership in the HARD-filtered candidate set (set.base is post-status,
	// post-pod/container; it does not yet apply the type/tag facets). The
	// current/threaded lead below must respect this: pickImageId resolves a default
	// from the raw catalog with NO pod constraint, so on a pod zone it can name a
	// VM-only image the candidate set already dropped. Leading with it re-added the
	// very image RankImages excluded — which is how a pinned pod zone still offered
	// "Ubuntu-nvidia 22.04" and the create gate refused it at the end.
	inCandidates := map[string]bool{}
	for _, sel := range set.base {
		inCandidates[sel.ID] = true
	}

	// The current/threaded selection leads — but only if it survives the active
	// facets AND the hard filters. A selection dropped by a facet is re-picked from
	// the facet-scoped candidates below; a selection dropped by the pod/status gate
	// is not a valid candidate at all and must not lead (or appear).
	current := pickImageId(params, images)
	if current != "" && imageSelectionMatchesFacets(snap, current, wantType, wantTag) {
		if entry, ok := snap.ByID(current); ok {
			if inCandidates[current] {
				appendOpt(entry.ID, entry.DisplayLabel(), entry.SupportedGPUTypes)
			} else {
				// Present in the catalog but dropped by a hard filter (e.g. a VM image
				// in a pod zone). Not a candidate — fall through to the ranked set.
				current = ""
			}
		} else {
			// An exact id reaches this picker only through the live catalog. The
			// workflow merges a resolver-verified page-out row before this function;
			// absence here therefore means it is not safe to offer.
			current = ""
		}
	} else {
		current = ""
	}
	for _, sel := range ranked {
		entry, ok := snap.ByID(sel.ID)
		// Label from the catalog row so two versions of one family are told apart by
		// their version; the ranked candidate carries only the (shared) family name.
		label := sel.Name
		if ok {
			if l := entry.DisplayLabel(); l != "" {
				label = l
			}
		}
		appendOpt(sel.ID, label, entry.SupportedGPUTypes)
	}
	if len(opts) == 0 {
		return "", nil, 0
	}
	if current == "" || !seen[current] || !enabledOptionExists(opts, current) {
		current = firstEnabledValue(opts)
	}
	return current, opts, total
}
