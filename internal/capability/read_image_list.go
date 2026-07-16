package capability

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Image-list read capability (migrated from the legacy intent route). The single
// image_list tool fans out on the request's Source facet to the platform, custom,
// community or shared listing — each its own upstream action, renderer and (for
// platform/custom/community) evidence envelope. The typed request carries the
// source + query + list mode, so the handler never re-reads the user's sentence
// (the legacy path re-derived them from Slots).

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
	Source platform.ImageSource `json:"source,omitempty"`
	Query  string               `json:"query,omitempty"`
	Mode   platform.ListMode    `json:"mode,omitempty"`
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
		Description: "查询平台、自制、社区或共享镜像。",
		Schema:      objectSchema(map[string]any{"source": enumSchema("platform", "custom", "community", "shared"), "query": stringSchema(), "mode": enumSchema("all", "filtered")}, nil),
		Handle:      imageListHandle,
		Render:      imageListRender,
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

func imageListRender(resp ImageListResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = resp.Action
	r.Envelope = resp.Envelope
	return r
}

func platformImageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	fieldOrder := []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"}
	raw, err := imageExecute(ctx, rt, platformImageAction, map[string]any{})
	if err != nil {
		return ImageListResponse{}, ReadFailureAfterTool(platformImageAction, imageListCapabilityLabel, err)
	}
	env := buildImageListEnvelope(raw, "ImageSet", fieldOrder, req.Query, req.Mode, platformImageAction, "platform")
	return ImageListResponse{
		Reply:    renderImageListReply(raw, "ImageSet", fieldOrder, req.Query, req.Mode),
		Action:   platformImageAction,
		Envelope: populatedEnvelope(env),
	}, ReadResult{}
}

func customImageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	fieldOrder := []string{"CompShareImageId", "Name", "ImageName", "Status"}
	raw, err := imageExecute(ctx, rt, customImageAction, map[string]any{})
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
	args := map[string]any{}
	if query := strings.TrimSpace(req.Query); query != "" {
		args["FuzzySearch"] = query
	}
	raw, err := imageExecute(ctx, rt, communityImageAction, args)
	if err != nil {
		return ImageListResponse{}, ReadFailureAfterTool(communityImageAction, imageListCapabilityLabel, err)
	}
	env := buildCommunityImageEnvelope(raw, req.Query, req.Mode)
	return ImageListResponse{
		Reply:    renderCommunityImageReply(raw, req.Query, req.Mode),
		Action:   communityImageAction,
		Envelope: populatedEnvelope(env),
	}, ReadResult{}
}

func sharedImageListHandle(ctx context.Context, req ImageListRequest, rt ReadRuntime) (ImageListResponse, ReadResult) {
	raw, err := imageExecute(ctx, rt, sharedImageAction, map[string]any{"Limit": 20})
	if err != nil {
		return ImageListResponse{}, ReadFailureAfterTool(sharedImageAction, imageListCapabilityLabel, err)
	}
	// Shared images carry no evidence envelope (legacy parity).
	reply, empty := renderSharedImageListReply(raw, req.Query, req.Mode)
	if empty {
		return ImageListResponse{}, ReadEmpty(reply)
	}
	return ImageListResponse{
		Reply:  reply,
		Action: sharedImageAction,
	}, ReadResult{}
}

// imageExecute runs an upstream image action, normalising a nil payload to an
// empty map exactly as the legacy executeRouteAction did.
func imageExecute(ctx context.Context, rt ReadRuntime, action string, args map[string]any) (map[string]any, error) {
	raw, err := rt.Executor.Execute(ctx, action, args)
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

// populatedEnvelope returns &env only when it carries subjects, mirroring the
// legacy setEnvelopeIfPopulated gate.
func populatedEnvelope(env envelope.Envelope) *envelope.Envelope {
	if len(env.Subjects) == 0 {
		return nil
	}
	return &env
}

// --- Relocated from intent/routing_registry.go (Slots → typed query/mode) -------

func renderImageListReply(raw map[string]any, listKey string, fieldOrder []string, searchQuery string, mode platform.ListMode) string {
	items := mapSliceAt(raw, listKey)
	if len(items) == 0 {
		return noImageListReply
	}
	query := imageFilterQuery(searchQuery, mode)
	// Match keywords against name-like fields only (not status/id/type).
	matchFields := []string{}
	for _, f := range fieldOrder {
		switch f {
		case "Name", "ImageName", "CompShareImageName", "Author":
			matchFields = append(matchFields, f)
		}
	}

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
	matchFields := []string{}
	for _, f := range fieldOrder {
		switch f {
		case "Name", "ImageName", "CompShareImageName", "Author":
			matchFields = append(matchFields, f)
		}
	}
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
		// Skip the raw-id / redundant-name display facts so the grounded renderer
		// does not dump CompShareImageId per row — the id lives in Subject.ID and the
		// name in Subject.Name. Keeps the rendered list clean (类型/状态/作者 only).
		for _, key := range fieldOrder {
			if _, skip := imageDisplaySkipFields[key]; skip {
				continue
			}
			v := safeString(entry, key)
			if v == "" {
				continue
			}
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID, Key: key, Label: imageFieldLabel(key), Value: v, Source: envelope.FactSourceAPI,
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

	// Surface genuinely-popular images first: the live API default order is
	// recommend-weighted, so sort by CreatedCount (部署次数) desc to make the
	// popularity figures monotonic and put the most-deployed images on top.
	sort.SliceStable(filtered, func(i, j int) bool {
		return communityDeployCount(filtered[i]) > communityDeployCount(filtered[j])
	})

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
	// Surface genuinely-popular images first: live API order is recommend-weighted,
	// so sort subjects by CreatedCount (部署次数) desc — the grounded renderer lists
	// subjects in envelope order.
	sort.SliceStable(filtered, func(i, j int) bool {
		return communityDeployCount(filtered[i]) > communityDeployCount(filtered[j])
	})

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
		envelope.Fact{Key: "total_count", Label: "Total count", Value: len(filtered), Source: envelope.FactSourceComputed},
	)
	lineBudget := communityImageGroupLimit
	for _, entry := range filtered {
		if lineBudget <= 0 {
			break
		}
		name := communityGroupName(entry)
		if name == "" {
			continue
		}
		subjectID := "image_group:" + name
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID: subjectID, Name: name, Type: envelope.SubjectImageGroup,
		})
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: "group_name", Label: "名称", Value: name, Source: envelope.FactSourceAPI,
		})
		if author := safeString(entry, "Author"); author != "" {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID, Key: "author", Label: "作者", Value: author, Source: envelope.FactSourceAPI,
			})
		}
		versions := mapSliceAt(entry, "Data")
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: "version_count", Label: "版本数", Value: len(versions), Source: envelope.FactSourceAPI,
		})
		if dc := communityDeployCount(entry); dc > 0 {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID, Key: "deploy_count", Label: "部署次数", Value: int(dc), Source: envelope.FactSourceAPI,
			})
		}
		lineBudget--
		shown := 0
		for _, v := range versions {
			if lineBudget <= 0 || shown >= communityVersionPerGroup {
				break
			}
			ver, ok := v.(map[string]any)
			if !ok {
				continue
			}
			parts := []string{}
			for _, key := range []string{"CompShareImageId", "Name", "VersionName", "Version"} {
				if val := safeString(ver, key); val != "" {
					parts = append(parts, imageFieldLabel(key)+"="+val)
				}
			}
			if len(parts) == 0 {
				continue
			}
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID,
				Key:       fmt.Sprintf("version_%d", shown+1),
				Label:     fmt.Sprintf("版本%d", shown+1),
				Value:     strings.Join(parts, ", "),
				Source:    envelope.FactSourceAPI,
			})
			lineBudget--
			shown++
		}
		if len(versions) > shown {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID,
				Key:       "versions_truncated",
				Label:     "版本截断",
				Value:     fmt.Sprintf("共 %d 个版本，仅展示 %d 个", len(versions), shown),
				Source:    envelope.FactSourceComputed,
			})
		}
	}
	return env
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
