package capability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runImageList(t *testing.T, exec ReadExecutor, req ImageListRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(imageListReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec})
}

func TestImageListRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, ImageListRequest{}.MissingFields())
}

// TestImageListHandle_PlatformEmpty: an empty platform image catalog is a
// structured Empty read (issue 1) — no subjects populated, so no envelope.
func TestImageListHandle_PlatformEmpty(t *testing.T) {
	result := runImageList(t, &fakeReadExec{result: map[string]any{"ImageSet": []any{}}}, ImageListRequest{})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Nil(t, result.Envelope)
}

// TestImageListHandle_SharedEmpty: shared images carry no envelope, so the shared
// handler reports its own emptiness as a structured Empty read.
func TestImageListHandle_SharedEmpty(t *testing.T) {
	result := runImageList(t, &fakeReadExec{result: map[string]any{"ImageSet": []any{}}},
		ImageListRequest{Source: platform.ImageSourceShared})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Contains(t, result.Reply, "未获取到共享给你的镜像")
}

// --- platform render parity -----------------------------------------------------

func TestImageListRender_PlatformFilterAndCleanDisplay(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-1", "ImageName": "Ubuntu 22.04 LTS", "ImageType": "System"},
		map[string]any{"CompShareImageId": "img-2", "ImageName": "PyTorch 2.1", "ImageType": "App"},
	}}
	fieldOrder := []string{"CompShareImageId", "ImageName", "ImageType"}

	filtered := renderImageListReply(raw, "ImageSet", fieldOrder, "Ubuntu 22.04", platform.ListModeFiltered)
	assert.Contains(t, filtered, "Ubuntu 22.04 LTS")
	assert.NotContains(t, filtered, "PyTorch")

	noMatch := renderImageListReply(raw, "ImageSet", fieldOrder, "Debian 12", platform.ListModeFiltered)
	assert.Contains(t, noMatch, "未找到匹配的镜像")

	all := renderImageListReply(raw, "ImageSet", fieldOrder, "", platform.ListModeAll)
	assert.Contains(t, all, "名称=Ubuntu 22.04 LTS")
	assert.Contains(t, all, "镜像类型=System")
	assert.NotContains(t, all, "img-1", "raw CompShareImageId dropped from clean display")
}

func TestImageListRender_CapAndOverflow(t *testing.T) {
	total := imageListDisplayCap + 7
	items := make([]any, 0, total)
	for i := 0; i < total; i++ {
		items = append(items, map[string]any{"CompShareImageId": fmt.Sprintf("img-%d", i), "Name": fmt.Sprintf("img-name-%d", i)})
	}
	reply := renderImageListReply(map[string]any{"ImageSet": items}, "ImageSet", []string{"CompShareImageId", "Name"}, "", platform.ListModeAll)
	require.Equal(t, imageListDisplayCap, strings.Count(reply, "名称="))
	assert.Contains(t, reply, fmt.Sprintf("共 %d 个镜像", total))
	assert.Contains(t, reply, "可补充关键词")
}

// --- community render parity ----------------------------------------------------

func TestImageListRender_CommunityShowsGroupsAndVersions(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "LiveTalking", "Author": "team", "CreatedCount": float64(200), "Data": []any{
			map[string]any{"CompShareImageId": "live-1", "Name": "数字人实时对话版"},
		}},
		map[string]any{"ImageName": "LTX-2.3", "CreatedCount": float64(500), "Data": []any{
			map[string]any{"CompShareImageId": "ltx-1", "Name": "LTX-v1"},
		}},
	}}
	reply := renderCommunityImageReply(raw, "数字人", platform.ListModeFiltered)
	assert.Contains(t, reply, "社区镜像")
	assert.Contains(t, reply, "LiveTalking")
	assert.Contains(t, reply, "LTX-2.3")
	// Most-deployed group (LTX, 500) sorts above LiveTalking (200).
	assert.Less(t, strings.Index(reply, "LTX-2.3"), strings.Index(reply, "LiveTalking"))
}

// --- shared render parity -------------------------------------------------------

func TestImageListRender_Shared(t *testing.T) {
	raw := map[string]any{
		"TotalCount": float64(1),
		"ImageSet": []any{map[string]any{
			"CompShareImageId": "img-shared-1", "Name": "shared-env", "ImageType": "Custom",
			"Status": "Available", "Container": "True",
			"Owner": map[string]any{"AccountName": "team-a", "AccountId": float64(123)},
		}},
	}
	all, allEmpty := renderSharedImageListReply(raw, "", platform.ListModeAll)
	assert.False(t, allEmpty, "a populated shared list is not empty")
	for _, want := range []string{"共享给你的镜像", "名称=shared-env", "所有者=team-a"} {
		assert.Contains(t, all, want)
	}
	assert.NotContains(t, all, "img-shared-1")

	noMatch, noMatchEmpty := renderSharedImageListReply(raw, "llama", platform.ListModeFiltered)
	assert.True(t, noMatchEmpty, "a no-match shared list is a structured Empty read")
	assert.Contains(t, noMatch, "未找到匹配的共享镜像")

	emptyReply, empty := renderSharedImageListReply(map[string]any{}, "", platform.ListModeAll)
	assert.True(t, empty)
	assert.Contains(t, emptyReply, "未获取到共享给你的镜像")
}

// --- envelope parity ------------------------------------------------------------

func TestImageListEnvelope_PlatformDropsRawIDFacts(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-pt", "Name": "PyTorch 2.9", "ImageType": "App"},
	}}
	fieldOrder := []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"}
	env := buildImageListEnvelope(raw, "ImageSet", fieldOrder, "", platform.ListModeAll, "DescribeCompShareImages", "platform")

	assert.Equal(t, envelope.KindImageList, env.Kind)
	for _, f := range env.Facts {
		assert.NotEqual(t, "CompShareImageId", f.Key, "raw id must not be a display fact")
		assert.NotEqual(t, "Name", f.Key, "name must not be a display fact")
	}
	require.Len(t, env.Subjects, 1)
	assert.Equal(t, "image:img-pt", env.Subjects[0].ID)
	assert.Equal(t, "PyTorch 2.9", env.Subjects[0].Name)
}

func TestImageListEnvelope_CommunitySortsByDeployCount(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "Low", "CreatedCount": float64(10), "Data": []any{}},
		map[string]any{"ImageName": "High", "CreatedCount": float64(900), "Data": []any{}},
	}}
	env := buildCommunityImageEnvelope(raw, "", platform.ListModeAll)
	require.Len(t, env.Subjects, 2)
	assert.Equal(t, "image_group:High", env.Subjects[0].ID, "most-deployed group must sort first")
}

// --- handler source-facet dispatch ----------------------------------------------

func TestImageListHandle_SourceFacetDispatch(t *testing.T) {
	imageSet := map[string]any{"ImageSet": []any{map[string]any{"CompShareImageId": "i1", "Name": "img"}}}
	group := map[string]any{"CompshareImageGroup": []any{map[string]any{"ImageName": "G", "CreatedCount": float64(1), "Data": []any{}}}}

	cases := []struct {
		name         string
		source       platform.ImageSource
		result       map[string]any
		wantAction   string
		wantEnvelope bool
	}{
		{"platform", platform.ImageSourcePlatform, imageSet, "DescribeCompShareImages", true},
		{"custom", platform.ImageSourceCustom, imageSet, "DescribeCompShareCustomImages", true},
		{"community", platform.ImageSourceCommunity, group, "DescribeCommunityImages", true},
		{"shared", platform.ImageSourceShared, imageSet, "DescribeCompShareSharingImages", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeReadExec{result: tc.result}
			result := runImageList(t, exec, ImageListRequest{Source: tc.source})
			require.Equal(t, platform.ReadStatusHandled, result.Status)
			assert.Equal(t, tc.wantAction, result.ToolAction)
			require.Len(t, exec.calls, 1)
			assert.Equal(t, tc.wantAction, exec.calls[0].action)
			if tc.wantEnvelope {
				assert.NotNil(t, result.Envelope, "platform/custom/community carry an evidence envelope")
			} else {
				assert.Nil(t, result.Envelope, "shared images carry no envelope (legacy parity)")
			}
		})
	}
}

func TestImageListHandle_CommunityFuzzySearchArg(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"CompshareImageGroup": []any{}}}
	runImageList(t, exec, ImageListRequest{Source: platform.ImageSourceCommunity, Query: "数字人", Mode: platform.ListModeFiltered})
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "数字人", exec.calls[0].args["FuzzySearch"])
}

func TestImageListHandle_UpstreamError(t *testing.T) {
	result := runImageList(t, errReadExec{err: errors.New("boom")}, ImageListRequest{})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, imageListCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}
