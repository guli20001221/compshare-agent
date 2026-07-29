package capability

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Image-tag-catalog read capability (migrated from the legacy intent route).
// The legacy handler made a single DescribeCompShareImageTags call and rendered
// the tag categories; it carried no request fields and no evidence envelope, so
// this capability is reply-only (like network_accelerator_status).

const (
	imageTagCatalogCapabilityLabel = string(intent.IntentImageTagCatalog)
	imageTagCatalogAction          = "DescribeCompShareImageTags"
)

// ImageTagCatalogRequest is the capability's own request contract — the platform
// image-tag catalog takes no parameters.
type ImageTagCatalogRequest struct{}

// MissingFields: none — the catalog is account-scoped and parameterless.
func (ImageTagCatalogRequest) MissingFields() []platform.MissingField { return nil }

// ImageTagCatalogResponse carries the rendered reply. Parity with the legacy
// route: there is no evidence envelope for this capability.
type ImageTagCatalogResponse struct {
	Reply string
}

func imageTagCatalogReadSpec() ReadCapabilitySpec[ImageTagCatalogRequest, ImageTagCatalogResponse] {
	return ReadCapabilitySpec[ImageTagCatalogRequest, ImageTagCatalogResponse]{
		Label:        imageTagCatalogCapabilityLabel,
		Description:  "查询平台镜像可用标签和分类目录。只返回标签体系，不返回具体镜像；具体镜像使用镜像列表能力。",
		Presentation: ReadPresentationBrowse,
		Params:       objectParam(nil),
		Handle:       imageTagCatalogHandle,
		Render:       imageTagCatalogRender,
	}
}

func imageTagCatalogHandle(ctx context.Context, _ ImageTagCatalogRequest, rt ReadRuntime) (ImageTagCatalogResponse, ReadResult) {
	raw, err := rt.Executor.Execute(ctx, imageTagCatalogAction, map[string]any{})
	if err != nil {
		return ImageTagCatalogResponse{}, ReadFailureAfterTool(imageTagCatalogAction, imageTagCatalogCapabilityLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	reply, empty := renderImageTagCatalogReply(raw)
	if empty {
		return ImageTagCatalogResponse{}, ReadEmpty(reply)
	}
	return ImageTagCatalogResponse{Reply: reply}, ReadResult{}
}

func imageTagCatalogRender(resp ImageTagCatalogResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = imageTagCatalogAction
	return r
}

// --- Relocated verbatim from intent/routing_registry.go -------------------------

// renderImageTagCatalogReply returns the catalog reply and whether the payload
// was empty (no categorized tags and no flat Tags) — an empty catalog is a
// structured Empty read, not a Handled answer.
func renderImageTagCatalogReply(raw map[string]any) (string, bool) {
	tagIndex := stringSliceAt(raw, "TagIndex")
	tagsMap := stringSliceMapAt(raw, "TagsMap")
	lines := []string{}
	for _, category := range tagIndex {
		tags := tagsMap[category]
		if len(tags) == 0 {
			continue
		}
		lines = append(lines, category+": "+strings.Join(limitStrings(tags, 12), "、"))
	}
	if len(lines) == 0 {
		tags := stringSliceAt(raw, "Tags")
		if len(tags) == 0 {
			return "未获取到镜像标签。", true
		}
		return "镜像标签: " + strings.Join(limitStrings(tags, 30), "、"), false
	}
	return "镜像标签分类:\n" + strings.Join(lines, "\n"), false
}
