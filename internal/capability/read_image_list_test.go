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

// TestImageListEnvelope_AgentSeesStructuredCandidateFacts is acceptance gate #1 (and
// the red half of gate #8): the image_list evidence the central Agent consumes must
// carry each candidate's REAL Tags (as a structured []string, never concatenated
// prose), its Description, source, runtime form and the structured SoftwareFacts —
// this is exactly what lets the Agent map a natural-language goal ("大模型推理",
// "深度学习") to a real image WITHOUT any keyword table. Deleting the
// appendStructuredImageFacts wiring turns this red.
func TestImageListEnvelope_AgentSeesStructuredCandidateFacts(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId":  "img-torch",
			"Name":              "PyTorch 2.9.1",
			"ImageType":         "App",
			"Status":            "Available",
			"Container":         "True",
			"Description":       "PyTorch 深度学习基础镜像",
			"Tags":              []any{"深度学习", "PyTorch"},
			"SupportedGpuTypes": []any{"4090", "A800"},
			"Softwares": map[string]any{
				"Framework": "PyTorch", "FrameworkVersion": "2.9.1",
				"CUDAVersion": "12.8", "OsVersion": "Ubuntu 22.04", "PythonVersion": "3.12",
			},
		},
	}}
	result := runImageList(t, &fakeReadExec{result: raw}, ImageListRequest{Source: platform.ImageSourcePlatform})
	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.NotNil(t, result.Envelope)

	facts := map[string]any{}
	for _, f := range result.Envelope.Facts {
		if f.SubjectID == "image:img-torch" {
			facts[f.Key] = f.Value
		}
	}

	tags, ok := facts["tags"].([]string)
	require.True(t, ok, "tags must be a structured []string, got %T", facts["tags"])
	assert.Equal(t, []string{"深度学习", "PyTorch"}, tags)
	gpus, ok := facts["supported_gpu_types"].([]string)
	require.True(t, ok, "supported_gpu_types must be a structured []string, got %T", facts["supported_gpu_types"])
	assert.Equal(t, []string{"4090", "A800"}, gpus)
	assert.Equal(t, "PyTorch 深度学习基础镜像", facts["description"])
	assert.Equal(t, "platform", facts["source"])
	assert.Equal(t, "App", facts["image_type"])
	assert.Equal(t, "Available", facts["status"])
	assert.Equal(t, true, facts["container"])
	assert.Equal(t, "PyTorch", facts["framework"])
	assert.Equal(t, "12.8", facts["cuda_version"])
	assert.Equal(t, "Ubuntu 22.04", facts["os_version"])
	assert.Equal(t, "3.12", facts["python_version"])
}

// TestImageListEnvelope_HonestAbsenceNoTagFabrication pins the honest-absence half:
// a bare image with no upstream Tags/Software emits NO tags fact and NO software
// facts — never an empty string or empty list the Agent might read as "matches no
// tag" and use to exclude the image (gate #3's data-side guarantee).
func TestImageListEnvelope_HonestAbsenceNoTagFabrication(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-bare", "Name": "Ubuntu 22.04", "ImageType": "System", "Status": "Available"},
	}}
	result := runImageList(t, &fakeReadExec{result: raw}, ImageListRequest{Source: platform.ImageSourcePlatform})
	require.NotNil(t, result.Envelope)
	for _, f := range result.Envelope.Facts {
		if f.SubjectID != "image:img-bare" {
			continue
		}
		switch f.Key {
		case "tags", "description", "framework", "cuda_version", "os_version", "python_version":
			t.Errorf("bare image must emit no %q fact (honest absence, not empty-match), got %v", f.Key, f.Value)
		}
	}
}

func TestImageListEnvelope_CommunitySortsByDeployCount(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "Low", "CreatedCount": float64(10), "Data": []any{
			map[string]any{"CompShareImageId": "cimg-low", "Name": "v1"},
		}},
		map[string]any{"ImageName": "High", "CreatedCount": float64(900), "Data": []any{
			map[string]any{"CompShareImageId": "cimg-high", "Name": "v1"},
		}},
	}}
	env := buildCommunityImageEnvelope(raw, "", platform.ListModeAll)
	require.Len(t, env.Subjects, 2)
	assert.Equal(t, "image:cimg-high", env.Subjects[0].ID, "most-deployed group's image must sort first")
	assert.Equal(t, envelope.SubjectImage, env.Subjects[0].Type, "community images are flattened to individually-citable image subjects")
}

// TestImageListEnvelope_CommunityFlattensStructuredFacts is the F1 gate: every community
// version is a SubjectImage carrying the SAME discrete structured facts the platform
// path emits (real Tags []string, container bool, supported_gpu_types, image_type,
// source=community) plus group provenance (author/version/deploy_count) — so the
// central Agent can semantically pick and CITE a specific community image, not just a
// group blob. Reverting to group subjects (dropping appendStructuredImageFacts) turns
// this red.
func TestImageListEnvelope_CommunityFlattensStructuredFacts(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"ImageName": "LTX 视频生成合集", "Author": "creator-a", "CreatedCount": float64(320),
			"Data": []any{
				map[string]any{
					"CompShareImageId":  "cimg-ltx",
					"Name":              "v26.0529",
					"ImageType":         "App",
					"Status":            "Available",
					"Container":         true,
					"Description":       "文生视频/图生视频",
					"Tags":              []any{"图像生成", "视频"},
					"SupportedGpuTypes": []any{"A800"},
				},
			},
		},
	}}
	env := buildCommunityImageEnvelope(raw, "", platform.ListModeAll)
	require.Len(t, env.Subjects, 1)
	require.Equal(t, "image:cimg-ltx", env.Subjects[0].ID)
	assert.Equal(t, envelope.SubjectImage, env.Subjects[0].Type)
	assert.Equal(t, "LTX 视频生成合集", env.Subjects[0].Name, "subject name is the recognizable family name")

	facts := map[string]any{}
	for _, f := range env.Facts {
		if f.SubjectID == "image:cimg-ltx" {
			facts[f.Key] = f.Value
		}
	}
	tags, ok := facts["tags"].([]string)
	require.True(t, ok, "community tags must be a structured []string, got %T", facts["tags"])
	assert.Equal(t, []string{"图像生成", "视频"}, tags)
	gpus, ok := facts["supported_gpu_types"].([]string)
	require.True(t, ok, "supported_gpu_types must be a structured []string, got %T", facts["supported_gpu_types"])
	assert.Equal(t, []string{"A800"}, gpus)
	assert.Equal(t, "community", facts["source"])
	assert.Equal(t, "App", facts["image_type"])
	assert.Equal(t, "Available", facts["status"])
	assert.Equal(t, true, facts["container"])
	assert.Equal(t, "文生视频/图生视频", facts["description"])
	assert.Equal(t, "creator-a", facts["author"], "group author attached as per-subject provenance")
	assert.Equal(t, "v26.0529", facts["version"], "version label distinct from the family name")
	assert.Equal(t, 320, facts["deploy_count"], "group popularity attached as per-subject provenance")
}

// TestImageListEnvelope_CommunityHonestAbsence: a bare community version with no Tags
// and no Softwares emits NO tags/framework facts — honest absence, never an empty value
// the Agent could read as "matches no tag". id-less version rows are dropped, never
// given a synthetic subject.
func TestImageListEnvelope_CommunityHonestAbsence(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "Bare", "Data": []any{
			map[string]any{"CompShareImageId": "cimg-bare", "Name": "v1", "Status": "Available"},
			map[string]any{"Name": "no-id-row"}, // id-less → honest drop, no subject
		}},
	}}
	env := buildCommunityImageEnvelope(raw, "", platform.ListModeAll)
	require.Len(t, env.Subjects, 1, "id-less version rows are dropped, never given a synthetic subject")
	require.Equal(t, "image:cimg-bare", env.Subjects[0].ID)
	for _, f := range env.Facts {
		if f.SubjectID != "image:cimg-bare" {
			continue
		}
		switch f.Key {
		case "tags", "description", "framework", "framework_version", "cuda_version", "os_version", "python_version":
			t.Errorf("bare community image must emit no %q fact (honest absence), got %v", f.Key, f.Value)
		}
	}
}

// --- handler source-facet dispatch ----------------------------------------------

func TestImageListHandle_SourceFacetDispatch(t *testing.T) {
	imageSet := map[string]any{"ImageSet": []any{map[string]any{"CompShareImageId": "i1", "Name": "img"}}}
	// A real community group carries at least one version row; the envelope now flattens
	// each version into an image subject, so a zero-version group is (correctly) a
	// structured Empty read — use a realistic one-version group to exercise dispatch.
	group := map[string]any{"CompshareImageGroup": []any{map[string]any{"ImageName": "G", "CreatedCount": float64(1), "Data": []any{
		map[string]any{"CompShareImageId": "cg-1", "Name": "v1"},
	}}}}

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
