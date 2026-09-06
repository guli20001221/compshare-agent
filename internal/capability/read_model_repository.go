package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Model-repository browse combines the live tag and model catalogs under one
// typed request.

const (
	modelRepositoryCapabilityLabel = string(intent.IntentModelRepositoryBrowse)
	modelRepositoryTagAction       = "DescribeModelRepositoryTags"
	modelRepositoryModelAction     = "DescribeModelRepositoryModels"
)

// ModelRepositoryRequest is the capability's own request contract.
type ModelRepositoryRequest struct {
	Query         string            `json:"query,omitempty"`
	Source        string            `json:"source,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	Categories    []string          `json:"categories,omitempty"`
	Status        string            `json:"status,omitempty"`
	ReplicaStatus string            `json:"replica_status,omitempty"`
	Zone          string            `json:"zone,omitempty"`
	Mode          platform.ListMode `json:"mode,omitempty"`
	Offset        int               `json:"offset,omitempty"`
}

// MissingFields: none — an unfiltered browse is valid.
func (ModelRepositoryRequest) MissingFields() []platform.MissingField { return nil }

// ModelRepositoryResponse carries the rendered repository reply.
type ModelRepositoryResponse struct {
	Reply string
}

func modelRepositoryReadSpec() ReadCapabilitySpec[ModelRepositoryRequest, ModelRepositoryResponse] {
	return ReadCapabilitySpec[ModelRepositoryRequest, ModelRepositoryResponse]{
		Label:       modelRepositoryCapabilityLabel,
		Description: "查询公共模型目录、路径和目标可用区的副本状态。目录记录不等于目标实例已预置；只有工具明确返回目标可用区副本健康时，才能判断对应路径可直接使用。它不是可创建的镜像目录，也不能证明平台已支持部署。",
		Params: objectParam(map[string]schemaNode{
			"query":          stringParam().described("模型名或仓库名关键词，不用于分类模糊匹配。"),
			"source":         enumParam("Unspecified", "HuggingFace", "ModelScope", "Internal"),
			"tags":           arrayParam(stringParam()).described("精确标签，区分大小写；使用实时标签目录或模型返回的 Tags 原值，未知时留空先浏览目录。"),
			"categories":     arrayParam(stringParam()).described("精确分类，区分大小写；使用模型目录返回的 Category 原值，未知时留空先浏览目录。"),
			"status":         enumParam("Unspecified", "Active", "Offline", "Draft"),
			"replica_status": enumParam("Unspecified", "Healthy", "Offline", "Incomplete", "Missing").described("副本状态；指定 zone 时按该区筛选。未指定 zone 时 Healthy 表示上游未发现任何区的副本问题，其余状态表示任一区存在该问题。"),
			"zone":           stringParam().described("仅在用户明确指定目标可用区，或当前实例事实已给出可用区时填写实时目录中的 Zone；不要猜测。"),
			"mode":           enumParam(platform.ListModeValues()...),
			"offset":         integerParam(0).described("分页偏移，默认 0；继续浏览时使用结果给出的下一页偏移。"),
		}),
		NeedsZoneCatalog: func(req ModelRepositoryRequest) bool {
			return strings.TrimSpace(req.Zone) != ""
		},
		Handle: modelRepositoryHandle,
		Render: modelRepositoryRender,
	}
}

func modelRepositoryHandle(ctx context.Context, req ModelRepositoryRequest, rt ReadRuntime) (ModelRepositoryResponse, ReadResult) {
	tagRaw, err := rt.Executor.Execute(ctx, modelRepositoryTagAction, map[string]any{})
	if err != nil {
		return ModelRepositoryResponse{}, ReadFailureAfterTool(modelRepositoryTagAction, modelRepositoryCapabilityLabel, err)
	}
	if tagRaw == nil {
		tagRaw = map[string]any{}
	}
	zoneID, zoneLabel, terminal := resolveModelRepositoryZone(req.Zone, rt.ZoneCatalog)
	if terminal.Status != "" {
		return ModelRepositoryResponse{}, terminal
	}
	args := modelRepositoryArgs(req, tagRaw, zoneID)
	modelRaw, err := rt.Executor.Execute(ctx, modelRepositoryModelAction, args)
	if err != nil {
		return ModelRepositoryResponse{}, ReadFailureAfterTool(modelRepositoryModelAction, modelRepositoryCapabilityLabel, err)
	}
	if modelRaw == nil {
		modelRaw = map[string]any{}
	}
	reply, empty := renderModelRepositoryReply(modelRaw, tagRaw, req, zoneID, zoneLabel)
	if empty {
		return ModelRepositoryResponse{}, ReadEmpty(reply)
	}
	return ModelRepositoryResponse{Reply: reply}, ReadResult{}
}

func modelRepositoryRender(resp ModelRepositoryResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = modelRepositoryModelAction
	return r
}

func modelRepositoryArgs(req ModelRepositoryRequest, tagRaw map[string]any, zoneID uint32) map[string]any {
	args := map[string]any{"Limit": imageModelBrowseDisplayCap, "Offset": req.Offset}
	query := strings.TrimSpace(req.Query)
	explicitTags := uniqueStrings(req.Tags)
	tags := explicitTags
	derivedTags := false
	if len(tags) == 0 {
		tags = matchModelRepositoryTags(query, uniqueStrings(stringSliceAt(tagRaw, "Tags")))
		derivedTags = len(tags) > 0
	}
	// An automatically recognised catalog tag replaces the free-text keyword;
	// sending both makes the upstream apply two filters and can turn a valid tag
	// browse into an empty result. Explicit tags plus an explicit query, however,
	// are intentionally conjunctive and both are preserved.
	if query != "" && req.Mode != platform.ListModeAll && !derivedTags {
		args["Keyword"] = query
	}
	if len(tags) > 0 {
		args["Tags"] = limitStrings(tags, 10)
	}
	if categories := uniqueStrings(req.Categories); len(categories) > 0 {
		args["Categories"] = limitStrings(categories, 10)
	}
	if source := modelRepositoryOptionalEnum(req.Source); source != "" {
		args["Source"] = source
	}
	if status := modelRepositoryOptionalEnum(req.Status); status != "" {
		args["Status"] = status
	}
	replica := modelRepositoryOptionalEnum(req.ReplicaStatus)
	if zoneID != 0 && (replica == "" || strings.EqualFold(replica, "Healthy")) {
		// Upstream ZoneID means AvailableZoneIDs contains this zone. Do not combine
		// it with ReplicaStatus=Healthy: that status is global (no unhealthy
		// replica anywhere), which is stricter than “healthy in this target zone”.
		args["ZoneID"] = zoneID
	} else if replica != "" {
		// Offline/Incomplete/Missing are global upstream filters. They narrow the
		// candidate set; renderModelRepositoryReply then checks the returned per-
		// status zone-id arrays so the typed capability answers for the exact zone.
		args["ReplicaStatus"] = replica
	}
	return args
}

func modelRepositoryOptionalEnum(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "Unspecified") {
		return ""
	}
	return value
}

func resolveModelRepositoryZone(query string, catalog *deployment.ZoneCatalogSnapshot) (uint32, string, ReadResult) {
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, "", ReadResult{}
	}
	if catalog == nil || !catalog.Available() {
		return 0, "", ReadUnavailable("目标可用区目录当前不可用，无法核验模型副本状态。", nil)
	}
	normalized := strings.ToLower(strings.ReplaceAll(query, " ", ""))
	var matches []deployment.ZoneCatalogEntry
	for _, zone := range catalog.Zones() {
		entry, ok := catalog.Entry(zone)
		if !ok {
			continue
		}
		zoneKey := strings.ToLower(strings.ReplaceAll(entry.Placement.Zone, " ", ""))
		labelKey := strings.ToLower(strings.ReplaceAll(entry.DisplayName, " ", ""))
		if normalized == zoneKey || (labelKey != "" && normalized == labelKey) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		result := zoneCatalogRender(ZoneCatalogResponse{Records: zoneCatalogRecords(catalog)})
		result.Reply = query + " 不是当前实时可用区目录中的可用区名称或 ZoneID，本次未查询模型副本状态。\n当前实时可用区目录：\n" + result.Reply
		result.Status = platform.ReadStatusNeedsInput
		result.FallbackReason = platform.ReadFallbackValidation
		return 0, "", result
	}
	if len(matches) > 1 {
		labels := make([]string, 0, len(matches))
		for _, entry := range matches {
			labels = append(labels, entry.DisplayName+"（"+entry.Placement.Zone+"）")
		}
		return 0, "", ReadConflict("目标区域对应多个可用区，请明确选择：" + strings.Join(labels, "、"))
	}
	entry := matches[0]
	label := entry.DisplayName
	if label == "" {
		label = entry.Placement.Zone
	}
	return entry.Placement.ZoneID, label + "（" + entry.Placement.Zone + "）", ReadResult{}
}

func matchModelRepositoryTags(userText string, tags []string) []string {
	if strings.TrimSpace(userText) == "" || len(tags) == 0 {
		return nil
	}
	lowerText := strings.ToLower(userText)
	matched := []string{}
	seen := map[string]struct{}{}
	for _, tag := range tags {
		clean := strings.TrimSpace(tag)
		if clean == "" {
			continue
		}
		if !strings.Contains(lowerText, strings.ToLower(clean)) {
			continue
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		matched = append(matched, clean)
	}
	return matched
}

// renderModelRepositoryReply returns the reply and whether the repository is
// wholly empty (no tags and no models at all). A no-match that still shows the
// tag vocabulary is a Handled partial answer, not Empty.
func renderModelRepositoryReply(modelRaw, tagRaw map[string]any, req ModelRepositoryRequest, zoneID uint32, zoneLabel string) (string, bool) {
	tags := uniqueStrings(stringSliceAt(tagRaw, "Tags"))
	models := mapSliceAt(modelRaw, "Models")
	filtered := modelRepositoryRows(models)
	filtered = filterModelRepositoryTargetReplica(filtered, zoneID, req.ReplicaStatus)
	sections := []string{}
	if total, known := numericField(modelRaw, "TotalCount"); known {
		next := req.Offset + len(models)
		sections = append(sections, fmt.Sprintf("上游筛选共 %d 个模型，本页偏移 %d，返回 %d 个。", int(total), req.Offset, len(models)))
		if len(models) > 0 && next < int(total) {
			sections = append(sections, fmt.Sprintf("还有后续目录；继续浏览请使用 offset=%d。", next))
		}
	}
	if len(tags) > 0 {
		sections = append(sections, "模型仓库标签: "+strings.Join(limitStrings(tags, 20), "、"))
	}
	if len(filtered) == 0 {
		if total, known := numericField(modelRaw, "TotalCount"); known && (total > float64(len(models)) || req.Offset > 0) {
			sections = append(sections, "本页未找到满足条件的模型；不能据此判断全部目录不存在匹配项。")
			return strings.Join(sections, "\n"), false
		}
		noMatch := "未找到匹配的模型。"
		if zoneID != 0 && modelRepositoryOptionalEnum(req.ReplicaStatus) != "" {
			noMatch = fmt.Sprintf("未找到在 %s 副本状态为 %s 的匹配模型。", zoneLabel, modelRepositoryOptionalEnum(req.ReplicaStatus))
		}
		if len(tags) > 0 {
			sections = append(sections, noMatch, modelRepositoryGuidanceFooter(false))
			return strings.Join(sections, "\n"), false
		}
		sections = append(sections, noMatch)
		return strings.Join(sections, "\n"), true
	}
	allLines := []string{}
	for _, entry := range filtered {
		line := buildModelRepositoryLine(entry, zoneID, zoneLabel)
		if line == "" {
			continue
		}
		allLines = append(allLines, line)
	}
	if len(allLines) == 0 {
		if len(tags) > 0 {
			sections = append(sections, "未找到匹配的模型。", modelRepositoryGuidanceFooter(false))
			return strings.Join(sections, "\n"), false
		}
		return "未获取到模型仓库数据。", true
	}
	lines := allLines
	if len(lines) > imageModelBrowseDisplayCap {
		lines = lines[:imageModelBrowseDisplayCap]
	}
	sections = append(sections, "模型仓库列表:\n"+strings.Join(lines, "\n"))
	if len(allLines) > len(lines) {
		sections = append(sections, fmt.Sprintf("（共 %d 个模型，已显示前 %d 个；可补充关键词进一步筛选）", len(allLines), len(lines)))
	}
	sections = append(sections, modelRepositoryGuidanceFooter(true, zoneID != 0))
	return strings.Join(sections, "\n"), false
}

// modelRepositoryGuidanceFooter bridges repository results to the two supported
// follow-ups: use the returned per-entry path, or download a missing model inside
// an instance. It does not hardcode a repository mount layout.
func modelRepositoryGuidanceFooter(found bool, targetZone ...bool) string {
	if !found {
		return "仓库里暂时没有匹配的模型。你可以部署实例后自行拉取：HuggingFace / ModelScope 下载，或 Ollama 容器用 `ollama pull <模型名>`——需要具体命令可以问我。"
	}
	zoneScoped := len(targetZone) > 0 && targetZone[0]
	availability := "说明：以上是模型目录记录；没有指定目标可用区，不能据此判断某台实例是否已经具备模型文件。"
	if zoneScoped {
		availability = "说明：只有条目明确标记“副本健康、可直接使用”时，才能在该目标可用区按返回路径加载；其他状态仍需同步、修复副本或自行下载。"
	}
	return strings.Join([]string{
		availability,
		"· 想部署某个模型，直接告诉我模型名（如「部署 Llama-3.1-8B」），我来帮你选 GPU 配置。",
		"· 仓库里没有的模型，可在实例内自行拉取（HuggingFace / ModelScope 下载，或 Ollama `ollama pull <模型名>`）——需要命令可以问我。",
	}, "\n")
}

// Query and tags have already been applied by the upstream catalog. Local
// filtering only handles the legacy Deleted marker and target-zone replicas.
func modelRepositoryRows(models []any) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, item := range models {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(safeString(entry, "Deleted")) == "1" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func filterModelRepositoryTargetReplica(models []map[string]any, zoneID uint32, requested string) []map[string]any {
	requested = modelRepositoryOptionalEnum(requested)
	if zoneID == 0 || requested == "" {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, entry := range models {
		if strings.EqualFold(modelRepositoryReplicaState(entry, zoneID), requested) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func modelRepositoryReplicaState(entry map[string]any, zoneID uint32) string {
	if containsUint32(uint32SliceAt(entry, "OfflineZoneIDs"), zoneID) {
		return "Offline"
	}
	if containsUint32(uint32SliceAt(entry, "IncompleteZoneIDs"), zoneID) {
		return "Incomplete"
	}
	if containsUint32(uint32SliceAt(entry, "MissingZoneIDs"), zoneID) {
		return "Missing"
	}
	if strings.EqualFold(strings.TrimSpace(safeString(entry, "Status")), "Active") &&
		containsUint32(uint32SliceAt(entry, "AvailableZoneIDs"), zoneID) {
		return "Healthy"
	}
	return "Unknown"
}

func buildModelRepositoryLine(entry map[string]any, zoneID uint32, zoneLabel string) string {
	parts := []string{}
	for _, key := range []string{"ModelID", "Name", "RepoName", "Source", "Category", "CanonicalPath", "Size", "Tag", "Path", "Status"} {
		if v := strings.TrimSpace(safeString(entry, key)); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	if tags := uniqueStrings(stringSliceAt(entry, "Tags")); len(tags) > 0 {
		parts = append(parts, "Tags="+strings.Join(tags, ","))
	}
	if size, ok := numericField(entry, "SizeBytes"); ok && size > 0 {
		parts = append(parts, "SizeBytes="+formatModelBytes(int64(size)))
	}
	parts = append(parts, modelRepositoryReplicaLabel(entry, zoneID, zoneLabel))
	return strings.Join(parts, ", ")
}

func formatModelBytes(size int64) string {
	const (
		mib = int64(1024 * 1024)
		gib = int64(1024 * 1024 * 1024)
	)
	if size >= gib {
		return fmt.Sprintf("%.2fGiB", float64(size)/float64(gib))
	}
	return fmt.Sprintf("%.2fMiB", float64(size)/float64(mib))
}

func modelRepositoryReplicaLabel(entry map[string]any, zoneID uint32, zoneLabel string) string {
	if zoneID == 0 {
		return fmt.Sprintf("Replica=目录状态（可用区健康需指定目标区；available=%d offline=%d incomplete=%d missing=%d）",
			len(uint32SliceAt(entry, "AvailableZoneIDs")), len(uint32SliceAt(entry, "OfflineZoneIDs")),
			len(uint32SliceAt(entry, "IncompleteZoneIDs")), len(uint32SliceAt(entry, "MissingZoneIDs")))
	}
	switch modelRepositoryReplicaState(entry, zoneID) {
	case "Offline":
		return "Replica=" + zoneLabel + " 副本离线"
	case "Incomplete":
		return "Replica=" + zoneLabel + " 副本不完整"
	case "Missing":
		return "Replica=" + zoneLabel + " 副本缺失"
	case "Healthy":
		return "Replica=" + zoneLabel + " 副本健康、可直接使用"
	default:
		return "Replica=" + zoneLabel + " 状态未知，不能视为已预置"
	}
}

func uint32SliceAt(m map[string]any, key string) []uint32 {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]uint32, 0, len(raw))
	for _, value := range raw {
		if n, ok := numericValue(value); ok && n > 0 {
			out = append(out, uint32(n))
		}
	}
	return out
}

func containsUint32(values []uint32, want uint32) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
