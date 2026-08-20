package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// platformCatalogRows builds a platform catalog with the ImageType mix the live
// catalog actually returns (System / App / Other).
func platformCatalogRows() map[string]any {
	row := func(id, name, imageType string, tags ...any) map[string]any {
		return map[string]any{
			"CompShareImageId": id, "Name": name, "ImageType": imageType,
			"Status": "Available", "Tags": tags,
		}
	}
	return map[string]any{"ImageSet": []any{
		row("sys-1", "Ubuntu 22.04", "System"),
		row("sys-2", "Ubuntu 20.04", "System"),
		row("app-1", "PyTorch 2.4", "App", "pytorch，Pytorch"),
		row("app-2", "TensorFlow", "App", "tensorflow，Tensorflow"),
		row("oth-1", "黑神话悟空", "Other", "黑神话，黑神话悟空"),
	}}
}

// TestTheFirstCardAsksWhatTheUserWantsToDo is the point of the reframe. The card
// used to ask "平台镜像 / 社区镜像", which requires knowing how this platform files
// its images before you can say what you want to run.
//
// The stored value is a real source choice — platform, community, or the current
// account's custom catalog — so this pins the QUESTION, not the plumbing.
func TestTheFirstCardAsksWhatTheUserWantsToDo(t *testing.T) {
	wfCtx := &Context{Params: map[string]any{}}
	form, err := buildGuidedImageSourceForm(wfCtx)
	require.NoError(t, err)

	field := form.Field("ImageSource")
	require.NotNil(t, field, "the stored key must stay ImageSource; only the question changed")

	byValue := map[string]ConfirmFormOption{}
	for _, o := range field.Options {
		byValue[o.Value] = o
	}
	require.Len(t, byValue, 3)

	assert.Equal(t, "平台镜像", byValue["platform"].Label)
	assert.Equal(t, "社区镜像", byValue["community"].Label)
	assert.Equal(t, "自制镜像", byValue["custom"].Label)
	for _, v := range []string{"platform", "community", "custom"} {
		assert.NotEmpty(t, byValue[v].Note,
			"%s must still say which catalog it reads, so the reframe informs rather than hides", v)
	}
	assert.NotContains(t, form.Step.Title, "来源",
		"the title must not ask where images are stored")
}

// TestTheFirstCardDoesNotMoveAnyoneToADifferentCatalog — reframing the question must
// not silently change which catalog an unspecified create reads. platform stays the
// default and stays first.
func TestTheFirstCardDoesNotMoveAnyoneToADifferentCatalog(t *testing.T) {
	form, err := buildGuidedImageSourceForm(&Context{Params: map[string]any{}})
	require.NoError(t, err)
	field := form.Field("ImageSource")
	require.NotNil(t, field)

	assert.Equal(t, "platform", field.Value, "the default catalog is unchanged")
	require.NotEmpty(t, field.Options)
	assert.Equal(t, "platform", field.Options[0].Value,
		"the pre-selected option must be the one shown first")

	// An explicit community create still shows community selected.
	form, err = buildGuidedImageSourceForm(&Context{Params: map[string]any{"ImageSource": "community"}})
	require.NoError(t, err)
	assert.Equal(t, "community", form.Field("ImageSource").Value)

	form, err = buildGuidedImageSourceForm(&Context{Params: map[string]any{"ImageSource": "custom"}})
	require.NoError(t, err)
	assert.Equal(t, "custom", form.Field("ImageSource").Value)
}

// TestEachBranchGetsTheFilterThatFitsItsData is the structural claim behind the
// reframe: no branch-specific code decides which filter to show. Community rows all
// carry ImageType=Community so the type facet omits itself for lack of a choice, and
// platform tags barely intersect the 用途 classification so the category facet omits
// itself the same way.
func TestEachBranchGetsTheFilterThatFitsItsData(t *testing.T) {
	t.Run("自己搭环境 gets 底座 types", func(t *testing.T) {
		wfCtx := &Context{
			Params: map[string]any{"ImageSource": "platform"},
			StepResults: map[string]map[string]any{
				"查询镜像":   platformCatalogRows(),
				"查询镜像分类": categoryTestTaxonomy(),
			},
		}
		form, err := buildGuidedImageFacetsForm(wfCtx)
		require.NoError(t, err)

		require.NotNil(t, form.Field("ImageType"), "platform images differ by type, so the type facet earns its place")
		assert.Nil(t, form.Field("ImageCategory"),
			"platform tags are framework names, not 用途 categories; offering 用途 here would be near-empty")
		assert.Contains(t, form.Step.Title, "底座")
	})

	t.Run("跑现成的应用 gets 用途 categories", func(t *testing.T) {
		wfCtx := categoryWfCtx(t, map[string]any{"ImageSource": "community"})
		form, err := buildGuidedImageFacetsForm(wfCtx)
		require.NoError(t, err)

		require.NotNil(t, form.Field("ImageCategory"))
		assert.Nil(t, form.Field("ImageType"),
			"every community row is ImageType=Community — a one-value facet is no choice")
		assert.Contains(t, form.Step.Title, "哪一类")
	})
}

// TestTheTypeFacetNamesEveryTypeTheCatalogReturns covers a real gap: the live
// platform catalog returns System(9), App(52) AND Other(11), and "Other" used to
// render as the bare English word beside Chinese labels. Counts are asserted too —
// the 用途 facet carries them, and one branch silently lacking them reads as
// unfinished rather than as a different kind of filter.
func TestTheTypeFacetNamesEveryTypeTheCatalogReturns(t *testing.T) {
	wfCtx := &Context{
		Params:      map[string]any{"ImageSource": "platform"},
		StepResults: map[string]map[string]any{"查询镜像": platformCatalogRows()},
	}
	opts := imageTypeFacetOptions(createImageCandidates(wfCtx))
	require.NotEmpty(t, opts)

	byValue := map[string]ConfirmFormOption{}
	for _, o := range opts {
		byValue[o.Value] = o
	}
	assert.Equal(t, "系统镜像", byValue["System"].Label)
	assert.Equal(t, "框架 / 应用镜像", byValue["App"].Label)
	assert.Equal(t, "其他镜像", byValue["Other"].Label)
	assert.Equal(t, "2 个镜像", byValue["System"].Note)
	assert.Equal(t, "1 个镜像", byValue["Other"].Note)

	for _, o := range opts {
		if o.Value == "" {
			continue
		}
		assert.False(t, strings.EqualFold(o.Label, o.Value),
			"%q rendered as the raw upstream enum value", o.Value)
	}
}

// TestAnUnknownTypeIsShownVerbatim — a value we cannot name is shown as the platform
// sent it, never guessed at or hidden.
func TestAnUnknownTypeIsShownVerbatim(t *testing.T) {
	assert.Equal(t, "SomethingNew", imageTypeFacetLabel("SomethingNew"))
}
