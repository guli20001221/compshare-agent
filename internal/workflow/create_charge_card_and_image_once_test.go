package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The charge type is now a card. It used to be reachable only by saying it in
// the request ("用抢占式创建一台…"), which was a workaround for an ordering
// constraint that measurement removed — see
// TestChargeTypeIsSettledBeforeEveryPoolScopedStep.
func TestChargeTypeCardIsOfferedWhenTheUserDidNotSayIt(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090"})

	skip, err := shouldSkipGuidedChargeTypeStep(wfCtx)
	require.NoError(t, err)
	require.False(t, skip, "no charge type was given and other cards show, so ask")

	form, err := buildGuidedChargeTypeForm(wfCtx)
	require.NoError(t, err)
	field := fieldByKey(t, form, "ChargeType")
	assert.True(t, field.Editable)
	assert.Equal(t, "Postpay", field.Value, "the existing default stays the default")
	assert.Len(t, field.Options, len(createFormChargeTypes))
	assert.NotEmpty(t, form.Step.Description, "a card without guidance text is a bare card")
}

// Two ways of already knowing the answer, neither of which should produce a
// question. The second is the one that matters for the request that pinned
// everything: adding a card there would interrogate the only user who asked for
// nothing.
func TestChargeTypeCardIsNotAskedWhenThereIsNothingToAsk(t *testing.T) {
	explicit := formWfCtx(t, map[string]any{"GpuType": "4090", "ChargeType": "Spot"})
	// "The user said Spot" is a fact about PROVENANCE, not about the params map.
	// This setup used to express it as key-presence, which is the same conflation
	// the flag removes — the Agent's own default lands in Params identically.
	explicit.referenceData.ChargeTypeUserPinned = true
	skip, err := shouldSkipGuidedChargeTypeStep(explicit)
	require.NoError(t, err)
	assert.True(t, skip, "the user already said Spot; asking again asks them to repeat themselves")

	fullySpecified := formWfCtx(t, map[string]any{
		"GpuType": "A800", "GuidedGpuLocked": true, "Zone": "cn-wlcb-01",
		"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
		"GuidedRecommended": true, "CompShareImageId": "img-002", "ImageName": "PyTorch 2.4",
	})
	skip, err = shouldSkipGuidedChargeTypeStep(fullySpecified)
	require.NoError(t, err)
	assert.True(t, skip, "every other card is skipped; do not turn a card-free flow into a one-card flow")
}

// The regression this pins, observed in live 联调: "在上海二A替我开一台4090" never
// showed the purchase-mode card. The create tool's schema says "默认 Postpay", so
// the Agent puts ChargeType in the params on a request that said nothing about
// billing — and the skip used to read key-presence as the user's answer. The card
// then vanished for exactly the users it exists for, and the final card told them
// to "重新发起创建" to change something they were never offered.
//
// Red on the old key-presence rule: it returns skip=true here.
func TestAgentSuppliedChargeTypeDefaultStillAsksTheUser(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090", "ChargeType": "Postpay"})
	// ChargeTypeUserPinned stays false: nothing in the user's words named a mode.

	skip, err := shouldSkipGuidedChargeTypeStep(wfCtx)
	require.NoError(t, err)
	require.False(t, skip, "the Agent's default is not the user's choice; still ask")

	form, err := buildGuidedChargeTypeForm(wfCtx)
	require.NoError(t, err)
	field := fieldByKey(t, form, "ChargeType")
	assert.True(t, field.Editable)
	assert.Equal(t, "Postpay", field.Value, "the value the Agent supplied is preselected, not sealed")
	assert.Len(t, field.Options, len(createFormChargeTypes),
		"all four purchase modes remain选-able")
}

// What the user asked for: say "最新pytorch" — in whatever casing — and the picker
// offers the PyTorch images to choose from, not a dead end and not all 75 rows.
//
// The casing is the whole point and is not incidental: a slot only earns
// SourceUserExplicit by being a verbatim span of the user's message, so the Agent
// sends the user's own spelling. Upstream's Name filter is case-sensitive
// ("Pytorch" = 0 rows live), which is why the query no longer narrows and this
// client-side ranking is now what does the filtering — nameSimilarity lowercases
// both sides, so every casing lands the same candidate set.
//
// NOT asserted: newest-first. rankRecommendations has a same-framework tiebreak on
// Software.FrameworkVersionIndex, but the live catalog sends that field as 0 on
// EVERY image (measured 2026-07-28: Framework="PyTorch", FrameworkVersion="2.13.0",
// FrameworkVersionIndex=0). Populating it in a fixture would test a value upstream
// never produces. Order is therefore the catalog's own, which the fixture pins
// verbatim.
func TestImagePickerOffersTheNamedFrameworkWhateverTheCasing(t *testing.T) {
	images := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-u", "Name": "Ubuntu 22.04 CUDA 12", "Size": float64(102400)},
		map[string]any{"CompShareImageId": "img-p25", "Name": "pytorch_2.5.0_Py3.12", "Size": float64(102400)},
		map[string]any{"CompShareImageId": "img-p1", "Name": "pytorch_1.8.1_Py3.8", "Size": float64(102400)},
		map[string]any{"CompShareImageId": "img-p23", "Name": "pytorch_2.3.0_Py3.12", "Size": float64(102400)},
		map[string]any{"CompShareImageId": "img-c", "Name": "ComfyUI", "Size": float64(102400)},
	}}

	for _, spelling := range []string{"Pytorch", "pytorch", "PyTorch"} {
		t.Run(spelling, func(t *testing.T) {
			params := map[string]any{"GpuType": "4090", "ImageName": spelling}

			current, opts, total := guidedImageFormOptions(params, images, "4090", nil, false)

			require.NotEmpty(t, opts, "a wrong-cased name must not empty the picker")
			values := optionValues(&ConfirmFormField{Options: opts})
			assert.NotContains(t, values, "img-u", "an unrelated image is not a PyTorch candidate")
			assert.NotContains(t, values, "img-c", "an unrelated image is not a PyTorch candidate")
			assert.Equal(t, 3, total, "all three PyTorch images are candidates")
			assert.Equal(t, []string{"img-p25", "img-p1", "img-p23"}, values,
				"catalog order, since upstream gives no version index to sort by")
			assert.Equal(t, "img-p25", current, "the card preselects the leading candidate")
		})
	}
}

// The community path picks a concrete image version on its own card. The final
// card then re-opened the same choice, asking the same question twice. The
// platform path has no such card by design (shouldSkipGuidedImageStep: the
// concrete image step IS the community picker), so there the final card is the
// only selector and must stay editable.
func TestFinalCardNeverReopensTheResolvedImage(t *testing.T) {
	params := map[string]any{
		"GpuType": "4090", "Zone": "cn-wlcb-01", "Gpu": float64(1),
		"Cpu": float64(16), "Memory": float64(65536),
	}

	platform := formWfCtx(t, params)
	form, err := buildGuidedFinalForm(platform)
	require.NoError(t, err)
	image := form.Field("ImageId")
	require.NotNil(t, image)
	assert.False(t, image.Editable)
	assert.Empty(t, image.Options)

	picked := formWfCtx(t, params)
	markGuidedStepReached(picked, guidedStepImage)
	form, err = buildGuidedFinalForm(picked)
	require.NoError(t, err)
	image = form.Field("ImageId")
	require.NotNil(t, image, "the chosen image is still shown — just not re-asked")
	assert.False(t, image.Editable, "the final card is confirmation-only")
	assert.Empty(t, image.Options, "a stated value carries no option list")
}
