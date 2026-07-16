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

// Stock-availability read capability (migrated from the legacy intent route).
// This is the heaviest read capability: a plain listing renders the catalog
// Normal/SoldOut status, but a matched-model turn runs a multi-call capacity
// precheck (support zones + raw GPU inventory + a probe image + a per-model /
// per-zone CheckCompShareResourceCapacity) with graceful partial-failure
// handling. The typed request carries the GPU name + zone; the RC017 referent
// (a subject-eliding follow-up resolving to the prior stock turn's model) comes
// from the runtime's FallbackGPUModel, never a re-read of the user's sentence.

const (
	stockCapabilityLabel = string(intent.IntentStockAvailability)
	stockAction          = "DescribeAvailableCompShareInstanceTypes"

	noStockReply      = "未获取到机型库存数据。"
	soldOutDisclaimer = "（CompShare 平台不公开精确剩余数量，仅 Normal/SoldOut 两态。）"
)

// StockAvailabilityRequest is the capability's own request contract.
type StockAvailabilityRequest struct {
	GPUType string `json:"gpu_type,omitempty"`
	Zone    string `json:"zone,omitempty"`
}

// MissingFields: none — an unfiltered stock listing is valid.
func (StockAvailabilityRequest) MissingFields() []platform.MissingField { return nil }

// StockAvailabilityResponse carries the rendered reply, the optional evidence
// envelope (only the plain-listing path builds one) and the resolved single
// model (RC017 session memory). The source fan-out lives in the handler, so the
// renderer is a trivial projection of the response.
type StockAvailabilityResponse struct {
	Reply            string
	Envelope         *envelope.Envelope
	ResolvedGPUModel string
}

type stockInstanceTypeEntry struct {
	Name   string
	Status string
	Zone   string
}

type stockCapacityCheck struct {
	Name        string
	Zone        string
	CheckedSpec int
	EnoughSpecs []string
	Failed      bool
}

func stockReadSpec() ReadCapabilitySpec[StockAvailabilityRequest, StockAvailabilityResponse] {
	return ReadCapabilitySpec[StockAvailabilityRequest, StockAvailabilityResponse]{
		Label:       stockCapabilityLabel,
		Description: "查询 GPU 机型的实时可售性。",
		Params:      objectParam(map[string]schemaNode{"gpu_type": stringParam(), "zone": stringParam()}),
		Handle:      stockHandle,
		Render:      stockRender,
		Observe:     stockObserve,
	}
}

// stockObserve declares the RC017 context side-effect: when the turn resolved to
// a single GPU model, remember it so a later subject-eliding follow-up resolves
// to that model. The referent is carried as a typed effect, not a field on the
// shared read result.
func stockObserve(resp StockAvailabilityResponse) []ReadEffect {
	if strings.TrimSpace(resp.ResolvedGPUModel) == "" {
		return nil
	}
	return []ReadEffect{RememberStockReferent{GPUModel: resp.ResolvedGPUModel}}
}

func stockHandle(ctx context.Context, req StockAvailabilityRequest, rt ReadRuntime) (StockAvailabilityResponse, ReadResult) {
	raw, err := rt.Executor.Execute(ctx, stockAction, map[string]any{})
	if err != nil {
		return StockAvailabilityResponse{}, ReadFailureAfterTool(stockAction, stockCapabilityLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	// RC017: resolve a subject-eliding follow-up ("现在还有库存吗") to the GPU model
	// a prior stock turn resolved to (rt.FallbackGPUModel), so it is not re-expanded
	// to every model. All three stock renderers filter off the same referent, so
	// substituting one effective text keeps them consistent.
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		// Query succeeded but no machine-type stock data — a structured Empty read.
		return StockAvailabilityResponse{}, ReadEmpty(noStockReply)
	}
	referent := stockReferentText(req, rt.FallbackGPUModel, items)
	resolved := singleStockModel(referent, items)

	if reply, ok, terminal := stockCapacityPrecheck(ctx, rt, req, raw); terminal.Status != "" {
		return StockAvailabilityResponse{}, terminal
	} else if ok {
		return StockAvailabilityResponse{Reply: reply, ResolvedGPUModel: resolved}, ReadResult{}
	}
	return StockAvailabilityResponse{
		Reply:            renderStockReply(raw, referent),
		Envelope:         populatedEnvelope(buildStockEnvelope(raw, referent)),
		ResolvedGPUModel: resolved,
	}, ReadResult{}
}

func stockRender(resp StockAvailabilityResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = stockAction
	r.Envelope = resp.Envelope
	return r
}

// --- Relocated from intent/routing_registry.go (Slots/handler → typed req/rt) ----

// stockReferentText returns the user text the stock renderers should filter on.
// When the current turn names a model that text is authoritative; when it elides
// the subject entirely and a prior stock turn resolved to a model that is STILL
// offered, that prior model name (fallbackGPUModel, RC017) is the referent.
func stockReferentText(req StockAvailabilityRequest, fallbackGPUModel string, items []any) string {
	search := strings.TrimSpace(req.GPUType)
	if search != "" {
		return search
	}
	if fallbackGPUModel == "" {
		return ""
	}
	if len(matchUserTextToInstanceTypeNames(fallbackGPUModel, items, false)) == 0 {
		return ""
	}
	return fallbackGPUModel
}

// singleStockModel returns the model name when the text resolves to exactly one
// available model, else "".
func singleStockModel(userText string, items []any) string {
	matched := matchUserTextToInstanceTypeNames(userText, items, false)
	if len(matched) == 1 {
		return matched[0]
	}
	return ""
}

func renderStockReply(raw map[string]any, userText string) string {
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
		lines = append(lines, renderStockStatusLine(name, status))
	}
	if len(lines) == 0 {
		if prefix != "" {
			return strings.TrimRight(prefix, "\n") + "\n" + soldOutDisclaimer
		}
		return noStockReply
	}
	return prefix + strings.Join(lines, "\n") + "\n" + soldOutDisclaimer
}

func renderStockStatusLine(name, status string) string {
	switch {
	case strings.EqualFold(status, "Normal"):
		return fmt.Sprintf("机型=%s, 状态=Normal（机型开售；不代表当前具体配置一定可创建，精确可创建性需做容量预检）", name)
	case strings.EqualFold(status, "SoldOut"):
		return fmt.Sprintf("机型=%s, 状态=SoldOut（售罄）", name)
	default:
		return fmt.Sprintf("机型=%s, 状态=%s", name, status)
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

// stockCapacityPrecheck reproduces the legacy renderStockWithCapacityPrecheck.
// It returns (reply, ok, terminal): ok=false means no matched Normal model, so
// the caller falls through to the plain listing; a non-empty terminal Status is
// a hard upstream failure. The legacy "intent must be stock" guard is omitted —
// this capability IS stock, so it was vacuously satisfied.
func stockCapacityPrecheck(ctx context.Context, rt ReadRuntime, req StockAvailabilityRequest, stockRaw map[string]any) (string, bool, ReadResult) {
	entries := matchedNormalStockEntries(stockRaw, strings.TrimSpace(req.GPUType))
	if len(entries) == 0 {
		return "", false, ReadResult{}
	}
	supportZones := stockSupportZones(ctx, rt)
	if filter := stockZoneFilterFromSlot(req.Zone, supportZones); len(filter) > 0 {
		entries = filterStockEntriesByZone(entries, filter)
		if len(entries) == 0 {
			return renderStockReply(stockRaw, strings.TrimSpace(req.GPUType)) + "\n未在你指定的可用区里找到该机型的开售信息。", true, ReadResult{}
		}
	}
	entriesByModel, modelOrder := groupStockEntriesByModel(entries)
	inventoryRaw, _ := rt.Executor.Execute(ctx, "DescribeCompShareGpuInventory", map[string]any{})
	inventoryLine := renderRawGPUInventoryLine(modelOrder, entriesByModel, inventoryRaw, supportZones)
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
		return renderStockInventoryCapacityReply(failedStockCapacityChecks(entriesByModel, modelOrder), inventoryLine) + "\n容量预检未执行：未获取到可用于预检的系统镜像。", true, ReadResult{}
	}

	checks := make([]stockCapacityCheck, 0, len(entries))

	for _, model := range modelOrder {
		zoneEntries := entriesByModel[model]
		var firstZone string
		var success stockCapacityCheck
		sawSuccess := false
		for _, entry := range zoneEntries {
			if entry.Zone == "" {
				continue
			}
			if firstZone == "" {
				firstZone = entry.Zone
			}
			args := capacityPrecheckArgs(entry, imageID, supportZones, stockRaw, imageRaw)
			capacityRaw, err := stockExecuteCapacityPrecheck(ctx, rt, args)
			if err != nil {
				continue
			}
			success = summarizeStockCapacity(entry, capacityRaw)
			sawSuccess = true
			break
		}
		if sawSuccess {
			checks = append(checks, success)
		} else if firstZone != "" {
			checks = append(checks, stockCapacityCheck{Name: model, Zone: firstZone, Failed: true})
		}
	}
	if len(checks) == 0 {
		return renderStockInventoryCapacityReply(failedStockCapacityChecks(entriesByModel, modelOrder), inventoryLine) + "\n容量预检未执行：当前接口结果缺少可用区信息。", true, ReadResult{}
	}
	return renderStockInventoryCapacityReply(checks, inventoryLine), true, ReadResult{}
}

func failedStockCapacityChecks(entriesByModel map[string][]stockInstanceTypeEntry, modelOrder []string) []stockCapacityCheck {
	var checks []stockCapacityCheck
	for _, model := range modelOrder {
		entries := entriesByModel[model]
		if len(entries) == 0 {
			continue
		}
		zone := entries[0].Zone
		checks = append(checks, stockCapacityCheck{Name: model, Zone: zone, Failed: true})
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

func stockSupportZones(ctx context.Context, rt ReadRuntime) []zones.ZoneInfo {
	list, err := zones.FetchSupportZones(ctx, rt.Executor, 0, 0)
	if err != nil {
		return nil
	}
	return list
}

func stockZoneFilterFromSlot(zoneText string, supportZones []zones.ZoneInfo) map[string]struct{} {
	if len(supportZones) == 0 {
		return nil
	}
	zoneText = strings.TrimSpace(zoneText)
	if zoneText == "" {
		return nil
	}
	if exact, ok := zones.ExactZone(supportZones, zoneText); ok {
		return map[string]struct{}{strings.ToLower(exact): {}}
	}
	for _, z := range supportZones {
		if z.Zone == "" {
			continue
		}
		if strings.EqualFold(zoneText, z.Zone) ||
			(z.Describe != "" && strings.EqualFold(zoneText, z.Describe)) ||
			(z.Region != "" && strings.EqualFold(zoneText, z.Region)) {
			return map[string]struct{}{strings.ToLower(z.Zone): {}}
		}
	}
	return nil
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

func capacityPrecheckArgs(entry stockInstanceTypeEntry, imageID string, supportZones []zones.ZoneInfo, catalog, images map[string]any) map[string]any {
	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:             entry.Zone,
		GPUType:          entry.Name,
		CompShareImageID: imageID,
		ChargeType:       deployment.ChargeTypePostpay,
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

func selectCapacityPrecheckImageID(raw map[string]any) string {
	items := mapSliceAt(raw, "ImageSet")
	bestID := ""
	bestScore := -1
	for _, item := range items {
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
		text := strings.ToLower(strings.Join([]string{
			safeString(entry, "Name"),
			safeString(entry, "ImageName"),
			safeString(entry, "CompShareImageName"),
		}, " "))
		score := 0
		if strings.EqualFold(safeString(entry, "ImageType"), "System") {
			score += 4
		}
		if strings.Contains(text, "ubuntu") {
			score += 4
		}
		if strings.Contains(text, "nvidia") || strings.Contains(text, "cuda") {
			score += 3
		}
		if status != "" {
			score++
		}
		if score > bestScore {
			bestScore = score
			bestID = id
		}
	}
	return bestID
}

func summarizeStockCapacity(entry stockInstanceTypeEntry, raw map[string]any) stockCapacityCheck {
	check := stockCapacityCheck{Name: entry.Name, Zone: entry.Zone}
	for _, item := range mapSliceAt(raw, "Specs") {
		spec, ok := item.(map[string]any)
		if !ok {
			continue
		}
		check.CheckedSpec++
		if resourceEnough(spec["ResourceEnough"]) {
			if label := capacitySpecLabel(spec); label != "" {
				check.EnoughSpecs = append(check.EnoughSpecs, label)
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

func renderStockCapacityReply(checks []stockCapacityCheck) string {
	names := make([]string, 0, len(checks))
	seenNames := map[string]struct{}{}
	var enough []string
	var failedZones []string
	checkedSpecs := 0
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
			enough = append(enough, fmt.Sprintf("%s/%s/%s", check.Name, check.Zone, spec))
		}
	}
	sort.Strings(names)
	models := strings.Join(names, "、")
	if len(enough) > 0 {
		sort.Strings(enough)
		reply := fmt.Sprintf("%s 当前有可创建库存，可以新建实例。", models)
		return appendCapacityFailureNote(reply, failedZones)
	}
	if checkedSpecs == 0 {
		// Every model reaching here came from matchedNormalStockEntries, so the
		// catalog already reports Status=Normal (机型开售). checkedSpecs==0 means no
		// zone yielded a usable capacity-precheck result — the precheck either
		// failed to run (CLI with empty project_id → RetCode 230; HTTP missing
		// ProjectId → RetCode 292) or ran but returned no Specs. Don't bury the
		// known catalog truth under "无法确认是否有可创建库存" (which wrongly implies we
		// can't even tell it is on sale). Fall back to the catalog-level 开售
		// statement and be explicit that exact creatability was not verified this
		// turn. (#3b graceful degradation — a precheck failure must not override
		// the catalog answer.)
		return fmt.Sprintf("%s 机型当前开售；本次容量预检未能确认具体配置的可创建性，精确库存请以控制台创建页为准。", models)
	}
	reply := fmt.Sprintf("%s 当前暂无可创建库存，暂时不能新建实例。", models)
	return appendCapacityFailureNote(reply, failedZones)
}

func renderStockInventoryCapacityReply(checks []stockCapacityCheck, inventoryLine string) string {
	reply := renderStockCapacityReply(checks)
	names := uniqueStockCheckNames(checks)
	if len(names) == 0 {
		return reply
	}
	sort.Strings(names)
	models := strings.Join(names, "、")
	if inventoryLine == "" {
		inventoryLine = fmt.Sprintf("原始 GPU 库存：接口未返回 %s 的库存数量。", models)
	}
	if len(checks) > 0 && anyStockCapacityEnough(checks) {
		return fmt.Sprintf("%s 默认创建配置已通过容量预检，可以新建实例。\n%s\n机型状态：开售。", models, inventoryLine)
	}
	if allStockCapacityFailed(checks) {
		return fmt.Sprintf("%s 默认创建配置容量预检未完成，暂不能确认默认配置是否可创建。\n%s\n机型状态：开售。", models, inventoryLine)
	}
	return fmt.Sprintf("%s 默认创建配置暂未通过容量预检，暂时不能新建实例。\n%s\n机型状态：开售。", models, inventoryLine)
}

func uniqueStockCheckNames(checks []stockCapacityCheck) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, check := range checks {
		if check.Name == "" {
			continue
		}
		if _, ok := seen[check.Name]; ok {
			continue
		}
		seen[check.Name] = struct{}{}
		out = append(out, check.Name)
	}
	return out
}

func anyStockCapacityEnough(checks []stockCapacityCheck) bool {
	for _, check := range checks {
		if len(check.EnoughSpecs) > 0 {
			return true
		}
	}
	return false
}

func allStockCapacityFailed(checks []stockCapacityCheck) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if !check.Failed {
			return false
		}
	}
	return true
}

func renderRawGPUInventoryLine(modelOrder []string, entriesByModel map[string][]stockInstanceTypeEntry, raw map[string]any, supportZones []zones.ZoneInfo) string {
	if len(modelOrder) == 0 {
		return ""
	}
	pool := stockInventoryPool(raw, "Exclusive")
	if len(pool) == 0 {
		return "原始 GPU 库存：库存接口未返回可用数据。"
	}
	lines := []string{}
	for _, model := range modelOrder {
		entries := entriesByModel[model]
		known := []string{}
		for _, entry := range entries {
			zoneID := stockInventoryZoneID(entry, supportZones)
			if zoneID == 0 {
				continue
			}
			gpuCounts, ok := pool[zoneID]
			if !ok {
				continue
			}
			count, ok := gpuCounts[entry.Name]
			if !ok {
				continue
			}
			zone := stockZoneDisplay(entry, supportZones)
			if count > 0 {
				known = append(known, fmt.Sprintf("%s 库存约 %s 张 GPU", zone, trimFloat(count)))
			} else {
				known = append(known, fmt.Sprintf("%s 暂无原始 GPU 库存", zone))
			}
		}
		if len(known) == 0 {
			labels := stockEntryZoneLabels(entries, supportZones)
			if len(labels) > 0 {
				lines = append(lines, fmt.Sprintf("%s 接口未返回 %s 的库存数量", strings.Join(labels, "、"), model))
			} else {
				lines = append(lines, fmt.Sprintf("接口未返回 %s 的库存数量", model))
			}
			continue
		}
		sort.Strings(known)
		lines = append(lines, strings.Join(known, "；"))
	}
	return "原始 GPU 库存：" + strings.Join(lines, "；") + "。"
}

func stockZoneDisplay(entry stockInstanceTypeEntry, supportZones []zones.ZoneInfo) string {
	if entry.Zone != "" {
		if describe := zones.DescribeFor(supportZones, entry.Zone); describe != "" {
			return describe
		}
		return entry.Zone
	}
	return "未知可用区"
}

func stockInventoryZoneID(entry stockInstanceTypeEntry, supportZones []zones.ZoneInfo) uint32 {
	if entry.Zone != "" {
		for _, z := range supportZones {
			if z.ZoneID != 0 && strings.EqualFold(z.Zone, entry.Zone) {
				return z.ZoneID
			}
		}
	}
	return 0
}

func stockEntryZoneLabels(entries []stockInstanceTypeEntry, supportZones []zones.ZoneInfo) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, entry := range entries {
		label := stockZoneDisplay(entry, supportZones)
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func stockInventoryPool(raw map[string]any, poolName string) map[uint32]map[string]float64 {
	if raw == nil {
		return nil
	}
	switch inv := raw["GpuInventory"].(type) {
	case map[string]any:
		return convertStockInventoryPool(inv[poolName])
	case map[string]map[uint32]map[string]uint32:
		return convertStockInventoryPool(inv[poolName])
	case map[string]map[uint32]map[string]float64:
		return convertStockInventoryPool(inv[poolName])
	default:
		return nil
	}
}

func convertStockInventoryPool(raw any) map[uint32]map[string]float64 {
	out := map[uint32]map[string]float64{}
	switch pool := raw.(type) {
	case map[string]any:
		for rawZoneID, rawGPUCounts := range pool {
			id, ok := parseUint32Loose(rawZoneID)
			if !ok {
				continue
			}
			if counts := convertGPUCountMap(rawGPUCounts); len(counts) > 0 {
				out[id] = counts
			}
		}
	case map[uint32]map[string]uint32:
		for id, counts := range pool {
			out[id] = map[string]float64{}
			for gpu, count := range counts {
				out[id][gpu] = float64(count)
			}
		}
	case map[uint32]map[string]float64:
		for id, counts := range pool {
			out[id] = map[string]float64{}
			for gpu, count := range counts {
				out[id][gpu] = count
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func convertGPUCountMap(raw any) map[string]float64 {
	out := map[string]float64{}
	switch counts := raw.(type) {
	case map[string]any:
		for gpu, rawCount := range counts {
			if count, ok := numericValue(rawCount); ok {
				out[gpu] = count
			}
		}
	case map[string]uint32:
		for gpu, count := range counts {
			out[gpu] = float64(count)
		}
	case map[string]float64:
		for gpu, count := range counts {
			out[gpu] = count
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseUint32Loose(v any) (uint32, bool) {
	switch typed := v.(type) {
	case string:
		typed = strings.TrimSpace(typed)
		if typed == "" {
			return 0, false
		}
		n, err := strconv.ParseUint(typed, 10, 32)
		if err != nil || n == 0 {
			return 0, false
		}
		return uint32(n), true
	default:
		n, ok := numericValue(typed)
		if !ok || n <= 0 || n != float64(uint32(n)) {
			return 0, false
		}
		return uint32(n), true
	}
}

func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func appendCapacityFailureNote(reply string, failedZones []string) string {
	if len(failedZones) == 0 {
		return reply
	}
	sort.Strings(failedZones)
	return reply + " 另有部分可用区暂时无法确认。"
}
