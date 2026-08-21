package capability

import (
	"context"
	"fmt"
	"strings"

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
	Query string            `json:"query,omitempty"`
	Mode  platform.ListMode `json:"mode,omitempty"`
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
		Description: "查询公共模型仓库中的预置权重和标签。用于下载、源码安装、adapter 或权重路径问题，或社区镜像没有精确候选时补充信息；它不是可创建的镜像目录，不能证明平台已支持部署。",
		Params:      objectParam(map[string]schemaNode{"query": stringParam(), "mode": enumParam(platform.ListModeValues()...)}),
		Handle:      modelRepositoryHandle,
		Render:      modelRepositoryRender,
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
	args := modelRepositoryArgs(req.Query, req.Mode, tagRaw)
	modelRaw, err := rt.Executor.Execute(ctx, modelRepositoryModelAction, args)
	if err != nil {
		return ModelRepositoryResponse{}, ReadFailureAfterTool(modelRepositoryModelAction, modelRepositoryCapabilityLabel, err)
	}
	if modelRaw == nil {
		modelRaw = map[string]any{}
	}
	reply, empty := renderModelRepositoryReply(modelRaw, tagRaw, req.Query, req.Mode)
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

func modelRepositoryArgs(query string, mode platform.ListMode, tagRaw map[string]any) map[string]any {
	args := map[string]any{}
	query = strings.TrimSpace(query)
	matchedTags := matchModelRepositoryTags(query, uniqueStrings(stringSliceAt(tagRaw, "Tags")))
	if len(matchedTags) > 0 {
		args["tags"] = strings.Join(limitStrings(matchedTags, 3), ",")
		return args
	}
	if query != "" && mode != platform.ListModeAll {
		args["name"] = strings.ToLower(query)
	}
	return args
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
func renderModelRepositoryReply(modelRaw, tagRaw map[string]any, query string, mode platform.ListMode) (string, bool) {
	tags := uniqueStrings(stringSliceAt(tagRaw, "Tags"))
	models := mapSliceAt(modelRaw, "Models")
	filtered := filterModelRepositoryModels(models, query, mode)
	sections := []string{}
	if len(tags) > 0 {
		sections = append(sections, "模型仓库标签: "+strings.Join(limitStrings(tags, 20), "、"))
	}
	if len(filtered) == 0 {
		if len(tags) > 0 {
			sections = append(sections, "未找到匹配的模型。", modelRepositoryGuidanceFooter(false))
			return strings.Join(sections, "\n"), false
		}
		return "未获取到模型仓库数据。", true
	}
	allLines := []string{}
	for _, entry := range filtered {
		line := buildModelRepositoryLine(entry)
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
	sections = append(sections, modelRepositoryGuidanceFooter(true))
	return strings.Join(sections, "\n"), false
}

// modelRepositoryGuidanceFooter bridges repository results to the two supported
// follow-ups: use the returned per-entry path, or download a missing model inside
// an instance. It does not hardcode a repository mount layout.
func modelRepositoryGuidanceFooter(found bool) string {
	if !found {
		return "仓库里暂时没有匹配的模型。你可以部署实例后自行拉取：HuggingFace / ModelScope 下载，或 Ollama 容器用 `ollama pull <模型名>`——需要具体命令可以问我。"
	}
	return strings.Join([]string{
		"说明：以上模型已预置在实例对应的 Path 路径下（见每条的 Path，按来源分布在 /model/HuggingFace、/model/ModelScope、/model/ollama 等），部署实例后可直接加载，无需重新下载。",
		"· 想部署某个模型，直接告诉我模型名（如「部署 Llama-3.1-8B」），我来帮你选 GPU 配置。",
		"· 仓库里没有的模型，可在实例内自行拉取（HuggingFace / ModelScope 下载，或 Ollama `ollama pull <模型名>`）——需要命令可以问我。",
	}, "\n")
}

func filterModelRepositoryModels(models []any, query string, mode platform.ListMode) []map[string]any {
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
	if len(out) == 0 || mode == platform.ListModeAll {
		return out
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return out
	}
	filtered := make([]map[string]any, 0, len(out))
	for _, entry := range out {
		if entryMatchesSlotQuery(entry, query, []string{"Name", "Path", "Tag"}) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func buildModelRepositoryLine(entry map[string]any) string {
	parts := []string{}
	for _, key := range []string{"Name", "Size", "Tag", "Path"} {
		if v := strings.TrimSpace(safeString(entry, key)); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}
