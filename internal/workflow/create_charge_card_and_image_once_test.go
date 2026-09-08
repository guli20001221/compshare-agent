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

	// Prefilling the machine does not remove the purchase choice.
	fullySpecified := formWfCtx(t, map[string]any{
		"GpuType": "A800", "GuidedGpuLocked": true, "Zone": "cn-wlcb-01",
		"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
		"GuidedRecommended": true, "CompShareImageId": "img-002", "ImageName": "PyTorch 2.4",
	})
	skip, err = shouldSkipGuidedChargeTypeStep(fullySpecified)
	require.NoError(t, err)
	assert.False(t, skip, "nothing here is the user's billing choice; still ask")
}

func TestChargeTypeCardKeepsPrefilledModesEditable(t *testing.T) {
	for _, charge := range []string{"Postpay", "Spot", "Day", "Month"} {
		t.Run(charge, func(t *testing.T) {
			wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090", "ChargeType": charge})
			skip, err := shouldSkipGuidedChargeTypeStep(wfCtx)
			require.NoError(t, err)
			require.False(t, skip, "a prefilled mode must not suppress the purchase choice")
			form, err := buildGuidedChargeTypeForm(wfCtx)
			require.NoError(t, err)
			field := fieldByKey(t, form, "ChargeType")
			assert.Equal(t, charge, field.Value)
			assert.True(t, field.Editable)
			assert.Greater(t, len(field.Options), 1)
		})
	}
}

// The supplied framework name should rank the same live candidates regardless of
// case. With no version index, candidates retain the catalog order.
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

// Both image sources settle the concrete image before hardware. The final card
// states that choice without reopening an edit that would invalidate hardware.
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
