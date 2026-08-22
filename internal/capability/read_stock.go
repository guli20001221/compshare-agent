package capability

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/zones"
)

// Stock availability is the heaviest read capability: a plain listing renders the catalog
// Normal/SoldOut status, but a matched-model turn runs a multi-call capacity
// precheck (support zones + a probe image + a per-model /
// per-zone CheckCompShareResourceCapacity) with graceful partial-failure
// handling. The typed request is the only filter source. Follow-up context is
// handled by the model from canonical transcript, never by server-side argument
// rewriting.

const (
	stockCapabilityLabel = string(intent.IntentStockAvailability)
	stockAction          = "DescribeAvailableCompShareInstanceTypes"

	noStockReply = "未获取到机型库存数据。"
	// Catalog counts are snapshots; concrete configurations still need a capacity check.
	soldOutDisclaimer  = "数量为本轮实时快照，仅供参考；某个具体配置能否创建仍需容量预检。"
	stockListingHeader = "当前可售机型："

	// Terra fills optional enum properties even when the user did not choose one.
	// Represent the absent state explicitly so the first business value
	// (Exclusive) cannot silently become a user selection.
	stockInventoryPoolUnspecified = "Unspecified"
)

// StockAvailabilityRequest is the capability's own request contract.
type StockAvailabilityRequest struct {
	GPUType       string   `json:"gpu_type,omitempty"`
	ZoneMentions  []string `json:"zone_mentions,omitempty"`
	InventoryPool string   `json:"inventory_pool,omitempty"`
}

// MissingFields: none — an unfiltered stock listing is valid.
func (StockAvailabilityRequest) MissingFields() []platform.MissingField { return nil }

// StockAvailabilityResponse carries the rendered reply and the optional evidence
// envelope (only the plain-listing path builds one). The source fan-out lives in
// the handler, so the renderer is a trivial projection of the response.
type StockAvailabilityResponse struct {
	Reply    string
	Envelope *envelope.Envelope
}

type stockInstanceTypeEntry struct {
	Name   string
	Status string
	Zone   string
}

type stockCapacityCheck struct {
	Name          string
	Zone          string
	CanonicalZone string
	CheckedSpec   int
	EnoughSpecs   []capacitySpec
	Failed        bool
}

// capacitySpec is one precheck-passing configuration. GPUCount is kept apart
// from the full label because the reply groups by card count: a zone that passes
// eight CPU/memory variants of the same card counts is one line, not eight.
type capacitySpec struct {
	GPUCount string
	Label    string
}

// stockCapabilityDescription states this capability's own boundary. "接口不提供补货或
// 到货时间" belongs in it: the upstream inventory APIs return a current count and
// nothing about future supply, so 「什么时候补货」 has no source to answer from and
// has to come back as a limit rather than an estimate.
const stockCapabilityDescription = "查询 GPU 机型在各可用区的实时库存，并在可能时做配置样本预检。库存数量来自本轮实时快照，仅作当前参考，接口不提供补货或到货时间；最终可创建性结合配置预检判断。规格参数查询使用 GPU 规格能力。"

// The first call preserves the user's wording. If it does not match the live
// catalog, the result gives the Agent that catalog and asks it to correct its own
// call; a later call may therefore carry an exact catalog value instead.
const stockZoneMentionsDescription = "首次调用填写用户本轮明确提到的可用区原文；工具返回实时目录后，语义唯一时可改用目录中的完整名称或 ZoneID 重试，存在多个合理候选时再请用户选择。不得默认区域或使用目录外的值。"

func stockReadSpec() ReadCapabilitySpec[StockAvailabilityRequest, StockAvailabilityResponse] {
	return ReadCapabilitySpec[StockAvailabilityRequest, StockAvailabilityResponse]{
		Label:       stockCapabilityLabel,
		Description: stockCapabilityDescription,
		Params: objectParam(map[string]schemaNode{
			// Deliberately undescribed. A description telling the model to carry the
			// card forward when the user elides it would be the natural companion to
			// deleting the server-side carry — but it costs ~200 bytes on EVERY
			// Canonical transcript replay gives the model the earlier stock call;
			// keep the schema concise and let it carry the referenced GPU explicitly.
			"gpu_type":       stringParam(),
			"zone_mentions":  arrayParam(stringParam()).described(stockZoneMentionsDescription),
			"inventory_pool": enumParam(stockInventoryPoolUnspecified, deployment.GPUInventoryPoolExclusive, deployment.GPUInventoryPoolSpot).described("用户明确询问独占库存时填 Exclusive，明确询问抢占式库存时填 Spot；未限定时填 Unspecified。"),
		}),
		Handle: stockHandle,
		Render: stockRender,
	}
}

func stockHandle(ctx context.Context, req StockAvailabilityRequest, rt ReadRuntime) (StockAvailabilityResponse, ReadResult) {
	raw, err := rt.Executor.Execute(ctx, stockAction, map[string]any{})
	if err != nil {
		return StockAvailabilityResponse{}, ReadFailureAfterTool(stockAction, stockCapabilityLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		// Query succeeded but no machine-type stock data — a structured Empty read.
		return StockAvailabilityResponse{}, ReadEmpty(noStockReply)
	}
	// The filter is the request's own gpu_type and nothing else. All three stock
	// renderers read this one value, so they stay consistent with each other and
	// with what the model actually asked for.
	referent := strings.TrimSpace(req.GPUType)

	if reply, ok, terminal := stockCapacityPrecheck(ctx, rt, req, raw); terminal.Status != "" {
		return StockAvailabilityResponse{}, terminal
	} else if ok {
		return StockAvailabilityResponse{Reply: reply}, ReadResult{}
	}
	// A catalog listing that only says 开售 does not answer "有多少". The card
	// counts live in a different API (DescribeCompShareGpuInventory), which the
	// capacity path already reads but this path did not, so a plain listing was
	// Normal/SoldOut and nothing else. Both extra reads are tolerant: a failure
	// here degrades to the sale-state-only listing rather than failing the turn.
	var inventory *deployment.GPUInventorySnapshot
	listingZones, zoneErr := stockSupportZones(ctx, rt)
	if zoneErr == nil {
		inventory = stockGPUInventorySnapshot(ctx, rt, listingZones)
	}
	return StockAvailabilityResponse{
		Reply:    renderStockReply(raw, referent, inventory, listingZones),
		Envelope: populatedEnvelope(buildStockEnvelope(raw, referent)),
	}, ReadResult{}
}

func stockRender(resp StockAvailabilityResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = stockAction
	r.Envelope = resp.Envelope
	return r
}

// stockModelCardTotals sums a model's card counts across every zone the catalog
// knows. The per-zone breakdown belongs to a zone-specific question; a "有什么卡"
// listing wants one number per model. Returns ok=false when the inventory source
// did not answer for this model at all — the caller then says nothing about
// quantity rather than printing a zero it cannot stand behind.
func stockModelCardTotals(snapshot *deployment.GPUInventorySnapshot, supportZones []zones.ZoneInfo, model string) (exclusive, spot uint32, ok bool) {
	if snapshot == nil {
		return 0, 0, false
	}
	for _, zone := range supportZones {
		zoneExclusive, zoneSpot, present, available := snapshot.Counts(zone.Zone, model)
		if !present || !available {
			continue
		}
		exclusive += zoneExclusive
		spot += zoneSpot
		ok = true
	}
	return exclusive, spot, ok
}

// stockCardCountSuffix renders the quantity clause for one machine-type line.
// 约 is not hedging for its own sake: the snapshot has an UpdateTime and is not
// transactional, so an exact-sounding number would overstate what we know.
func stockCardCountSuffix(snapshot *deployment.GPUInventorySnapshot, supportZones []zones.ZoneInfo, model string) string {
	exclusive, spot, ok := stockModelCardTotals(snapshot, supportZones, model)
	if !ok {
		return ""
	}
	var pools []string
	if exclusive > 0 {
		pools = append(pools, fmt.Sprintf("独占约 %d 张", exclusive))
	}
	if spot > 0 {
		pools = append(pools, fmt.Sprintf("抢占约 %d 张", spot))
	}
	if len(pools) == 0 {
		return "，当前库存快照为 0"
	}
	return "，" + strings.Join(pools, " / ")
}

func renderStockReply(raw map[string]any, userText string, inventory *deployment.GPUInventorySnapshot, supportZones []zones.ZoneInfo) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noStockReply
	}
	matched := matchUserTextToInstanceTypeNames(userText, items, false)

	var prefix string
	filterTo := map[string]struct{}{}
	if len(matched) > 0 {
		for _, m := range matched {
			filterTo[m] = struct{}{}
		}
	} else if strings.TrimSpace(userText) != "" {
		prefix = "未在当前可售机型里找到您提到的型号。以下是当前可售机型库存：\n"
	}

	lines := make([]string, 0, len(items))
	seenNames := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		if len(filterTo) > 0 {
			if _, ok := filterTo[name]; !ok {
				continue
			}
		}
		if _, ok := seenNames[name]; ok {
			continue // dedupe API duplicates across zones
		}
		seenNames[name] = struct{}{}
		status := safeString(entry, "Status")
		if status == "" {
			// Some prod responses omit Status; "appears in available list" ≈ available.
			status = "Normal"
		}
		lines = append(lines, renderStockStatusLine(name, status)+stockCardCountSuffix(inventory, supportZones, name))
	}
	if len(lines) == 0 {
		if prefix != "" {
			return strings.TrimRight(prefix, "\n") + "\n" + soldOutDisclaimer
		}
		return noStockReply
	}
	// The header is only useful when nothing above it already said what the list
	// is; the no-match prefix does.
	if prefix == "" {
		prefix = stockListingHeader + "\n"
	}
	return prefix + strings.Join(lines, "\n") + "\n" + soldOutDisclaimer
}

// renderStockStatusLine translates known upstream sale-state values.
func renderStockStatusLine(name, status string) string {
	switch {
	case strings.EqualFold(status, "Normal"):
		return fmt.Sprintf("- %s：开售", name)
	case strings.EqualFold(status, "SoldOut"):
		return fmt.Sprintf("- %s：售罄", name)
	default:
		return fmt.Sprintf("- %s：%s", name, status)
	}
}

func buildStockEnvelope(raw map[string]any, userText string) envelope.Envelope {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	matched := matchUserTextToInstanceTypeNames(userText, items, false)
	filterTo := map[string]struct{}{}
	for _, m := range matched {
		filterTo[m] = struct{}{}
	}

	env := envelope.Envelope{
		Kind:          envelope.KindStockAvailability,
		SourceActions: []string{"DescribeAvailableCompShareInstanceTypes"},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints:   envelope.Constraints{DoNotInventInstances: true},
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "disclaimer", Label: "Disclaimer", Value: soldOutDisclaimer, Source: envelope.FactSourceComputed},
	)

	seenNames := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		if len(filterTo) > 0 {
			if _, ok := filterTo[name]; !ok {
				continue
			}
		}
		if _, ok := seenNames[name]; ok {
			continue
		}
		seenNames[name] = struct{}{}
		status := safeString(entry, "Status")
		if status == "" {
			status = "Normal"
		}
		subjectID := "stock:" + name
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID: subjectID, Name: name, Type: envelope.SubjectGPUModel,
		})
		env.Facts = append(env.Facts,
			envelope.Fact{SubjectID: subjectID, Key: "model_name", Label: "机型", Value: name, Source: envelope.FactSourceAPI},
			envelope.Fact{SubjectID: subjectID, Key: "status", Label: "状态", Value: status, Source: envelope.FactSourceAPI},
		)
	}
	if len(matched) == 0 && strings.TrimSpace(userText) != "" {
		env.Computed = append(env.Computed,
			envelope.Fact{Key: "no_match_hint", Label: "未找到匹配", Value: "未在当前可售机型里找到您提到的型号", Source: envelope.FactSourceComputed},
		)
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "total_count", Label: "Total count", Value: len(env.Subjects), Source: envelope.FactSourceComputed},
	)
	return env
}

// stockCapacityPrecheck returns (reply, ok, terminal). ok=false falls through
// to the plain listing; a terminal status is an upstream failure.
func stockCapacityPrecheck(ctx context.Context, rt ReadRuntime, req StockAvailabilityRequest, stockRaw map[string]any) (string, bool, ReadResult) {
	entries := matchedNormalStockEntries(stockRaw, strings.TrimSpace(req.GPUType))
	if len(entries) == 0 {
		return "", false, ReadResult{}
	}
	supportZones, err := stockSupportZones(ctx, rt)
	if err != nil {
		return "", false, ReadFailureAfterTool("DescribeCompShareSupportZone", stockCapabilityLabel, err)
	}
	filter, unresolved := stockZoneFilterFromMentions(req.ZoneMentions, supportZones)
	if len(unresolved) > 0 {
		return "", false, unmatchedZoneMentionResult(unresolved, supportZones)
	}
	allEntries := append([]stockInstanceTypeEntry(nil), entries...)
	_, modelOrder := groupStockEntriesByModel(allEntries)
	inventory := stockGPUInventorySnapshot(ctx, rt, supportZones)
	inventoryReply := renderStockGPUInventory(inventory, modelOrder, filter, supportZones)
	if len(filter) > 0 {
		entries = filterStockEntriesByZone(entries, filter)
		if len(entries) == 0 {
			return joinStockReply(renderRequestedZonesNotOffered(req.GPUType, filter, supportZones), inventoryReply), true, ReadResult{}
		}
	}
	entriesByModel, modelOrder := groupStockEntriesByModel(entries)
	imageRaw, err := rt.Executor.Execute(ctx, "DescribeCompShareImages", map[string]any{
		"ImageType": "System",
		"Limit":     20,
	})
	if err != nil {
		return "", false, ReadFailureAfterTool("DescribeCompShareImages", stockCapabilityLabel, err)
	}
	if imageRaw == nil {
		imageRaw = map[string]any{}
	}
	imageID := selectCapacityPrecheckImageID(imageRaw)
	if imageID == "" {
		return joinStockReply(renderStockCapacityReply(failedStockCapacityChecks(entriesByModel, modelOrder))+"\n容量预检未执行：未获取到可用于预检的系统镜像。", inventoryReply), true, ReadResult{}
	}

	checks := make([]stockCapacityCheck, 0, len(entries))

	for _, model := range modelOrder {
		zoneEntries := entriesByModel[model]
		for _, entry := range zoneEntries {
			if entry.Zone == "" {
				continue
			}
			zoneLabel := stockZoneDisplay(entry, supportZones)
			args := capacityPrecheckArgs(entry, imageID, supportZones, stockRaw, imageRaw, req.InventoryPool)
			capacityRaw, err := stockExecuteCapacityPrecheck(ctx, rt, args)
			if err != nil {
				checks = append(checks, stockCapacityCheck{Name: model, Zone: zoneLabel, CanonicalZone: entry.Zone, Failed: true})
				continue
			}
			check := summarizeStockCapacity(entry, capacityRaw)
			check.Zone = zoneLabel
			checks = append(checks, check)
		}
	}
	if len(checks) == 0 {
		return joinStockReply(renderStockCapacityReply(failedStockCapacityChecks(entriesByModel, modelOrder))+"\n容量预检未执行：当前接口结果缺少可用区信息。", inventoryReply), true, ReadResult{}
	}
	if pool := normalizeInventoryPool(req.InventoryPool); pool != "" {
		eligibleChecks, poolReply := requestedInventoryPoolView(inventory, checks, pool)
		return joinStockReply(renderStockCapacityReply(eligibleChecks), poolReply), true, ReadResult{}
	}
	// Keep inventory counts inline with their zone so the answer remains one table.
	return renderStockCapacityReplyWithCounts(checks, inventory), true, ReadResult{}
}

func stockGPUInventorySnapshot(ctx context.Context, rt ReadRuntime, supportZones []zones.ZoneInfo) *deployment.GPUInventorySnapshot {
	catalog := stockZoneCatalogSnapshot(supportZones)
	officialArgs := stockIdentityArgs(rt)
	official, officialErr := rt.Executor.ExecuteInternal(ctx, "DescribeCompShareGpuInventory", officialArgs)
	officialAvailable := officialErr == nil && deployment.GPUInventoryPayloadAvailable(official)

	var pod map[string]any
	podAttempted := false
	podAvailable := false
	if selector, ok := deployment.PodSelectorZoneID(catalog); ok {
		podAttempted = true
		var podErr error
		podArgs := stockIdentityArgs(rt)
		podArgs["zone_id"] = selector
		pod, podErr = rt.Executor.ExecuteInternal(ctx, "DescribeCompShareGpuInventory", podArgs)
		podAvailable = podErr == nil && deployment.GPUInventoryPayloadAvailable(pod)
	}
	return deployment.NewGPUInventorySnapshot(
		catalog,
		official, true, officialAvailable,
		pod, podAttempted, podAvailable,
	)
}

func stockIdentityArgs(rt ReadRuntime) map[string]any {
	args := map[string]any{}
	if rt.TopOrganizationID != 0 {
		args["top_organization_id"] = rt.TopOrganizationID
	}
	if rt.OrganizationID != 0 {
		args["organization_id"] = rt.OrganizationID
	}
	return args
}

func stockZoneCatalogSnapshot(list []zones.ZoneInfo) *deployment.ZoneCatalogSnapshot {
	entries := make([]deployment.ZoneCatalogEntry, 0, len(list))
	for _, zone := range list {
		entries = append(entries, deployment.ZoneCatalogEntry{
			Placement: deployment.ZonePlacement{
				Zone: zone.Zone, Region: zone.Region, ZoneID: zone.ZoneID,
				AzGroup: zone.RegionID, IsPod: zone.IsPod,
			},
			DisplayName:           zone.Describe,
			DisableImageSync:      zone.DisableImageSync,
			UnsupportedImageTypes: append([]string(nil), zone.UnsupportedImageTypes...),
		})
	}
	return deployment.NewZoneCatalogSnapshot(true, entries)
}

func renderStockGPUInventory(snapshot *deployment.GPUInventorySnapshot, models []string, filter map[string]struct{}, supportZones []zones.ZoneInfo) string {
	if snapshot == nil || len(models) == 0 {
		return ""
	}
	if len(filter) == 0 &&
		!snapshot.SourceState(deployment.GPUInventorySourceOfficial).Available &&
		!snapshot.SourceState(deployment.GPUInventorySourcePod).Available {
		return ""
	}
	var rows []string
	for _, zone := range supportZones {
		if len(filter) > 0 {
			if _, ok := filter[strings.ToLower(zone.Zone)]; !ok {
				continue
			}
		}
		for _, model := range models {
			exclusive, spot, present, available := snapshot.Counts(zone.Zone, model)
			label := fmt.Sprintf("%s / %s", zones.Label(supportZones, zone.Zone), model)
			switch {
			case !available:
				rows = append(rows, label+"：对应库存源本轮不可用")
			case !present:
				rows = append(rows, label+"：库存接口未返回该型号数量")
			default:
				var pools []string
				if exclusive > 0 {
					pools = append(pools, fmt.Sprintf("独占约 %d 张", exclusive))
				}
				if spot > 0 {
					pools = append(pools, fmt.Sprintf("抢占约 %d 张", spot))
				}
				if len(pools) == 0 {
					pools = append(pools, "当前库存快照为 0")
				}
				rows = append(rows, label+"："+strings.Join(pools, "、"))
			}
		}
	}
	if len(rows) == 0 {
		return ""
	}
	return "原始 GPU 库存快照（只用于补充区域信息，不等同于最终可创建性）：" + strings.Join(rows, "；") + "。"
}

func joinStockReply(primary, inventory string) string {
	primary = strings.TrimSpace(primary)
	inventory = strings.TrimSpace(inventory)
	if primary == "" {
		return inventory
	}
	if inventory == "" {
		return primary
	}
	return primary + "\n" + inventory
}

func normalizeInventoryPool(pool string) string {
	switch {
	case strings.EqualFold(strings.TrimSpace(pool), deployment.GPUInventoryPoolExclusive):
		return deployment.GPUInventoryPoolExclusive
	case strings.EqualFold(strings.TrimSpace(pool), deployment.GPUInventoryPoolSpot):
		return deployment.GPUInventoryPoolSpot
	default:
		return ""
	}
}

// requestedInventoryPoolView treats pool membership as product support and the
// numeric count as a point-in-time snapshot. Unsupported rows are excluded;
// supported zeroes remain eligible and never become a global no-stock claim.
func requestedInventoryPoolView(snapshot *deployment.GPUInventorySnapshot, checks []stockCapacityCheck, pool string) ([]stockCapacityCheck, string) {
	if snapshot == nil || len(checks) == 0 {
		return checks, ""
	}
	pool = normalizeInventoryPool(pool)
	if pool == "" {
		return checks, ""
	}
	poolLabel := "独占"
	if pool == deployment.GPUInventoryPoolSpot {
		poolLabel = "抢占式"
	}
	rows := make([]string, 0, len(checks))
	eligible := make([]stockCapacityCheck, 0, len(checks))
	seen := map[string]struct{}{}
	for _, check := range checks {
		supported, known := snapshot.PoolSupported(check.CanonicalZone, check.Name, pool)
		if known && !supported {
			key := check.CanonicalZone + "\x00" + check.Name
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				rows = append(rows, fmt.Sprintf("%s 的 %s 当前不支持%s购买方式", check.Zone, check.Name, poolLabel))
			}
			continue
		}
		eligible = append(eligible, check)
		key := check.CanonicalZone + "\x00" + check.Name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		target := fmt.Sprintf("%s 的 %s", check.Zone, check.Name)
		if !known {
			rows = append(rows, fmt.Sprintf("%s 本轮未确认%s购买方式的支持状态", target, poolLabel))
			continue
		}
		count, present, available := snapshot.PoolCount(check.CanonicalZone, check.Name, pool)
		if !available || !present {
			rows = append(rows, fmt.Sprintf("%s 支持%s购买方式；库存接口本轮未返回该型号的数量", target, poolLabel))
			continue
		}
		if count > 0 {
			rows = append(rows, fmt.Sprintf("%s 支持%s购买方式；原始库存快照约 %d 张", target, poolLabel, count))
			continue
		}
		rows = append(rows, fmt.Sprintf("%s 支持%s购买方式；本轮原始库存快照为 0，但这不等同于无法创建", target, poolLabel))
	}
	if len(rows) == 0 {
		return eligible, ""
	}
	// One line per 可用区+机型, for the same reason the capacity block is grouped:
	// joined with "；" this was a single unreadable sentence once more than a
	// couple of zones came back.
	sort.Strings(rows)
	return eligible, "- " + strings.Join(rows, "\n- ")
}

func failedStockCapacityChecks(entriesByModel map[string][]stockInstanceTypeEntry, modelOrder []string) []stockCapacityCheck {
	var checks []stockCapacityCheck
	for _, model := range modelOrder {
		entries := entriesByModel[model]
		if len(entries) == 0 {
			continue
		}
		zone := entries[0].Zone
		checks = append(checks, stockCapacityCheck{Name: model, Zone: zone, CanonicalZone: zone, Failed: true})
	}
	return checks
}

func groupStockEntriesByModel(entries []stockInstanceTypeEntry) (map[string][]stockInstanceTypeEntry, []string) {
	entriesByModel := map[string][]stockInstanceTypeEntry{}
	modelOrder := []string{}
	for _, entry := range entries {
		if _, ok := entriesByModel[entry.Name]; !ok {
			modelOrder = append(modelOrder, entry.Name)
		}
		entriesByModel[entry.Name] = append(entriesByModel[entry.Name], entry)
	}
	return entriesByModel, modelOrder
}

func stockSupportZones(ctx context.Context, rt ReadRuntime) ([]zones.ZoneInfo, error) {
	raw, err := rt.Executor.ExecuteInternal(ctx, "DescribeCompShareSupportZone", stockIdentityArgs(rt))
	if err != nil {
		return nil, err
	}
	list := zones.ParseSupportZones(raw)
	if len(list) == 0 {
		return nil, fmt.Errorf("平台可用区目录为空")
	}
	return list, nil
}

// unmatchedZoneMentionResult answers a zone mention that names nothing in the
// live catalog by handing over the catalog itself.
//
// It is deliberately NOT ReadConflict. Conflict means "this resolved to several
// candidates, pick one", and the engine turns it into choose_alternative /
// AMBIGUOUS_SELECTION / next_step=ask_user_to_choose — a shape whose only
// recovery, per the tool-result contract the model is given, is to correct its
// own call from the live catalog. Reporting "no match" as "several matches" made
// that impossible and sent the question back to the user.
//
// What the capability CAN state is a fact it just fetched. This layer normalizes
// format, never meaning — no alias table, no 2↔二 mapping, no city keywords —
// so whether "华北2a" denotes 华北二A is a semantic judgment, and the Agent is
// the component that can make it. It only needs to see the alternatives, and
// they were already in this function's locals. The catalog is rendered by the
// same producer the zone-catalog capability uses, so the two cannot drift into
// describing the same zones differently.
func unmatchedZoneMentionResult(unresolved []string, supportZones []zones.ZoneInfo) ReadResult {
	result := zoneCatalogRender(ZoneCatalogResponse{
		Records: zoneCatalogRecords(stockZoneCatalogSnapshot(supportZones)),
	})
	quoted := make([]string, 0, len(unresolved))
	for _, mention := range unresolved {
		quoted = append(quoted, "「"+mention+"」")
	}
	// Facts only. The engine supplies the corrective instruction on the outer
	// AgentTool result; this text only explains what the live lookup established.
	result.Reply = fmt.Sprintf(
		"%s不是当前实时可用区目录中的可用区名称或 ZoneID，本次未按可用区查询库存。\n当前实时可用区目录：\n%s",
		strings.Join(quoted, "、"), result.Reply)
	result.Status = platform.ReadStatusNeedsInput
	result.FallbackReason = platform.ReadFallbackValidation
	return result
}

func stockZoneFilterFromMentions(mentions []string, supportZones []zones.ZoneInfo) (map[string]struct{}, []string) {
	if len(mentions) == 0 {
		return nil, nil
	}
	filter := map[string]struct{}{}
	var unresolved []string
	for _, mention := range mentions {
		mention = strings.TrimSpace(mention)
		if mention == "" {
			continue
		}
		exact, ok := zones.ExactZone(supportZones, mention)
		if !ok {
			unresolved = append(unresolved, mention)
			continue
		}
		filter[strings.ToLower(exact)] = struct{}{}
	}
	return filter, unresolved
}

func renderRequestedZonesNotOffered(gpuType string, filter map[string]struct{}, supportZones []zones.ZoneInfo) string {
	labels := make([]string, 0, len(filter))
	for _, zone := range supportZones {
		if _, ok := filter[strings.ToLower(zone.Zone)]; ok {
			labels = append(labels, zones.Label(supportZones, zone.Zone))
		}
	}
	sort.Strings(labels)
	return fmt.Sprintf("%s 未在指定可用区（%s）的开售目录中出现；未执行跨区容量预检，也不能据此推断其他区域库存。", strings.TrimSpace(gpuType), strings.Join(labels, "、"))
}

func filterStockEntriesByZone(entries []stockInstanceTypeEntry, filter map[string]struct{}) []stockInstanceTypeEntry {
	if len(filter) == 0 {
		return entries
	}
	out := make([]stockInstanceTypeEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := filter[strings.ToLower(entry.Zone)]; ok {
			out = append(out, entry)
		}
	}
	return out
}

func matchedNormalStockEntries(raw map[string]any, userText string) []stockInstanceTypeEntry {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return nil
	}
	matchedNames := matchUserTextToInstanceTypeNames(userText, items, false)
	if len(matchedNames) == 0 {
		return nil
	}
	wanted := map[string]struct{}{}
	for _, name := range matchedNames {
		wanted[name] = struct{}{}
	}
	out := []stockInstanceTypeEntry{}
	seen := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if _, ok := wanted[name]; !ok {
			continue
		}
		status := safeString(entry, "Status")
		if status == "" {
			status = "Normal"
		}
		if !strings.EqualFold(status, "Normal") {
			continue
		}
		zone := safeString(entry, "Zone")
		key := name + "\x00" + zone
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, stockInstanceTypeEntry{
			Name:   name,
			Status: status,
			Zone:   zone,
		})
	}
	return out
}

func capacityPrecheckArgs(entry stockInstanceTypeEntry, imageID string, supportZones []zones.ZoneInfo, catalog, images map[string]any, inventoryPool string) map[string]any {
	chargeType := deployment.ChargeTypePostpay
	if normalizeInventoryPool(inventoryPool) == deployment.GPUInventoryPoolSpot {
		chargeType = deployment.ChargeTypeSpot
	}
	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:             entry.Zone,
		GPUType:          entry.Name,
		CompShareImageID: imageID,
		ChargeType:       chargeType,
		Disks:            deployment.ResolveBootDisk(images, catalog, imageID, entry.Name, entry.Zone),
	})
	placement := deployment.ZonePlacement{
		Zone:   entry.Zone,
		Region: stockRegionFromZone(entry.Zone),
	}
	if zone := stockZoneInfoForEntry(entry, supportZones); zone.Zone != "" {
		placement.Zone = zone.Zone
		if zone.Region != "" {
			placement.Region = zone.Region
		}
		placement.ZoneID = zone.ZoneID
		placement.AzGroup = zone.RegionID
		placement.IsPod = zone.IsPod
	}
	return deployment.ApplyCapacityPlacementArgs(args, placement)
}

func stockExecuteCapacityPrecheck(ctx context.Context, rt ReadRuntime, args map[string]any) (map[string]any, error) {
	return rt.Executor.ExecuteInternal(ctx, "CheckCompShareResourceCapacity", args)
}

func stockZoneInfoForEntry(entry stockInstanceTypeEntry, supportZones []zones.ZoneInfo) zones.ZoneInfo {
	for _, z := range supportZones {
		if z.Zone != "" && strings.EqualFold(z.Zone, entry.Zone) {
			return z
		}
	}
	return zones.ZoneInfo{}
}

func stockRegionFromZone(zone string) string {
	zone = strings.TrimSpace(zone)
	if zone == "" || strings.Count(zone, "-") < 2 {
		return ""
	}
	idx := strings.LastIndex(zone, "-")
	if idx <= 0 {
		return ""
	}
	return zone[:idx]
}

// selectCapacityPrecheckImageID returns any Available System image id to satisfy the
// capacity API's REQUIRED CompShareImageId param (CheckCompShareResourceCapacity returns
// RetCode 230 without it). It is deliberately keyword-free — NO ubuntu/nvidia/cuda name
// scoring (that was a second image interpreter the image convergence removed everywhere
// else) and a stock turn carries no user image request to resolve — so it just returns
// the first Available System row.
//
// This image is a SAMPLE probe parameter, NOT a representative or authoritative one, and
// the choice is NOT inert: per upstream CheckCompShareResourceCapacity the image feeds
// the OS type and boot-disk sizing into the scheduler simulation, and
// DescribeCompShareImages returns rows in resource-search order (there is no "first row
// is the canonical probe image" contract). So a precheck that runs with this image and
// returns all ResourceEnough=false is a SAMPLE negative only — it must NOT be
// generalized to "this GPU has no stock" (see renderStockInventoryCapacityReply). Only a
// POSITIVE result confirms creatability; the authoritative negative comes from the create
// workflow re-checking the user's final sealed image/zone/disk/spec.
//
// Returns "" when no usable row — the caller then surfaces an honest 容量预检未执行
// message rather than defaulting to some image.
func selectCapacityPrecheckImageID(raw map[string]any) string {
	for _, item := range mapSliceAt(raw, "ImageSet") {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := safeString(entry, "CompShareImageId")
		if id == "" {
			continue
		}
		status := safeString(entry, "Status")
		if status != "" && !strings.EqualFold(status, "Available") && !strings.EqualFold(status, "Normal") {
			continue
		}
		return id
	}
	return ""
}

func summarizeStockCapacity(entry stockInstanceTypeEntry, raw map[string]any) stockCapacityCheck {
	check := stockCapacityCheck{Name: entry.Name, Zone: entry.Zone, CanonicalZone: entry.Zone}
	for _, item := range mapSliceAt(raw, "Specs") {
		spec, ok := item.(map[string]any)
		if !ok {
			continue
		}
		check.CheckedSpec++
		if resourceEnough(spec["ResourceEnough"]) {
			if label := capacitySpecLabel(spec); label != "" {
				check.EnoughSpecs = append(check.EnoughSpecs, capacitySpec{
					GPUCount: capacitySpecGPUCount(spec), Label: label,
				})
			}
		}
	}
	return check
}

func resourceEnough(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

// capacitySpecGPUCount reads the card count off a precheck spec. Empty when the
// upstream row carries none — an unlabelled count is grouped under "" rather
// than invented.
func capacitySpecGPUCount(spec map[string]any) string {
	gpu := fmt.Sprint(spec["Gpu"])
	if gpu == "" || gpu == "<nil>" {
		return ""
	}
	return gpu
}

func capacitySpecLabel(spec map[string]any) string {
	gpu := fmt.Sprint(spec["Gpu"])
	cpu := fmt.Sprint(spec["Cpu"])
	mem := fmt.Sprint(spec["Mem"])
	parts := []string{}
	if gpu != "" && gpu != "<nil>" {
		parts = append(parts, gpu+"卡")
	}
	if cpu != "" && cpu != "<nil>" {
		parts = append(parts, cpu+"C")
	}
	if mem != "" && mem != "<nil>" {
		parts = append(parts, mem+"G")
	}
	return strings.Join(parts, "/")
}

// renderStockCapacityReplyWithCounts is renderStockCapacityReply plus the card
// count on each 机型/可用区 line. Split rather than merged so the many callers
// that have no inventory snapshot (degraded paths, failure notes) keep the
// count-free rendering without threading a nil through each one.
func renderStockCapacityReplyWithCounts(checks []stockCapacityCheck, inventory *deployment.GPUInventorySnapshot) string {
	return renderStockCapacityReplyLines(checks, inventory)
}

func renderStockCapacityReply(checks []stockCapacityCheck) string {
	return renderStockCapacityReplyLines(checks, nil)
}

// zoneCardCountSuffix renders the pool counts for one exact (model, zone) pair.
// CanonicalZone is the id the inventory snapshot is keyed by; Zone is the
// display label, which would not resolve.
func zoneCardCountSuffix(inventory *deployment.GPUInventorySnapshot, model, canonicalZone string) string {
	if inventory == nil || canonicalZone == "" {
		return ""
	}
	exclusive, spot, present, available := inventory.Counts(canonicalZone, model)
	if !present || !available {
		return ""
	}
	var pools []string
	if exclusive > 0 {
		pools = append(pools, fmt.Sprintf("独占约 %d 张", exclusive))
	}
	if spot > 0 {
		pools = append(pools, fmt.Sprintf("抢占约 %d 张", spot))
	}
	if len(pools) == 0 {
		return "；库存快照为 0"
	}
	return "；" + strings.Join(pools, " / ")
}

func renderStockCapacityReplyLines(checks []stockCapacityCheck, inventory *deployment.GPUInventorySnapshot) string {
	if len(checks) == 0 {
		return ""
	}
	names := make([]string, 0, len(checks))
	seenNames := map[string]struct{}{}
	var failedZones []string
	checkedSpecs := 0
	// Group the passing configurations by 机型+可用区 and keep only the distinct card
	// counts. Listing every configuration flattened three levels into one sentence:
	// three zones x eight CPU/memory variants was 24 items joined by "、", and this
	// block is inserted verbatim, so what the user reads is exactly what is built
	// here. The dropped dimension is stated in the header rather than silently lost.
	type capacityGroup struct{ name, zone string }
	countsByGroup := map[capacityGroup][]string{}
	canonicalByGroup := map[capacityGroup]string{}
	groupOrder := make([]capacityGroup, 0, len(checks))
	for _, check := range checks {
		if _, ok := seenNames[check.Name]; !ok {
			seenNames[check.Name] = struct{}{}
			names = append(names, check.Name)
		}
		if check.Failed {
			failedZones = append(failedZones, check.Zone)
			continue
		}
		checkedSpecs += check.CheckedSpec
		for _, spec := range check.EnoughSpecs {
			group := capacityGroup{check.Name, check.Zone}
			// The group is keyed by the DISPLAY zone label; the inventory snapshot is
			// keyed by the zone id. Keep the id alongside so the card count can be
			// looked up — 华北二A (cn-wlcb-01) would not resolve.
			canonicalByGroup[group] = check.CanonicalZone
			if _, seen := countsByGroup[group]; !seen {
				groupOrder = append(groupOrder, group)
			}
			if !containsString(countsByGroup[group], spec.GPUCount) {
				countsByGroup[group] = append(countsByGroup[group], spec.GPUCount)
			}
		}
	}
	sort.Strings(names)
	models := strings.Join(names, "、")
	if len(groupOrder) > 0 {
		sort.Slice(groupOrder, func(i, j int) bool {
			if groupOrder[i].name != groupOrder[j].name {
				return groupOrder[i].name < groupOrder[j].name
			}
			return groupOrder[i].zone < groupOrder[j].zone
		})
		lines := make([]string, 0, len(groupOrder)+1)
		lines = append(lines, fmt.Sprintf("%s 当前有可创建库存，可以新建实例。已通过预检的卡数（每种卡数下还有多种 CPU/内存组合，创建时可选）：", models))
		for _, group := range groupOrder {
			counts := countsByGroup[group]
			sort.Slice(counts, func(i, j int) bool { return gpuCountOrder(counts[i]) < gpuCountOrder(counts[j]) })
			lines = append(lines, fmt.Sprintf("- %s / %s：%s 卡%s", group.name, group.zone,
				strings.Join(counts, "、"), zoneCardCountSuffix(inventory, group.name, canonicalByGroup[group])))
		}
		return appendCapacityFailureNote(strings.Join(lines, "\n"), failedZones)
	}
	if checkedSpecs == 0 {
		// Every model reaching here came from matchedNormalStockEntries, so the
		// catalog already reports Status=Normal (机型开售). checkedSpecs==0 means no
		// zone yielded a usable capacity-precheck result — the precheck either
		// failed to run (empty project_id → RetCode 230; request missing
		// ProjectId → RetCode 292) or ran but returned no Specs. Don't bury the
		// known catalog truth under "无法确认是否有可创建库存" (which wrongly implies we
		// can't even tell it is on sale). Fall back to the catalog-level 开售
		// statement and be explicit that exact creatability was not verified this
		// turn. A precheck failure must not override
		// the catalog answer.)
		return fmt.Sprintf("%s 机型当前开售；本次容量预检未完成，尚未确认具体配置的可创建性，精确库存请以控制台创建页为准。", models)
	}
	// checkedSpecs>0 but nothing enough is a SAMPLE negative (the probe image's OS/disk
	// feed the sim); it must not be generalized into a global creation denial. Keep the
	// on-sale truth and defer the authoritative answer to the create flow.
	reply := fmt.Sprintf("%s 机型开售；样本容量预检未通过，但不能据此判断该机型无法创建，精确可创建性以创建流程中你最终选择的镜像与配置为准。", models)
	return appendCapacityFailureNote(reply, failedZones)
}

func stockZoneDisplay(entry stockInstanceTypeEntry, supportZones []zones.ZoneInfo) string {
	if entry.Zone != "" {
		return zones.Label(supportZones, entry.Zone)
	}
	return "未知可用区"
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// gpuCountOrder sorts card counts numerically, so 8 follows 4 instead of leading
// with 1/2/4/8 read as text. A non-numeric count sorts last rather than crashing
// the ordering.
func gpuCountOrder(count string) int {
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return 1 << 30
	}
	return n
}

func appendCapacityFailureNote(reply string, failedZones []string) string {
	if len(failedZones) == 0 {
		return reply
	}
	sort.Strings(failedZones)
	return reply + " 另有部分可用区暂时无法确认。"
}
