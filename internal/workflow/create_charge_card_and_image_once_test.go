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
	skip, err := shouldSkipGuidedChargeTypeStep(explicit)
	require.NoError(t, err)
	assert.True(t, skip, "the user already said Spot; asking again asks them to repeat themselves")

	fullySpecified := formWfCtx(t, map[string]any{
		"GpuType": "A800", "GuidedGpuLocked": true, "Zone": "cn-wlcb-01",
		"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
		"GuidedRecommended": true, "ImageName": "PyTorch",
	})
	skip, err = shouldSkipGuidedChargeTypeStep(fullySpecified)
	require.NoError(t, err)
	assert.True(t, skip, "every other card is skipped; do not turn a card-free flow into a one-card flow")
}

// The community path picks a concrete image version on its own card. The final
// card then re-opened the same choice, asking the same question twice. The
// platform path has no such card by design (shouldSkipGuidedImageStep: the
// concrete image step IS the community picker), so there the final card is the
// only selector and must stay editable.
func TestFinalCardReopensTheImageOnlyWhenNoEarlierCardAskedForIt(t *testing.T) {
	params := map[string]any{
		"GpuType": "4090", "Zone": "cn-wlcb-01", "Gpu": float64(1),
		"Cpu": float64(16), "Memory": float64(65536),
	}

	platform := formWfCtx(t, params)
	form, err := buildGuidedFinalForm(platform)
	require.NoError(t, err)
	image := form.Field("ImageId")
	require.NotNil(t, image, "the platform path has no earlier image card, so this is the selector")
	assert.True(t, image.Editable)
	assert.NotEmpty(t, image.Options)

	picked := formWfCtx(t, params)
	markGuidedStepReached(picked, guidedStepImage)
	form, err = buildGuidedFinalForm(picked)
	require.NoError(t, err)
	image = form.Field("ImageId")
	require.NotNil(t, image, "the chosen image is still shown — just not re-asked")
	assert.False(t, image.Editable, "an image card already ran; re-opening it asks twice")
	assert.Empty(t, image.Options, "a stated value carries no option list")
}
