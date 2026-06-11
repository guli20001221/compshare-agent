package intent

import (
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stockAPIResponse(entries ...map[string]any) map[string]any {
	items := make([]any, len(entries))
	for i, e := range entries {
		items[i] = e
	}
	return map[string]any{"AvailableInstanceTypes": items}
}

func stockEntry(name, status string) map[string]any {
	return map[string]any{"Name": name, "Status": status}
}

func TestBuildStockEnvelope_Basic(t *testing.T) {
	raw := stockAPIResponse(
		stockEntry("4090", "Normal"),
		stockEntry("H20", "SoldOut"),
	)
	env := buildStockEnvelope(raw, "有什么GPU")
	assert.Equal(t, envelope.KindStockAvailability, env.Kind)
	assert.Len(t, env.Subjects, 2)
	assert.Equal(t, "stock:4090", env.Subjects[0].ID)
	assert.Equal(t, "stock:H20", env.Subjects[1].ID)
	assert.Equal(t, envelope.SubjectGPUModel, env.Subjects[0].Type)

	statusFacts := filterFacts(env.Facts, "status")
	assert.Len(t, statusFacts, 2)
	assert.Equal(t, "Normal", statusFacts[0].Value)
	assert.Equal(t, "SoldOut", statusFacts[1].Value)

	assert.NotEmpty(t, computedValue(env, "disclaimer"))
}

func TestBuildStockEnvelope_Filtered(t *testing.T) {
	raw := stockAPIResponse(
		stockEntry("4090", "Normal"),
		stockEntry("H20", "Normal"),
		stockEntry("A100", "SoldOut"),
	)
	env := buildStockEnvelope(raw, "4090 有货吗")
	assert.Len(t, env.Subjects, 1)
	assert.Equal(t, "stock:4090", env.Subjects[0].ID)
}

func TestBuildStockEnvelope_UnavailableGPU(t *testing.T) {
	raw := stockAPIResponse(stockEntry("4090", "Normal"))
	env := buildStockEnvelope(raw, "H100 有货吗")
	assert.Len(t, env.Subjects, 1, "all models shown when unavailable GPU asked")
	assert.Equal(t, "H100", computedValue(env, "unavailable_models"))
}

func TestBuildStockEnvelope_NoMatchHint(t *testing.T) {
	raw := stockAPIResponse(stockEntry("4090", "Normal"))
	env := buildStockEnvelope(raw, "XYZ99 有货吗")
	hint := computedValue(env, "no_match_hint")
	assert.NotEmpty(t, hint, "should have no_match_hint when user mentioned unknown GPU")
}

func TestBuildStockEnvelope_EmptyItems(t *testing.T) {
	env := buildStockEnvelope(map[string]any{}, "有什么GPU")
	assert.Len(t, env.Subjects, 0)
}

func TestBuildImageListEnvelope_Platform(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId":   "img-001",
			"CompShareImageName": "Ubuntu 22.04 CUDA",
			"ImageType":          "System",
			"Name":               "Ubuntu-nvidia 22.04",
		},
		map[string]any{
			"CompShareImageId": "img-002",
			"Name":             "Windows 2022",
			"ImageType":        "System",
		},
	}}
	fields := []string{"CompShareImageId", "CompShareImageName", "ImageType", "Name"}
	env := buildImageListEnvelope(raw, "ImageSet", fields, "平台有什么镜像", "DescribeCompShareImages", "platform")

	assert.Equal(t, envelope.KindImageList, env.Kind)
	assert.Len(t, env.Subjects, 2)
	assert.Equal(t, envelope.SubjectImage, env.Subjects[0].Type)
	assert.Equal(t, "image:img-001", env.Subjects[0].ID)
	assert.Equal(t, "Ubuntu-nvidia 22.04", env.Subjects[0].Name)
	assert.Equal(t, "platform", computedValue(env, "image_category"))
}

func TestBuildImageListEnvelope_KeywordFilter(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04"},
		map[string]any{"CompShareImageId": "img-002", "Name": "Windows 2022"},
	}}
	fields := []string{"CompShareImageId", "Name"}
	env := buildImageListEnvelope(raw, "ImageSet", fields, "ubuntu 镜像", "DescribeCompShareImages", "platform")
	assert.Len(t, env.Subjects, 1)
	assert.Equal(t, "Ubuntu 22.04", env.Subjects[0].Name)
}

func TestBuildImageListEnvelope_NoMatch(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04"},
	}}
	fields := []string{"CompShareImageId", "Name"}
	env := buildImageListEnvelope(raw, "ImageSet", fields, "rocky 镜像", "DescribeCompShareImages", "platform")
	assert.Len(t, env.Subjects, 0, "no match should produce empty subjects")
}

func TestBuildCommunityImageEnvelope_ImageNameField(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"ImageName": "Stable Diffusion WebUI",
			"Author":    "community",
			"Data": []any{
				map[string]any{"CompShareImageId": "cimg-001", "Name": "SD v1.9"},
			},
		},
	}}
	env := buildCommunityImageEnvelope(raw, "有哪些社区镜像")
	require.Len(t, env.Subjects, 1)
	assert.Equal(t, "Stable Diffusion WebUI", env.Subjects[0].Name)
	assert.Equal(t, envelope.SubjectImageGroup, env.Subjects[0].Type)

	nameFacts := filterFacts(env.Facts, "group_name")
	require.Len(t, nameFacts, 1)
	assert.Equal(t, "Stable Diffusion WebUI", nameFacts[0].Value)
}

func TestBuildCommunityImageEnvelope_NameFallback(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"Name":   "OldStyleGroup",
			"Author": "dev",
			"Data":   []any{map[string]any{"CompShareImageId": "cimg-x", "Name": "v1"}},
		},
	}}
	env := buildCommunityImageEnvelope(raw, "社区镜像")
	require.Len(t, env.Subjects, 1)
	assert.Equal(t, "OldStyleGroup", env.Subjects[0].Name)
}

func TestBuildCommunityImageEnvelope_LineBudgetCountsVersions(t *testing.T) {
	groups := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		versions := make([]any, 0, 5)
		for j := 0; j < 5; j++ {
			versions = append(versions, map[string]any{
				"CompShareImageId": "cimg",
				"Name":             "ver",
			})
		}
		groups = append(groups, map[string]any{
			"ImageName": "Group",
			"Data":      versions,
		})
	}
	raw := map[string]any{"CompshareImageGroup": groups}
	env := buildCommunityImageEnvelope(raw, "社区镜像")

	subjectCount := len(env.Subjects)
	versionFactCount := 0
	for _, f := range env.Facts {
		if len(f.Key) > 8 && f.Key[:8] == "version_" && f.Key != "version_count" {
			versionFactCount++
		}
	}
	totalLines := subjectCount + versionFactCount
	assert.LessOrEqual(t, totalLines, communityImageGroupLimit,
		"total group headers + version lines must respect lineBudget")
}

func TestBuildCommunityImageEnvelope_TruncationHint(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"ImageName": "BigGroup",
			"Data": []any{
				map[string]any{"CompShareImageId": "a", "Name": "v1"},
				map[string]any{"CompShareImageId": "b", "Name": "v2"},
				map[string]any{"CompShareImageId": "c", "Name": "v3"},
				map[string]any{"CompShareImageId": "d", "Name": "v4"},
				map[string]any{"CompShareImageId": "e", "Name": "v5"},
			},
		},
	}}
	env := buildCommunityImageEnvelope(raw, "社区镜像")
	truncated := filterFacts(env.Facts, "versions_truncated")
	require.Len(t, truncated, 1)
	assert.Contains(t, truncated[0].Value, "共 5 个版本")
}

func TestSetEnvelopeIfPopulated_Empty(t *testing.T) {
	result := HandledResult("fallback")
	env := envelope.Envelope{Subjects: []envelope.Subject{}}
	setEnvelopeIfPopulated(&result, env)
	assert.Nil(t, result.Envelope, "should not set envelope when subjects empty")
}

func TestSetEnvelopeIfPopulated_HasSubjects(t *testing.T) {
	result := HandledResult("fallback")
	env := envelope.Envelope{Subjects: []envelope.Subject{
		{ID: "test:1", Type: envelope.SubjectImage},
	}}
	setEnvelopeIfPopulated(&result, env)
	assert.NotNil(t, result.Envelope)
	assert.Len(t, result.RendererInputEnvelopeHashes, 1)
}

// --- helpers ---

func filterFacts(facts []envelope.Fact, key string) []envelope.Fact {
	out := []envelope.Fact{}
	for _, f := range facts {
		if f.Key == key {
			out = append(out, f)
		}
	}
	return out
}

func computedValue(env envelope.Envelope, key string) string {
	for _, f := range env.Computed {
		if f.Key == key {
			switch v := f.Value.(type) {
			case string:
				return v
			default:
				return ""
			}
		}
	}
	return ""
}
