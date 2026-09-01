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

func TestImageListDescriptionUsesTheLiveCatalogForNamedModelDeployment(t *testing.T) {
	description := NewReadCapability(imageListReadSpec()).Tool.Function.Description
	for _, want := range []string{
		"用于浏览、推荐和创建前选型",
		"部署或运行具名模型/应用时先查社区镜像",
		"有精确候选就据此回答",
		"目录不能替代登录、默认配置、使用步骤或故障文档",
		"源码、权重或 adapter 问题",
	} {
		require.Contains(t, description, want)
	}
}

func TestImageListSemanticQueriesPreferCommunityForNamedModelDeployment(t *testing.T) {
	schema, ok := NewReadCapability(imageListReadSpec()).Tool.Function.Parameters.(map[string]any)
	require.True(t, ok)
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	semanticQueries, ok := properties["semantic_queries"].(map[string]any)
	require.True(t, ok)
	description, ok := semanticQueries["description"].(string)
	require.True(t, ok)
	require.Contains(t, description, "先查 community")
	require.Contains(t, description, "未命中或用户要求基础环境时再查 platform")
}

type communityQueryExec struct {
	results map[string]map[string]any
	calls   []string
}

func (e *communityQueryExec) Execute(_ context.Context, _ string, args map[string]any) (map[string]any, error) {
	query, _ := args["FuzzySearch"].(string)
	e.calls = append(e.calls, query)
	return e.results[query], nil
}

func (e *communityQueryExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func TestCommunitySemanticQueriesUnionWithGroundedUserQuery(t *testing.T) {
	group := func(name, id string, deploys int) map[string]any {
		return map[string]any{
			"ImageName": name, "CreatedCount": float64(deploys),
			"Data": []any{map[string]any{"CompShareImageId": id, "Name": name + " v1"}},
		}
	}
	exec := &communityQueryExec{results: map[string]map[string]any{
		"数字人": {
			"CompshareImageGroup": []any{
				group("InfiniteTalk", "infinite-1", 17710),
				group("HeyGem", "heygem-1", 1076),
			},
		},
		"LiveTalking": {
			"CompshareImageGroup": []any{
				group("LiveTalking", "live-1", 4599),
			},
		},
	}}

	result := runImageList(t, exec, ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "数字人",
		SemanticQueries: []string{"LiveTalking"}, Mode: platform.ListModeFiltered,
	})

	require.Equal(t, []string{"数字人", "LiveTalking"}, exec.calls)
	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.NotNil(t, result.Envelope)
	names := make([]string, 0, len(result.Envelope.Subjects))
	for _, subject := range result.Envelope.Subjects {
		names = append(names, subject.Name)
	}
	require.Contains(t, names, "InfiniteTalk")
	require.Contains(t, names, "LiveTalking")
	require.Equal(t, "InfiniteTalk", names[0],
		"the guessed expansion cannot exclude the more popular candidate found by the user's own purpose")
}

func TestCommunitySemanticQueriesFilterAndDedupeFlatFallback(t *testing.T) {
	exec := &communityQueryExec{results: map[string]map[string]any{
		"数字人": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-infinite", "Name": "数字人 InfiniteTalk"},
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu"},
			},
		},
		"LiveTalking": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-live", "Name": "LiveTalking"},
				map[string]any{"CompShareImageId": "img-infinite", "Name": "数字人 InfiniteTalk"},
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu"},
			},
		},
	}}

	result := runImageList(t, exec, ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "数字人",
		SemanticQueries: []string{"LiveTalking"}, Mode: platform.ListModeFiltered,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.NotNil(t, result.Envelope)
	names := make([]string, 0, len(result.Envelope.Subjects))
	for _, subject := range result.Envelope.Subjects {
		names = append(names, subject.Name)
	}
	require.Equal(t, []string{"数字人 InfiniteTalk", "LiveTalking"}, names)
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

	byID := renderImageListReply(raw, "ImageSet", fieldOrder, "img-2", platform.ListModeFiltered)
	assert.Contains(t, byID, "PyTorch 2.1", "a create workflow's returned image id must support a follow-up lookup")
	assert.NotContains(t, byID, "Ubuntu")
	assert.NotContains(t, byID, "img-2", "matching by id must not leak the raw id into the clean display")
	byIDEnvelope := buildImageListEnvelope(raw, "ImageSet", fieldOrder, "img-2", platform.ListModeFiltered,
		"DescribeCompShareImages", "platform")
	require.Len(t, byIDEnvelope.Subjects, 1)
	assert.Equal(t, "image:img-2", byIDEnvelope.Subjects[0].ID)

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

// digitalHumanCommunityCatalog mirrors the live shape of a 数字人 community search
// (measured 2026-07-28): a runaway top family with many versions, a second
// multi-version family, and a tail of one-version families. Every version of a
// family carries the SAME name, which is what makes a depth-first fill useless
// as evidence — the Agent gets ten rows it cannot tell apart.
func digitalHumanCommunityCatalog() map[string]any {
	family := func(name string, created int, versions int) map[string]any {
		data := make([]any, 0, versions)
		for i := 0; i < versions; i++ {
			data = append(data, map[string]any{
				"CompShareImageId": fmt.Sprintf("cimg-%s-%d", name, i),
				"Name":             name,
				"VersionName":      fmt.Sprintf("v%d", versions-i),
			})
		}
		return map[string]any{"ImageName": name, "Author": "author-" + name, "CreatedCount": float64(created), "Data": data}
	}
	return map[string]any{"CompshareImageGroup": []any{
		family("InfiniteTalk", 17667, 7),
		family("LTX", 5021, 6),
		family("HeyGem", 1076, 1),
		family("LongCatAvatar", 390, 1),
		family("Fay", 251, 2),
	}}
}

func subjectNames(env envelope.Envelope) []string {
	out := make([]string, 0, len(env.Subjects))
	for _, s := range env.Subjects {
		out = append(out, s.Name)
	}
	return out
}

// The recommendation defect this fixes: the browse listing's Reply never reaches
// the model (buildReadObservation drops it for a browse listing), so these ten
// subjects ARE the evidence a "推荐一个做数字人的镜像" turn reasons over. Spending
// them depth-first showed two families out of five — the Agent could not have
// recommended HeyGem because it was never told HeyGem exists. Breadth first: one
// version per family before any family gets a second.
func TestImageListEnvelope_CommunityCapBuysDistinctFamiliesNotDuplicateVersions(t *testing.T) {
	env := buildCommunityImageEnvelope(digitalHumanCommunityCatalog(), "数字人", platform.ListMode(""))

	require.Len(t, env.Subjects, imageModelBrowseDisplayCap)
	assert.Equal(t,
		[]string{"InfiniteTalk", "LTX", "HeyGem", "LongCatAvatar", "Fay"},
		subjectNames(env)[:5],
		"the cap first buys one version of every family, in 部署次数 order")

	distinct := map[string]bool{}
	for _, name := range subjectNames(env) {
		distinct[name] = true
	}
	assert.Len(t, distinct, 5, "all five families reach the Agent; depth-first showed only two")
	assert.Contains(t, distinct, "HeyGem", "a one-version family must not be starved by a seven-version one")
}

// The other half of the same rule: when the search already narrowed to one
// family, the cap must still spend itself on that family's versions rather than
// reporting a single row. Breadth-first must not become breadth-only.
func TestImageListEnvelope_CommunityStillShowsVersionsWhenTheFieldIsNarrow(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "LiveTalking", "CreatedCount": float64(4596), "Data": []any{
			map[string]any{"CompShareImageId": "cimg-lt-22", "Name": "LiveTalking", "VersionName": "v2.2"},
			map[string]any{"CompShareImageId": "cimg-lt-21", "Name": "LiveTalking", "VersionName": "v2.1"},
			map[string]any{"CompShareImageId": "cimg-lt-20", "Name": "LiveTalking", "VersionName": "v2.0"},
		}},
	}}

	env := buildCommunityImageEnvelope(raw, "LiveTalking", platform.ListMode(""))

	require.Len(t, env.Subjects, 3, "a single-family search still surfaces every version")
	assert.Equal(t,
		[]string{"image:cimg-lt-22", "image:cimg-lt-21", "image:cimg-lt-20"},
		[]string{env.Subjects[0].ID, env.Subjects[1].ID, env.Subjects[2].ID},
		"catalog version order is preserved within a family")
}

// The truncation note is the Agent's only signal about what it did NOT see. For a
// recommendation the missing figure is coverage of the FIELD, not of the rows.
func TestImageListEnvelope_CommunityTruncationNoteReportsFamilyCoverage(t *testing.T) {
	env := buildCommunityImageEnvelope(digitalHumanCommunityCatalog(), "数字人", platform.ListMode(""))

	note := ""
	for _, c := range env.Computed {
		if c.Key == "display_truncated" {
			note = fmt.Sprint(c.Value)
		}
	}
	require.NotEmpty(t, note, "17 images do not fit in 10 subjects; the Agent must be told")
	assert.Contains(t, note, "showing 10 of 17 community images")
	assert.Contains(t, note, "across 5 of 5 image families")
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
