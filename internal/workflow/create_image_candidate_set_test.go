package workflow

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// platformCatalogLargerThanOnePage mirrors the shape of the live platform catalog
// (measured 2026-07-22: TotalCount=72, System images carry no tags, App images
// carry framework names) at a size that exceeds maxGuidedImageOptions, which is
// the only condition under which the count/page disagreement is observable.
//
// It deliberately also carries OFFLINE rows. They are what make the two candidate
// computations distinguishable at all: a count taken over the raw catalog includes
// them while the picker's hard gates have already dropped them, so a fixture of
// uniformly-available rows would let the old double computation pass unnoticed.
func platformCatalogLargerThanOnePage() map[string]any {
	tags := []string{"pytorch", "tensorflow", "comfyUI", "miniconda", "Miniconda3"}
	var set []any
	for i := 0; i < 9; i++ {
		set = append(set, map[string]any{
			"CompShareImageId": fmt.Sprintf("sys-%d", i),
			"Name":             fmt.Sprintf("Ubuntu 22.04 #%d", i),
			"ImageType":        "System", "Status": "Available",
		})
	}
	for i := 0; i < 55; i++ {
		set = append(set, map[string]any{
			"CompShareImageId": fmt.Sprintf("app-%d", i),
			"Name":             fmt.Sprintf("App image #%d", i),
			"ImageType":        "App", "Status": "Available",
			"Tags": []any{tags[i%len(tags)]},
		})
	}
	for i := 0; i < 3; i++ {
		set = append(set, map[string]any{
			"CompShareImageId": fmt.Sprintf("app-offline-%d", i),
			"Name":             fmt.Sprintf("Retired app image #%d", i),
			"ImageType":        "App", "Status": "Offline",
			"Tags": []any{"pytorch"},
		})
	}
	for i := 0; i < 2; i++ {
		set = append(set, map[string]any{
			"CompShareImageId": fmt.Sprintf("sys-offline-%d", i),
			"Name":             fmt.Sprintf("Retired system image #%d", i),
			"ImageType":        "System", "Status": "Offline",
		})
	}
	return map[string]any{"ImageSet": set}
}

func candidateWfCtx(params map[string]any) *Context {
	params["ImageSource"] = "platform"
	return &Context{
		Params:      params,
		StepResults: map[string]map[string]any{"查询镜像": platformCatalogLargerThanOnePage()},
	}
}

// TestTheFacetCountAndThePickerPopulationAreTheSameNumber is the invariant the
// whole imageCandidateSet exists to hold.
//
// They used to be two independent computations: the facet card counted the raw
// catalog while the picker ran the ranker's hard gates first and only then applied
// the facets. The card therefore stated "框架 / 应用镜像 55 个镜像" against a picker
// that had already dropped rows for reasons the count never saw. Asserting the two
// numbers are equal — rather than asserting either number — is what makes a future
// second producer fail here instead of in front of a user.
func TestTheFacetCountAndThePickerPopulationAreTheSameNumber(t *testing.T) {
	wfCtx := candidateWfCtx(map[string]any{})
	set := createImageCandidates(wfCtx)
	require.Less(t, len(set.base), len(set.snap.Entries()),
		"premise: the hard gates drop rows, so counting the raw catalog is a DIFFERENT number")

	facet := imageTypeFacetOptions(set)
	require.NotEmpty(t, facet)

	for _, opt := range facet {
		if opt.Value == "" {
			continue // the 全部类型 sentinel carries no count
		}
		picked := candidateWfCtx(map[string]any{"ImageType": opt.Value})
		_, opts, population := guidedImageFormOptions(picked.Params, picked.Result("查询镜像"), "", nil, false)
		require.NotEmpty(t, opts, "type %q was offered, so it must reach a picker", opt.Value)
		assert.Equal(t, fmt.Sprintf("%d 个镜像", population), opt.Note,
			"the count on the facet card and the picker's population are one fact")
	}
}

// TestOnAPodZoneTheCountDropsVmOnlyImagesToo is the live 华北一C case. A user who
// names the zone up front ("为我创建一台华北一C的4090") reaches the image cards with
// the pod flag already set, and a pod zone can only boot container images. The old
// count never saw that gate — it counted VM-only rows the picker had already
// dropped — so the card advertised images that zone could not run.
func TestOnAPodZoneTheCountDropsVmOnlyImagesToo(t *testing.T) {
	catalog := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-ctr", "Name": "PyTorch", "ImageType": "App", "Status": "Available", "Container": "True"},
		map[string]any{"CompShareImageId": "img-vm-1", "Name": "Windows Server", "ImageType": "App", "Status": "Available", "Container": "False"},
		map[string]any{"CompShareImageId": "img-vm-2", "Name": "Windows Server 2019", "ImageType": "App", "Status": "Available", "Container": "False"},
		map[string]any{"CompShareImageId": "img-sys", "Name": "Ubuntu 22.04", "ImageType": "System", "Status": "Available", "Container": "True"},
	}}
	appCount := func(params map[string]any) string {
		params["ImageSource"] = "platform"
		wfCtx := &Context{Params: params, StepResults: map[string]map[string]any{"查询镜像": catalog}}
		for _, o := range imageTypeFacetOptions(createImageCandidates(wfCtx)) {
			if o.Value == "App" {
				return o.Note
			}
		}
		return ""
	}

	require.Equal(t, "3 个镜像", appCount(map[string]any{}),
		"premise: off a pod zone all three App rows are real candidates")
	assert.Equal(t, "1 个镜像", appCount(map[string]any{"Zone": "cn-bj2-03", "ZoneIsPod": true}),
		"a pod zone cannot boot a VM image; the count must not offer what the picker drops")

	// zoneIsPod is now an explicit argument, not read from the params map — that is
	// the fix for the pinned-pod-zone bug, where the param was absent at picker time.
	podParams := map[string]any{"ImageSource": "platform", "Zone": "cn-bj2-03", "ImageType": "App"}
	_, opts, population := guidedImageFormOptions(podParams, catalog, "", nil, true)
	assert.Equal(t, []string{"img-ctr"}, optionValues(&ConfirmFormField{Options: opts}))
	assert.Equal(t, 1, population)
}

// TestThePickerStatesWhatItIsNotShowing covers the half a shared candidate set does
// not fix by itself. 55 really is the honest population and the card really can only
// list maxGuidedImageOptions of them, so both available silences are wrong: saying
// "10" hides real candidates, and listing all 55 makes the card unusable. The card
// must state both numbers and name a way forward.
func TestThePickerStatesWhatItIsNotShowing(t *testing.T) {
	wfCtx := candidateWfCtx(map[string]any{"ImageType": "App"})
	form, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)

	shown := len(fieldByKey(t, form, "ImageId").Options)
	require.Equal(t, maxGuidedImageOptions, shown, "premise: this catalog overflows one page")

	assert.Contains(t, form.Step.Description, fmt.Sprintf("当前展示 %d 个", shown))
	assert.Contains(t, form.Step.Description, "共 55 个匹配镜像")
}

// TestAPageThatShowsEverythingClaimsNoRemainder is the other direction: the card
// must not announce a remainder that does not exist. A count-and-page banner on a
// list that IS complete reads as "there is more" and sends the user back to narrow
// something that is already narrow.
func TestAPageThatShowsEverythingClaimsNoRemainder(t *testing.T) {
	wfCtx := &Context{
		Params: map[string]any{"ImageSource": "platform"},
		StepResults: map[string]map[string]any{"查询镜像": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-a", "Name": "PyTorch", "ImageType": "App", "Status": "Available"},
			map[string]any{"CompShareImageId": "img-b", "Name": "TensorFlow", "ImageType": "App", "Status": "Available"},
		}}},
	}
	form, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)
	assert.NotContains(t, form.Step.Description, "共")
	assert.NotContains(t, form.Step.Description, "当前展示")
}

// TestTheTagCardActuallyRunsInTheGuidedSequence proves the split is reachable, not
// merely correct in isolation. A new Step that every skip predicate declines is a
// card no user ever sees, and the existing end-to-end fixture cannot catch that:
// its images declare no distinct type or tag, so BOTH facet cards skip there.
//
// This drives the real CreateInstanceGuidedDef with a catalog that has two types
// and real tags, and asserts the type card and the tag card arrive as separate,
// consecutive cards — one narrowing question each.
func TestTheTagCardActuallyRunsInTheGuidedSequence(t *testing.T) {
	executor := formMockExecutor()
	executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-sys", "Name": "Ubuntu 22.04", "ImageType": "System",
			"Status": "Available", "Container": "True"},
		map[string]any{"CompShareImageId": "img-torch", "Name": "PyTorch 2.4", "ImageType": "App",
			"Status": "Available", "Container": "True", "Tags": []any{"pytorch"}},
		map[string]any{"CompShareImageId": "img-comfy", "Name": "ComfyUI", "ImageType": "App",
			"Status": "Available", "Container": "True", "Tags": []any{"comfyUI"}},
	}}

	var cards []string
	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		require.NotNil(t, form)
		switch {
		case form.Field("ImageSource") != nil:
			cards = append(cards, "source")
		case form.Field("ImageType") != nil:
			cards = append(cards, "type")
			assert.Nil(t, form.Field("ImageTag"), "the type card must not also carry the tag question")
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"ImageType": "App"}}
		case form.Field("ImageTag") != nil:
			cards = append(cards, "tag")
			// Only App tags are offered — the System row contributed none, so the
			// pair that used to dead-end cannot be assembled here.
			assert.Equal(t, []string{"", "pytorch", "comfyUI"}, optionValues(fieldByKey(t, form, "ImageTag")))
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"ImageTag": "comfyUI"}}
		// The final card also states ImageId, but read-only and with no options; the
		// picker is the editable one.
		case form.Field("ImageId") != nil && form.Field("ImageId").Editable:
			cards = append(cards, "image")
			assert.Equal(t, []string{"img-comfy"}, optionValues(fieldByKey(t, form, "ImageId")),
				"the picker offers exactly what the two cards narrowed to")
		}
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, []string{"source", "type", "tag", "image"}, cards,
		"the tag question is its own card, between the type card and the picker")
}

// TestEveryGuidedCardIsNumberedInOneScheme guards the ordinal table against the
// wizard outgrowing it. The full platform run is now ten cards; a table that
// stopped at five put "第五步" and "第6步" in the same sequence.
func TestEveryGuidedCardIsNumberedInOneScheme(t *testing.T) {
	for i := 1; i <= 10; i++ {
		assert.NotContains(t, guidedOrdinal(i), fmt.Sprint(i),
			"card %d fell through to the digit fallback", i)
	}
}

// TestEveryTagOfferedReachesAnImage is the general form of the 系统镜像 + 标签 dead
// end: whatever pair of type and tag the two cards let a user assemble, the picker
// that follows must have something in it. The cards are the only place these values
// come from, so walking their own options is the complete set of reachable pairs.
func TestEveryTagOfferedReachesAnImage(t *testing.T) {
	types := imageTypeFacetOptions(createImageCandidates(candidateWfCtx(map[string]any{})))
	require.NotEmpty(t, types)

	pairs := 0
	for _, typeOpt := range types {
		afterType := candidateWfCtx(map[string]any{"ImageType": typeOpt.Value})
		skip, err := shouldSkipGuidedImageTagStep(afterType)
		require.NoError(t, err)
		if skip {
			continue
		}
		for _, tagOpt := range imageTagFacetOptions(createImageCandidates(afterType)) {
			pairs++
			picked := candidateWfCtx(map[string]any{"ImageType": typeOpt.Value, "ImageTag": tagOpt.Value})
			_, opts, _ := guidedImageFormOptions(picked.Params, picked.Result("查询镜像"), "", nil, false)
			assert.NotEmptyf(t, opts,
				"type=%q tag=%q is clickable on the cards but reaches 未找到可选镜像", typeOpt.Value, tagOpt.Value)
		}
	}
	require.GreaterOrEqual(t, pairs, 6, "premise: the walk actually exercised reachable pairs")
}

// TestTagCountsDescribeThePostTypeCandidates pins WHICH set the tag counts come
// from. Counting the whole catalog would put "9 个镜像" beside a tag that only has
// three images left under the chosen type — the same class of lie as the facet
// count, one card further along.
func TestTagCountsDescribeThePostTypeCandidates(t *testing.T) {
	catalog := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "app-1", "Name": "A", "ImageType": "App", "Status": "Available", "Tags": []any{"pytorch"}},
		map[string]any{"CompShareImageId": "app-2", "Name": "B", "ImageType": "App", "Status": "Available", "Tags": []any{"pytorch"}},
		map[string]any{"CompShareImageId": "oth-1", "Name": "C", "ImageType": "Other", "Status": "Available", "Tags": []any{"pytorch"}},
	}}
	ctxFor := func(imageType string) *Context {
		return &Context{
			Params:      map[string]any{"ImageSource": "platform", "ImageType": imageType},
			StepResults: map[string]map[string]any{"查询镜像": catalog},
		}
	}
	note := func(imageType string) string {
		for _, o := range imageTagFacetOptions(createImageCandidates(ctxFor(imageType))) {
			if strings.EqualFold(o.Value, "pytorch") {
				return o.Note
			}
		}
		return ""
	}

	assert.Equal(t, "3 个镜像", note(""), "unfiltered, every row carrying the tag counts")
	assert.Equal(t, "2 个镜像", note("App"), "under a type, only that type's rows count")
	assert.Equal(t, "1 个镜像", note("Other"))
}

// TestNearIdenticalUpstreamTagsStayDistinctOptions records a deliberate non-fix.
// miniconda and Miniconda3 are two real upstream tags and the card offers both;
// folding them would mean asserting an equivalence the platform never stated — a
// keyword table by another name. The counts are what tell the user which one has
// images, which is why they are not optional.
func TestNearIdenticalUpstreamTagsStayDistinctOptions(t *testing.T) {
	opts := imageTagFacetOptions(createImageCandidates(candidateWfCtx(map[string]any{"ImageType": "App"})))
	byValue := map[string]ConfirmFormOption{}
	for _, o := range opts {
		byValue[o.Value] = o
	}
	require.Contains(t, byValue, "miniconda")
	require.Contains(t, byValue, "Miniconda3")
	assert.NotEmpty(t, byValue["miniconda"].Note)
	assert.NotEmpty(t, byValue["Miniconda3"].Note)
}
