package capability

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/imagecatalogfetch"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Image-list fans a typed source/query request into the platform, custom,
// community or shared live catalog and returns the corresponding evidence.

const (
	imageListCapabilityLabel = string(intent.IntentImageList)

	platformImageAction  = "DescribeCompShareImages"
	customImageAction    = "DescribeCompShareCustomImages"
	communityImageAction = "DescribeCommunityImages"
	sharedImageAction    = "DescribeCompShareSharingImages"

	noImageListReply        = "未获取到镜像列表。"
	noImageListNoMatchReply = "未找到匹配的镜像。"
	noCommunityReply        = "未获取到社区镜像数据。"

	imageListDisplayCap      = imageModelBrowseDisplayCap
	communityImageGroupLimit = 10 // upper bound on community renderer output lines
	communityVersionPerGroup = 3  // versions to show per CompshareImageGroup
)

// imageDisplaySkipFields are the raw-id / redundant-name keys the clean image
// display intentionally OMITS: the name is shown once up front (bestImageName) and
// the raw CompShareImageId is dropped from the default view (用户按名称引用即可; the
// envelope's Subject.ID still carries the id for any downstream consumer).
var imageDisplaySkipFields = map[string]struct{}{
	"CompShareImageId":   {},
	"Name":               {},
	"CompShareImageName": {},
	"ImageName":          {},
}

// ImageListRequest is the capability's own request contract.
type ImageListRequest struct {
	Source          platform.ImageSource `json:"source,omitempty"`
	Query           string               `json:"query,omitempty"`
	SemanticQueries []string             `json:"semantic_queries,omitempty"`
	Mode            platform.ListMode    `json:"mode,omitempty"`
}

// MissingFields: none — an unfiltered platform listing is valid.
func (ImageListRequest) MissingFields() []platform.MissingField { return nil }

// ImageListResponse carries the rendered reply, the upstream action used and the
// (optional) evidence envelope — the source fan-out and envelope-population gate
// live in the handler, so the renderer is a trivial projection of the response.
type ImageListResponse struct {
	Reply    string
	Action   string
	Envelope *envelope.Envelope
}

func imageListReadSpec() ReadCapabilitySpec[ImageListRequest, ImageListResponse] {
	return ReadCapabilitySpec[ImageListRequest, ImageListResponse]{
		Label:       imageListCapabilityLabel,
		Description: "查询平台、自制、社区或共享镜像的实时目录，用于浏览、推荐和创建前选型。部署或运行具名模型/应用时先查社区镜像；有精确候选就据此回答，没有则如实说明。源码、权重或 adapter 问题再使用模型仓库或知识检索。",
		Params: objectParam(map[string]schemaNode{
			"source": enumParam(platform.ImageSourceValues()...),
			"query": stringParam().described(
				"用户原话中的目录查询词；复制最短且有意义的用途、约束或用户明确点名的镜像，可取用户表达中的子串，不要先猜候选镜像名。无查询条件时留空。",
			),
			"semantic_queries": arrayParam(stringParam()).described(
				"最多 3 个补充查询词。根据用户用途提炼技术、项目类别或运行时名称；不能替代 query，结果会合并。部署具名模型/应用时先查 community，未命中或用户要求基础环境时再查 platform。",
			),
			"mode": enumParam(platform.ListModeValues()...),
		}),
		Handle: imageListHandle,
		Render: imageListRender,
	}
}

func imageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	// Shared images carry no envelope, so that handler reports its own emptiness.
	if req.Source == platform.ImageSourceShared {
		return sharedImageListHandle(ctx, req, rt)
	}
	var resp ImageListResponse
	var terminal ReadResult
	switch req.Source {
	case platform.ImageSourceCustom:
		resp, terminal = customImageListHandle(ctx, req, rt)
	case platform.ImageSourceCommunity:
		resp, terminal = communityImageListHandle(ctx, req, rt)
	default:
		resp, terminal = platformImageListHandle(ctx, req, rt)
	}
	if terminal.Status != "" {
		return resp, terminal
	}
	if resp.Envelope == nil {
		// populatedEnvelope returns nil only when no image subjects were listed —
		// the query found nothing (empty catalog or no match): a structured Empty.
		return ImageListResponse{}, ReadEmpty(resp.Reply)
	}
	return resp, ReadResult{}
}

// imageListRender projects the response. Its spec declares browse presentation:
// the Agent curates the catalog instead of pasting every row into the answer.
func imageListRender(resp ImageListResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = resp.Action
	r.Envelope = resp.Envelope
	return r
}

func platformImageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	fieldOrder := []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"}
	raw, err := imageExecuteAll(ctx, rt, platformImageAction, "ImageSet", map[string]any{})
	if err != nil {
		return ImageListResponse{}, ReadFailureAfterTool(platformImageAction, imageListCapabilityLabel, err)
	}
	query, mode := req.Query, req.Mode
	// Semantic expansion on the platform catalog costs NOTHING extra: unlike the
	// community path (one upstream FuzzySearch per query), the platform listing is
	// fetched whole and filtered here, so the expansion is just a wider client-side
	// match over rows already in hand.
	//
	// Why it is needed at all: the platform's inference images are named after the
	// runtime — vLLM v0.25.1, SGLang v0.5.15, Ollama v0.32.1 — so a user asking for
	// 大模型推理 matches ZERO of them by name, while the same intent on the community
	// side expands and matches. The Agent could only answer from the side that had
	// matches, which is why platform-maintained images never appeared in a
	// recommendation even though they exist.
	if queries := imageSearchQueries(req); len(queries) > 1 {
		raw = filterImageSetByAnyQuery(raw, "ImageSet", queries, imageQueryMatchFields(fieldOrder))
		// The union IS the filter; re-applying the primary query below would
		// narrow back to it and erase every expansion (the same self-narrowing
		// bug the community path documents).
		query, mode = "", platform.ListModeAll
	}
	env := buildImageListEnvelope(raw, "ImageSet", fieldOrder, query, mode, platformImageAction, "platform")
	return ImageListResponse{
		Reply:    renderImageListReply(raw, "ImageSet", fieldOrder, query, mode),
		Action:   platformImageAction,
		Envelope: populatedEnvelope(env),
	}, ReadResult{}
}

// imageQueryMatchFields picks the fields a catalog query matches against, from
// the same fieldOrder buildImageListEnvelope uses — so a pre-filter and the
// envelope's own filter can never disagree about what "matches" means.
func imageQueryMatchFields(fieldOrder []string) []string {
	out := make([]string, 0, len(fieldOrder))
	for _, f := range fieldOrder {
		switch f {
		case "Name", "ImageName", "CompShareImageName", "Author":
			out = append(out, f)
		}
	}
	return out
}

// primaryImageQueryMatchFields includes the stable image id in addition to
// human names. The primary query is copied from the user's request, so an id
// returned by a just-completed create workflow must be usable for a follow-up
// status lookup. Semantic expansion deliberately keeps using
// imageQueryMatchFields above: generated purpose words should never match an
// opaque id by accident.
func primaryImageQueryMatchFields(fieldOrder []string) []string {
	out := imageQueryMatchFields(fieldOrder)
	for _, f := range fieldOrder {
		if f == "CompShareImageId" {
			out = append(out, f)
			break
		}
	}
	return out
}

// filterImageSetByAnyQuery keeps every row matching AT LEAST ONE query. Union,
// never intersection: an expansion may only add candidates, so a bad expansion
// term costs nothing and cannot hide the images the user's own words found.
func filterImageSetByAnyQuery(raw map[string]any, listKey string, queries []string, fields []string) map[string]any {
	if len(queries) == 0 || len(fields) == 0 {
		return raw
	}
	items := mapSliceAt(raw, listKey)
	kept := make([]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, query := range queries {
			if strings.TrimSpace(query) == "" || entryMatchesSlotQuery(entry, query, fields) {
				kept = append(kept, item)
				break
			}
		}
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	out[listKey] = kept
	return out
}

func customImageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	fieldOrder := []string{"CompShareImageId", "Name", "ImageName", "Status"}
	raw, err := imageExecuteAll(ctx, rt, customImageAction, "ImageSet", map[string]any{})
	if err != nil {
		return ImageListResponse{}, ReadFailureAfterTool(customImageAction, imageListCapabilityLabel, err)
	}
	env := buildImageListEnvelope(raw, "ImageSet", fieldOrder, req.Query, req.Mode, customImageAction, "custom")
	return ImageListResponse{
		Reply:    renderImageListReply(raw, "ImageSet", fieldOrder, req.Query, req.Mode),
		Action:   customImageAction,
		Envelope: populatedEnvelope(env),
	}, ReadResult{}
}

func communityImageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	queries := imageSearchQueries(req)
	results := make([]map[string]any, 0, len(queries))
	for _, query := range queries {
		// ExcludeReadme: the Readme rich text is never parsed into a catalog entry
		// — Description is a separate upstream field and is what the model reads —
		// and it is most of the payload: measured live, the full 835-family catalog
		// is 5.9MB with Readme and 2.1MB without. We were fetching it and throwing
		// it away, once per query.
		args := map[string]any{"ExcludeReadme": true}
		if query != "" {
			args["FuzzySearch"] = query
		}
		raw, err := imageExecuteAll(ctx, rt, communityImageAction, "CompshareImageGroup", args)
		if err != nil {
			return ImageListResponse{}, ReadFailureAfterTool(communityImageAction, imageListCapabilityLabel, err)
		}
		results = append(results, filterFlatCommunityImageResult(raw, query))
	}
	raw := mergeCommunityImageResults(results)
	// Upstream already applied every individual query. The merged catalog is the
	// union; applying the primary query again here would erase the semantic
	// expansions and restore the self-narrowing bug. Keep the actual mode so a
	// filtered union preserves primary-query results ahead of its expansions;
	// only an unfiltered catalog browse is popularity-ranked.
	env := buildCommunityImageEnvelope(raw, "", req.Mode)
	return ImageListResponse{
		Reply:    renderCommunityImageReply(raw, "", req.Mode),
		Action:   communityImageAction,
		Envelope: populatedEnvelope(env),
	}, ReadResult{}
}

func imageSearchQueries(req ImageListRequest) []string {
	if req.Mode == platform.ListModeAll {
		return []string{""}
	}
	seen := map[string]bool{}
	out := make([]string, 0, 1+len(req.SemanticQueries))
	for _, query := range append([]string{req.Query}, req.SemanticQueries...) {
		query = strings.TrimSpace(query)
		key := strings.ToLower(query)
		if query == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, query)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func mergeCommunityImageResults(results []map[string]any) map[string]any {
	if len(results) == 0 {
		return map[string]any{}
	}
	groups := make([]map[string]any, 0)
	flat := make([]any, 0)
	for _, raw := range results {
		for _, item := range mapSliceAt(raw, "CompshareImageGroup") {
			if group, ok := item.(map[string]any); ok {
				groups = append(groups, group)
			}
		}
		flat = append(flat, mapSliceAt(raw, "ImageSet")...)
	}
	merged := map[string]any{}
	if len(groups) > 0 {
		deduped := dedupeCommunityImageGroups(groups)
		items := make([]any, 0, len(deduped))
		for _, group := range deduped {
			items = append(items, group)
		}
		merged["CompshareImageGroup"] = items
		merged["TotalCount"] = len(items)
	}
	if len(flat) > 0 {
		deduped := dedupeFlatImageRows(flat)
		merged["ImageSet"] = deduped
		if len(groups) == 0 {
			merged["TotalCount"] = len(deduped)
		}
	}
	return merged
}

// filterFlatCommunityImageResult preserves the compatibility path for community
// APIs that return ImageSet instead of CompshareImageGroup. Group responses are
// already filtered upstream; a flat response is filtered again locally so an
// upstream implementation that ignores FuzzySearch cannot turn one semantic
// expansion into an unfiltered catalog.
func filterFlatCommunityImageResult(raw map[string]any, query string) map[string]any {
	if strings.TrimSpace(query) == "" || len(mapSliceAt(raw, "CompshareImageGroup")) > 0 {
		return raw
	}
	items := mapSliceAt(raw, "ImageSet")
	if len(items) == 0 {
		return raw
	}
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok || !entryMatchesSlotQuery(entry, query,
			[]string{"Name", "ImageName", "CompShareImageName", "Author"}) {
			continue
		}
		filtered = append(filtered, entry)
	}
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		out[key] = value
	}
	out["ImageSet"] = filtered
	out["TotalCount"] = len(filtered)
	return out
}

func dedupeFlatImageRows(items []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(safeString(entry, "CompShareImageId")))
		if key == "" {
			name := strings.ToLower(strings.TrimSpace(bestImageName(entry)))
			author := strings.ToLower(strings.TrimSpace(safeString(entry, "Author")))
			if name != "" || author != "" {
				key = name + "\x00" + author
			}
		}
		if key != "" && seen[key] {
			continue
		}
		if key != "" {
			seen[key] = true
		}
		out = append(out, entry)
	}
	return out
}

func sharedImageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	raw, err := imageExecuteAll(ctx, rt, sharedImageAction, "ImageSet", map[string]any{})
	if err != nil {
		return ImageListResponse{}, ReadFailureAfterTool(sharedImageAction, imageListCapabilityLabel, err)
	}
	// Shared-image results do not expose an evidence envelope.
	reply, empty := renderSharedImageListReply(raw, req.Query, req.Mode)
	if empty {
		return ImageListResponse{}, ReadEmpty(reply)
	}
	return ImageListResponse{
		Reply:  reply,
		Action: sharedImageAction,
	}, ReadResult{}
}

// imageExecuteAll normalizes a nil upstream payload to an empty map.
func imageExecuteAll(ctx context.Context, rt ReadRuntime, action, listKey string, args map[string]any) (map[string]any, error) {
	raw, err := imagecatalogfetch.FetchAll(ctx, rt.Executor.Execute, action, listKey, args)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

// imageFilterQuery is the typed replacement for slotFilterQuery: a "list all"
// mode clears the keyword filter, otherwise the trimmed query filters.
func imageFilterQuery(query string, mode platform.ListMode) string {
	if mode == platform.ListModeAll {
		return ""
	}
	return strings.TrimSpace(query)
}

// populatedEnvelope returns only evidence envelopes with subjects.
func populatedEnvelope(env envelope.Envelope) *envelope.Envelope {
	if len(env.Subjects) == 0 {
		return nil
	}
	return &env
}

func renderImageListReply(raw map[string]any, listKey string, fieldOrder []string, searchQuery string, mode platform.ListMode) string {
	items := mapSliceAt(raw, listKey)
	if len(items) == 0 {
		return noImageListReply
	}
	query := imageFilterQuery(searchQuery, mode)
	// Match the user's primary query against names or the stable image id. Status
	// and type remain non-search fields: asking for "Available" must not dump the
	// entire usable catalog.
	matchFields := primaryImageQueryMatchFields(fieldOrder)

	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if query != "" && len(matchFields) > 0 {
			if !entryMatchesSlotQuery(entry, query, matchFields) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	// "query + 0 matches" -> explicit not-found, do not silently fall
	// through to the full list (that's what confused users in round 1 smoke).
	if query != "" && len(filtered) == 0 {
		return noImageListNoMatchReply
	}
	lines := make([]string, 0, imageListDisplayCap)
	for _, entry := range filtered {
		if len(lines) >= imageListDisplayCap {
			break
		}
		line := formatImageDisplayLine(entry, fieldOrder)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return noImageListReply
	}
	out := strings.Join(lines, "\n")
	if len(filtered) > len(lines) {
		out += fmt.Sprintf("\n（共 %d 个镜像，已显示前 %d 个；可补充关键词进一步筛选）", len(filtered), len(lines))
	}
	return out
}

// formatImageDisplayLine renders one image as a clean, ID-free display line —
// 名称 first, then the human-relevant fields (类型/作者/状态) with 中文 labels
// (imageFieldLabel). Returns "" when there is no name.
func formatImageDisplayLine(entry map[string]any, fieldOrder []string) string {
	name := bestImageName(entry)
	if name == "" {
		return ""
	}
	parts := []string{"名称=" + name}
	seen := map[string]struct{}{}
	for _, key := range fieldOrder {
		if _, skip := imageDisplaySkipFields[key]; skip {
			continue
		}
		v := safeString(entry, key)
		if v == "" {
			continue
		}
		label := imageFieldLabel(key)
		if _, dup := seen[label]; dup {
			continue
		}
		seen[label] = struct{}{}
		parts = append(parts, label+"="+v)
	}
	return strings.Join(parts, ", ")
}

func buildImageListEnvelope(raw map[string]any, listKey string, fieldOrder []string, searchQuery string, mode platform.ListMode, action string, category string) envelope.Envelope {
	items := mapSliceAt(raw, listKey)
	query := imageFilterQuery(searchQuery, mode)
	matchFields := primaryImageQueryMatchFields(fieldOrder)
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if query != "" && len(matchFields) > 0 {
			if !entryMatchesSlotQuery(entry, query, matchFields) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}

	env := envelope.Envelope{
		Kind:          envelope.KindImageList,
		SourceActions: []string{action},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints:   envelope.Constraints{DoNotInventInstances: true},
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "image_category", Label: "Image category", Value: category, Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "total_count", Label: "Total count", Value: len(filtered), Source: envelope.FactSourceComputed},
	)
	// Parse the same rows into typed catalog entries so every image subject carries
	// the STRUCTURED per-candidate facts the central Agent reads to choose an image —
	// source, type, status, container, the real Tags, Description and SoftwareFacts —
	// as discrete keyed facts (Tags/SupportedGpuTypes stay []string values), never a
	// rendered sentence. The raw CompShareImageId / redundant Name are still NOT facts
	// (the id is Subject.ID, the name Subject.Name); the human-facing list is a
	// separate clean render (renderImageListReply) — this envelope is the Agent's
	// evidence, the thing the Agent reasons over and cites.
	byID := map[string]deployment.ImageCatalogEntry{}
	for _, catalogEntry := range deployment.ParsePlatformImageEntries(raw, category) {
		byID[strings.ToLower(catalogEntry.ID)] = catalogEntry
	}
	shown := 0
	for i, entry := range filtered {
		if shown >= imageListDisplayCap {
			break
		}
		id := safeString(entry, "CompShareImageId")
		if id == "" {
			id = fmt.Sprintf("image_%d", i)
		}
		subjectID := "image:" + id
		name := bestImageName(entry)
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID: subjectID, Name: name, Type: envelope.SubjectImage,
		})
		if catalogEntry, ok := byID[strings.ToLower(id)]; ok {
			appendStructuredImageFacts(&env, subjectID, catalogEntry)
		}
		// Author is a listing field, not a catalog-entry field, so it is emitted
		// straight from the row when present (community/custom author attribution).
		if author := safeString(entry, "Author"); author != "" {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID, Key: "author", Label: "作者", Value: author, Source: envelope.FactSourceAPI,
			})
		}
		shown++
	}
	if len(filtered) > shown {
		env.Computed = append(env.Computed, envelope.Fact{
			Key: "display_truncated", Label: "Display truncated",
			Value:  fmt.Sprintf("showing %d of %d images; ask with a keyword to narrow", shown, len(filtered)),
			Source: envelope.FactSourceComputed,
		})
	}
	return env
}

// appendStructuredImageFacts emits, for one image subject, the per-candidate
// structured facts the central Agent reads to make a semantic image choice —
// source, type, status, container, the real Tags, the Description and the
// SoftwareFacts — as DISCRETE keyed facts (Tags and SupportedGpuTypes stay
// []string values, never concatenated into prose), so the Agent reasons over
// structure, not a rendered sentence. Absent fields are OMITTED (honest absence):
// a missing Framework is no fact, never an empty string the Agent could mistake for
// a value, and absent Tags is no tag fact — never a signal to exclude the image.
// Container is the one field emitted unconditionally (as a bool): it is a hard
// runtime-form constraint (a pod requires a container image), so the Agent must
// never have to infer it from the name.
func appendStructuredImageFacts(env *envelope.Envelope, subjectID string, e deployment.ImageCatalogEntry) {
	add := func(key, label string, value any) {
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: key, Label: label, Value: value, Source: envelope.FactSourceAPI,
		})
	}
	if e.Source != "" {
		add("source", "来源", e.Source)
	}
	if e.ImageType != "" {
		add("image_type", "镜像类型", e.ImageType)
	}
	if e.Status != "" {
		add("status", "状态", e.Status)
	}
	add("container", "容器镜像", e.Container)
	if len(e.Tags) > 0 {
		add("tags", "标签", append([]string(nil), e.Tags...))
	}
	if e.Description != "" {
		add("description", "描述", e.Description)
	}
	if len(e.SupportedGPUTypes) > 0 {
		add("supported_gpu_types", "推荐GPU机型", append([]string(nil), e.SupportedGPUTypes...))
	}
	if e.Software.Present {
		if e.Software.Framework != "" {
			add("framework", "框架", e.Software.Framework)
		}
		if e.Software.FrameworkVersion != "" {
			add("framework_version", "框架版本", e.Software.FrameworkVersion)
		}
		if e.Software.CUDAVersion != "" {
			add("cuda_version", "CUDA版本", e.Software.CUDAVersion)
		}
		if e.Software.OsVersion != "" {
			add("os_version", "操作系统", e.Software.OsVersion)
		}
		if e.Software.PythonVersion != "" {
			add("python_version", "Python版本", e.Software.PythonVersion)
		}
	}
}

func renderCommunityImageReply(raw map[string]any, searchQuery string, mode platform.ListMode) string {
	groups := mapSliceAt(raw, "CompshareImageGroup")
	if len(groups) == 0 {
		// Fallback: some responses use a flat ImageSet shape.
		return renderImageListReply(raw, "ImageSet",
			[]string{"Name", "Author", "CompShareImageId"}, searchQuery, mode)
	}
	filtered := make([]map[string]any, 0, len(groups))
	for _, item := range groups {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		filtered = append(filtered, entry)
	}

	filtered = dedupeCommunityImageGroups(filtered)

	// A full browse is popularity-ranked. Filtered input is already the ordered
	// union of the user's primary query followed by semantic expansions; globally
	// sorting that union would let a popular expansion displace an exact match.
	if mode == platform.ListModeAll {
		sort.SliceStable(filtered, func(i, j int) bool {
			return communityDeployCount(filtered[i]) > communityDeployCount(filtered[j])
		})
	}

	lines := make([]string, 0, communityImageGroupLimit)
	lineBudget := communityImageGroupLimit
	for _, entry := range filtered {
		if lineBudget <= 0 {
			break
		}
		header := buildCommunityGroupHeader(entry)
		if header == "" {
			continue
		}
		lines = append(lines, header)
		lineBudget--

		versions := mapSliceAt(entry, "Data")
		shown := 0
		for _, v := range versions {
			if lineBudget <= 0 {
				break
			}
			if shown >= communityVersionPerGroup {
				if len(versions) > shown {
					lines = append(lines, fmt.Sprintf("  ... 共 %d 个版本", len(versions)))
					lineBudget--
				}
				break
			}
			ver, ok := v.(map[string]any)
			if !ok {
				continue
			}
			versionLine := buildCommunityVersionLine(ver)
			if versionLine == "" {
				continue
			}
			lines = append(lines, "  "+versionLine)
			lineBudget--
			shown++
		}
	}
	if len(lines) == 0 {
		return noCommunityReply
	}
	return "社区镜像：\n" + strings.Join(lines, "\n")
}

func buildCommunityImageEnvelope(raw map[string]any, searchQuery string, mode platform.ListMode) envelope.Envelope {
	groups := mapSliceAt(raw, "CompshareImageGroup")
	if len(groups) == 0 {
		// Some community responses use the flat ImageSet shape; the shared builder
		// already flattens those per-image with structured facts.
		return buildImageListEnvelope(raw, "ImageSet",
			[]string{"Name", "Author", "CompShareImageId"}, searchQuery, mode,
			"DescribeCommunityImages", "community")
	}
	filtered := make([]map[string]any, 0, len(groups))
	for _, item := range groups {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		filtered = append(filtered, entry)
	}
	// Keep the structured evidence in the same order as the user-visible result.
	// Full browsing is popularity-ranked; filtered results retain primary-query
	// priority over semantic expansions.
	if mode == platform.ListModeAll {
		sort.SliceStable(filtered, func(i, j int) bool {
			return communityDeployCount(filtered[i]) > communityDeployCount(filtered[j])
		})
	}

	// Flatten every community version into its OWN image subject carrying the same
	// discrete, structured per-candidate facts the platform path emits — so the central
	// Agent, which owns semantic image selection, sees each community image's real Tags,
	// Description, ImageType, Container and SupportedGpuTypes and can cite it by id,
	// exactly as it sees a platform image. The typed catalog parses the real per-version
	// fields (honest absence for whatever a community row omits — usually the Softwares
	// block); group popularity/author/version are attached as per-subject provenance,
	// never a filter.
	byID := map[string]deployment.ImageCatalogEntry{}
	for _, e := range deployment.ParseCommunityImageEntries(raw) {
		byID[strings.ToLower(e.ID)] = e
	}

	env := envelope.Envelope{
		Kind:          envelope.KindImageList,
		SourceActions: []string{"DescribeCommunityImages"},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints:   envelope.Constraints{DoNotInventInstances: true},
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "image_category", Label: "Image category", Value: "community", Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "total_count", Label: "Total count", Value: len(byID), Source: envelope.FactSourceComputed},
	)

	// Breadth before depth: show one version per family before adding another
	// version of the same family.
	families := groupCommunityVersionRows(filtered, byID)
	shown := 0
	for depth := 0; shown < imageListDisplayCap; depth++ {
		advanced := false
		for _, rows := range families {
			if depth >= len(rows) {
				continue
			}
			advanced = true
			appendCommunityImageSubject(&env, rows[depth])
			shown++
			if shown >= imageListDisplayCap {
				break
			}
		}
		if !advanced {
			break
		}
	}
	if len(byID) > shown {
		// Name the family coverage too: "10 of 98 images" reads like a deep slice
		// of a shallow catalog, when what the Agent needs to know before it
		// recommends one is how much of the FIELD it was shown.
		env.Computed = append(env.Computed, envelope.Fact{
			Key: "display_truncated", Label: "Display truncated",
			Value: fmt.Sprintf("showing %d of %d community images across %d of %d image families; ask with a keyword to narrow",
				shown, len(byID), countCommunityFamiliesShown(families, shown), len(families)),
			Source: envelope.FactSourceComputed,
		})
	}
	return env
}

// communityVersionRow is one community image version paired with the family row
// it came from, so the round-robin pass can emit group provenance (author,
// 部署次数) without re-walking the groups.
type communityVersionRow struct {
	id      string
	version map[string]any
	entry   deployment.ImageCatalogEntry
	group   map[string]any
}

// groupCommunityVersionRows flattens each family's version rows in catalog order,
// keeping only versions that carry an id AND parsed into a typed catalog entry
// (an id-less or unparsed row is an honest drop, never a synthetic subject).
// Families keep their incoming popularity order; families that contribute no
// usable version are omitted entirely rather than left as empty slots.
func groupCommunityVersionRows(groups []map[string]any, byID map[string]deployment.ImageCatalogEntry) [][]communityVersionRow {
	seen := map[string]bool{}
	families := make([][]communityVersionRow, 0, len(groups))
	for _, group := range groups {
		rows := make([]communityVersionRow, 0, 4)
		for _, v := range mapSliceAt(group, "Data") {
			ver, ok := v.(map[string]any)
			if !ok {
				continue
			}
			id := safeString(ver, "CompShareImageId")
			if id == "" {
				continue
			}
			key := strings.ToLower(id)
			if seen[key] {
				continue
			}
			entry, ok := byID[key]
			if !ok {
				continue
			}
			seen[key] = true
			rows = append(rows, communityVersionRow{id: id, version: ver, entry: entry, group: group})
		}
		if len(rows) > 0 {
			families = append(families, rows)
		}
	}
	return families
}

// countCommunityFamiliesShown reports how many families the first `shown`
// round-robin emissions covered — the breadth figure the truncation note quotes.
// Depth 0 visits every family exactly once (a family with no usable version was
// dropped when the rows were grouped), so the count is simply whichever ran out
// first: the cap or the field.
func countCommunityFamiliesShown(families [][]communityVersionRow, shown int) int {
	if shown < len(families) {
		return shown
	}
	return len(families)
}

// appendCommunityImageSubject emits one community version as an image subject
// with the same structured per-candidate facts the platform path emits, plus the
// group provenance attached per subject (never a filter): author, the family's
// 部署次数 popularity, and the version's own label.
func appendCommunityImageSubject(env *envelope.Envelope, row communityVersionRow) {
	subjectID := "image:" + row.id
	name := row.entry.Name
	if name == "" {
		name = communityGroupName(row.group)
	}
	env.Subjects = append(env.Subjects, envelope.Subject{
		ID: subjectID, Name: name, Type: envelope.SubjectImage,
	})
	appendStructuredImageFacts(env, subjectID, row.entry)
	author := safeString(row.version, "Author")
	if author == "" {
		author = safeString(row.group, "Author")
	}
	if author != "" {
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: "author", Label: "作者", Value: author, Source: envelope.FactSourceAPI,
		})
	}
	if label := communityVersionLabel(row.version); label != "" {
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: "version", Label: "版本", Value: label, Source: envelope.FactSourceAPI,
		})
	}
	if deployCount := communityDeployCount(row.group); deployCount > 0 {
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: "deploy_count", Label: "部署次数", Value: int(deployCount), Source: envelope.FactSourceAPI,
		})
	}
}

// communityVersionLabel returns the version-specific label (e.g. "v26.0529",
// "latest") from a community version row — distinct from the family (group) name that
// becomes the subject name. Absent → "" (no fact emitted; honest absence).
func communityVersionLabel(ver map[string]any) string {
	for _, key := range []string{"VersionName", "Version", "Name"} {
		if v := safeString(ver, key); v != "" {
			return v
		}
	}
	return ""
}

// renderSharedImageListReply returns the reply and whether the shared-image list
// is empty (no images or none matching). Shared images carry no evidence
// envelope, so this bool is how the handler reports a structured Empty read.
func renderSharedImageListReply(raw map[string]any, searchQuery string, mode platform.ListMode) (string, bool) {
	items := mapSliceAt(raw, "ImageSet")
	if len(items) == 0 {
		return "未获取到共享给你的镜像。", true
	}
	query := imageFilterQuery(searchQuery, mode)
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if query != "" && !entryMatchesSlotQuery(entry, query, []string{"Name", "Description", "CompShareImageId", "ImageType", "Status"}) {
			continue
		}
		filtered = append(filtered, entry)
	}
	if query != "" && len(filtered) == 0 {
		return "未找到匹配的共享镜像。", true
	}
	lines := []string{}
	for _, entry := range filtered {
		line := buildSharedImageLine(entry)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) >= 20 {
			break
		}
	}
	if len(lines) == 0 {
		return "未获取到共享给你的镜像。", true
	}
	prefix := "共享给你的镜像"
	if total := strings.TrimSpace(safeString(raw, "TotalCount")); total != "" && total != "0" {
		prefix += "（共 " + total + " 个）"
	}
	return prefix + ":\n" + strings.Join(lines, "\n"), false
}

func buildSharedImageLine(entry map[string]any) string {
	name := bestImageName(entry)
	if name == "" {
		return ""
	}
	// 名称 first, raw CompShareImageId dropped (用户按名称引用即可) — same clean style
	// as the platform image list.
	parts := []string{"名称=" + name}
	for _, key := range []string{"ImageType", "Status"} {
		if v := strings.TrimSpace(safeString(entry, key)); v != "" {
			parts = append(parts, imageFieldLabel(key)+"="+v)
		}
	}
	if v := strings.TrimSpace(safeString(entry, "Container")); v != "" {
		parts = append(parts, "容器="+v)
	}
	if owner := sharedImageOwnerDisplay(entry); owner != "" {
		parts = append(parts, "所有者="+owner)
	}
	return strings.Join(parts, ", ")
}

func sharedImageOwnerDisplay(entry map[string]any) string {
	owner, ok := entry["Owner"].(map[string]any)
	if !ok || owner == nil {
		return ""
	}
	if name := strings.TrimSpace(safeString(owner, "AccountName")); name != "" {
		return name
	}
	if id := strings.TrimSpace(safeString(owner, "AccountId")); id != "" && id != "0" {
		return id
	}
	return ""
}

func buildCommunityGroupHeader(entry map[string]any) string {
	parts := []string{}
	// Live DescribeCommunityImages carries the group name in ImageName (group-level
	// Name is empty); communityGroupName reads ImageName||Name.
	if name := communityGroupName(entry); name != "" {
		parts = append(parts, "名称="+name)
	}
	if v := safeString(entry, "Author"); v != "" {
		parts = append(parts, "作者="+v)
	}
	if n := communityDeployCount(entry); n > 0 {
		parts = append(parts, fmt.Sprintf("部署次数=%d", n))
	}
	versions := mapSliceAt(entry, "Data")
	if len(versions) > 0 {
		parts = append(parts, fmt.Sprintf("版本数=%d", len(versions)))
	}
	return strings.Join(parts, ", ")
}

// communityDeployCount reads CreatedCount (the catalog's 部署次数 popularity
// signal) from a community-image group, falling back to its first version.
func communityDeployCount(entry map[string]any) int64 {
	if n := numericFieldInt(entry, "CreatedCount"); n > 0 {
		return n
	}
	if versions := mapSliceAt(entry, "Data"); len(versions) > 0 {
		if v0, ok := versions[0].(map[string]any); ok {
			return numericFieldInt(v0, "CreatedCount")
		}
	}
	return 0
}

// numericFieldInt reads a JSON-decoded numeric field as int64 (live responses
// decode numbers to float64).
func numericFieldInt(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func buildCommunityVersionLine(ver map[string]any) string {
	// 版本名 first, raw CompShareImageId dropped from display (用户按名称引用即可).
	parts := []string{}
	if name := bestImageName(ver); name != "" {
		parts = append(parts, "版本="+name)
	}
	for _, key := range []string{"VersionName", "Version"} {
		if v := safeString(ver, key); v != "" {
			parts = append(parts, "版本号="+v)
			break
		}
	}
	return strings.Join(parts, ", ")
}

func communityGroupName(entry map[string]any) string {
	if name := safeString(entry, "ImageName"); name != "" {
		return name
	}
	return safeString(entry, "Name")
}

func bestImageName(entry map[string]any) string {
	for _, key := range []string{"Name", "CompShareImageName", "ImageName"} {
		if v := safeString(entry, key); v != "" {
			return v
		}
	}
	return ""
}

func imageFieldLabel(key string) string {
	switch key {
	case "CompShareImageId":
		return "镜像ID"
	case "CompShareImageName":
		return "镜像名称"
	case "ImageName":
		return "镜像名"
	case "ImageType":
		return "镜像类型"
	case "Name":
		return "名称"
	case "Status":
		return "状态"
	case "Author":
		return "作者"
	default:
		return key
	}
}

func dedupeCommunityImageGroups(groups []map[string]any) []map[string]any {
	if len(groups) <= 1 {
		return groups
	}
	bestByName := map[string]map[string]any{}
	order := []string{}
	for _, group := range groups {
		name := strings.TrimSpace(bestImageName(group))
		if name == "" {
			name = strings.TrimSpace(safeString(group, "ImageName"))
		}
		key := strings.ToLower(name)
		if key == "" {
			key = fmt.Sprintf("__unnamed_%d", len(order))
		}
		if _, ok := bestByName[key]; !ok {
			order = append(order, key)
			bestByName[key] = group
			continue
		}
		if communityDeployCount(group) > communityDeployCount(bestByName[key]) {
			bestByName[key] = group
		}
	}
	out := make([]map[string]any, 0, len(bestByName))
	for _, key := range order {
		if group := bestByName[key]; group != nil {
			out = append(out, group)
		}
	}
	return out
}
