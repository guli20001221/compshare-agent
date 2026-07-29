package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/platform"
)

// platformCatalogExec serves a slice of the REAL platform catalog, read live from
// the account on 2026-07-29, in the shape upstream actually returns.
//
// The row shape is measured, not assumed: DescribeCompShareImages rows carry
// `Name` and NOT `ImageName` (verified against all 40 live rows). A fixture that
// invented ImageName would still pass — imageQueryMatchFields accepts both — while
// testing a key production never sees, so the filter could break on the real key
// with every test green.
//
// The names matter more than the count: the platform's inference images are named
// after the runtime, never after the job, which is the whole reason a 用途 query
// finds none of them.
type platformCatalogExec struct{ calls int }

func (e *platformCatalogExec) Execute(_ context.Context, _ string, _ map[string]any) (map[string]any, error) {
	e.calls++
	rows := []any{}
	for _, name := range []string{
		"vLLM v0.25.1", "vLLM v0.12.0", "SGLang v0.5.15", "Ollama v0.32.1",
		"Ubuntu 22.04 64位", "Windows 2022 64位", "ComfyUI基础镜像0.3.75",
		"cuda130_torch291_py312", "Dify Ubuntu 22.04",
	} {
		rows = append(rows, map[string]any{
			"CompShareImageId": "img-" + name, "Name": name, "ImageType": "App",
		})
	}
	return map[string]any{"ImageSet": rows}, nil
}

func (e *platformCatalogExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func envelopeSubjectNames(t *testing.T, res ReadResult) []string {
	t.Helper()
	if res.Envelope == nil {
		return nil
	}
	out := make([]string, 0, len(res.Envelope.Subjects))
	for _, s := range res.Envelope.Subjects {
		out = append(out, s.Name)
	}
	return out
}

// TestPlatformSemanticExpansionSurfacesRuntimeNamedImages is the recommendation
// bug, at the layer that caused it. 「推荐一个做大模型推理的镜像」 matched ZERO platform
// images because none of them contains 大模型推理 in its name — so the Agent, asked
// to recommend, could only answer from the catalog that DID return candidates
// (community, which expands and ranks by deploy count). The platform's own
// vLLM / SGLang / Ollama images were never mentioned, on any run.
//
// The baseline arm is what makes this non-vacuous: the same query without the
// expansion must still find nothing, or the expansion is not what recovered them.
func TestPlatformSemanticExpansionSurfacesRuntimeNamedImages(t *testing.T) {
	exec := &platformCatalogExec{}

	baseline := runImageList(t, exec, ImageListRequest{
		Source: platform.ImageSourcePlatform, Query: "大模型推理", Mode: platform.ListModeFiltered,
	})
	assert.Empty(t, envelopeSubjectNames(t, baseline),
		"premise: the user's own words match no platform image name")

	expanded := runImageList(t, exec, ImageListRequest{
		Source: platform.ImageSourcePlatform, Query: "大模型推理",
		SemanticQueries: []string{"vLLM", "SGLang", "Ollama"}, Mode: platform.ListModeFiltered,
	})
	names := envelopeSubjectNames(t, expanded)
	assert.Contains(t, names, "vLLM v0.25.1")
	assert.Contains(t, names, "SGLang v0.5.15")
	assert.Contains(t, names, "Ollama v0.32.1")
	assert.NotContains(t, names, "Windows 2022 64位",
		"the expansion widens the match; it does not drop the filter")
}

// The expansion is a UNION over the catalog already fetched: it must never narrow
// to the expansion terms and lose what the user's own words found, and it must not
// cost a second upstream call (unlike community, the platform listing is fetched
// whole and filtered here).
func TestPlatformSemanticExpansionUnionsAndCostsNoExtraCall(t *testing.T) {
	exec := &platformCatalogExec{}

	res := runImageList(t, exec, ImageListRequest{
		Source: platform.ImageSourcePlatform, Query: "Ubuntu",
		SemanticQueries: []string{"vLLM"}, Mode: platform.ListModeFiltered,
	})

	names := envelopeSubjectNames(t, res)
	assert.Contains(t, names, "Ubuntu 22.04 64位", "the user's own query must survive the expansion")
	assert.Contains(t, names, "vLLM v0.25.1", "…alongside what the expansion added")
	assert.NotContains(t, names, "SGLang v0.5.15", "nothing unmatched is admitted")
	assert.Equal(t, 1, exec.calls, "the platform catalog is fetched once and filtered locally")
}

// A single query keeps the pre-existing path byte-for-byte: the expansion branch
// must not change how an ordinary filtered browse behaves.
func TestPlatformSingleQueryPathIsUnchanged(t *testing.T) {
	exec := &platformCatalogExec{}

	res := runImageList(t, exec, ImageListRequest{
		Source: platform.ImageSourcePlatform, Query: "Ubuntu", Mode: platform.ListModeFiltered,
	})

	names := envelopeSubjectNames(t, res)
	require.NotEmpty(t, names)
	assert.Contains(t, names, "Ubuntu 22.04 64位")
	assert.Contains(t, names, "Dify Ubuntu 22.04")
	assert.NotContains(t, names, "vLLM v0.25.1")
}
