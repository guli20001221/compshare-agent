package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsImageUnavailableMessage locks WHICH create failures are eligible for the
// zone-image recovery: only the upstream "image not available in this zone" 230,
// never sold-out stock / balance / other failures (those have different remedies
// and must keep their existing grounded replies).
func TestIsImageUnavailableMessage(t *testing.T) {
	assert.True(t, isImageUnavailableMessage("步骤「检查库存」执行失败: API error (RetCode=230): Params [CompShareImageId] not available"))
	assert.False(t, isImageUnavailableMessage("4090 1 卡当前库存不足（售罄），请换一个规格或稍后再试。"))
	assert.False(t, isImageUnavailableMessage("API error (RetCode=400): insufficient balance"))
	assert.False(t, isImageUnavailableMessage(""))
}

// TestRankCreateImageCandidates locks the recovery's image-selection intent: a
// "PyTorch" request must prefer a name that contains the keyword, then one that
// shares a meaningful substring (cuda…torch…), and must DROP unrelated images (a
// bare Ubuntu/Windows system image) so recovery never silently swaps in a wholly
// different kind of image. The already-failed image is excluded.
func TestRankCreateImageCandidates(t *testing.T) {
	imageSet := []any{
		map[string]any{"CompShareImageId": "img-bad", "Name": "PyTorch:24.04-py3"},       // the failed one
		map[string]any{"CompShareImageId": "img-cuda", "Name": "cuda128_torch291_py312"}, // shares "torch"
		map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04"},         // unrelated → dropped
		map[string]any{"CompShareImageId": "img-pt", "Name": "PyTorch 2.1 CUDA 12.1"},    // contains keyword
	}
	got := rankCreateImageCandidates(imageSet, "PyTorch", "img-bad")

	require.Len(t, got, 2, "the failed image and the unrelated Ubuntu image must be excluded")
	assert.Equal(t, "img-pt", got[0].id, "name containing the keyword ranks above a substring-only share")
	assert.Equal(t, "img-cuda", got[1].id, "cuda…torch… shares a substring and stays as a fallback candidate")
}

// TestSharesSubstringFold covers the substring bridge that connects a "PyTorch"
// request to the platform's actual "cuda…torch…py…" image names.
func TestSharesSubstringFold(t *testing.T) {
	assert.True(t, sharesSubstringFold("PyTorch", "cuda128_torch291_py312", 4)) // shares "torch"
	assert.False(t, sharesSubstringFold("PyTorch", "Ubuntu 22.04", 4))
	assert.False(t, sharesSubstringFold("SD", "cuda128_torch291", 4)) // keyword shorter than minLen
}

// recoveryMockExecutor is arg-sensitive (unlike the action-keyed mockExecutor):
// CheckCompShareResourceCapacity 230s for the zone-absent image but succeeds for
// the recovered one, and DescribeCompShareImages returns the lone unavailable
// image for the narrow Name="PyTorch" query but the available alternatives for
// the recovery's broad (no-Name) query. This is exactly the real upstream shape
// the live probe revealed.
type recoveryMockExecutor struct {
	calls []string
}

func (m *recoveryMockExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"TotalCount": float64(0), "UHostSet": []any{}}, nil
	case "DescribeCompShareImages":
		if name, _ := args["Name"].(string); name == "PyTorch" {
			// Narrow name match → the single image that is absent from the zone.
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-bad", "Name": "PyTorch:24.04-py3", "ImageType": "App"},
			}}, nil
		}
		// Broad recovery query (and the re-run's exact-name query) → alternatives.
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-good", "Name": "cuda128_torch291_py312", "ImageType": "App"},
			map[string]any{"CompShareImageId": "img-win", "Name": "Windows-nvidia 2022", "ImageType": "System"},
		}}, nil
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal", "MachineSizes": []any{
				map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}},
			}},
		}}, nil
	case "CheckCompShareResourceCapacity":
		if id, _ := args["CompShareImageId"].(string); id == "img-bad" {
			return nil, fmt.Errorf("API error (RetCode=230): Params [CompShareImageId] not available")
		}
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}}, nil
	case "GetCompShareInstanceUserPrice":
		return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}}, nil
	case "CreateCompShareInstance":
		return map[string]any{"UHostIds": []any{"uhost-good1"}}, nil
	}
	return map[string]any{"Action": action, "RetCode": float64(0)}, nil
}

// TestCreateZoneRecovery_SwapsToAvailableImage is the end-to-end proof of the
// fix: a named platform image that 230s in the resolved zone is transparently
// recovered to an available same-intent image, and the user reaches a confirm
// card for the WORKING image (with a FallbackNote) instead of a cryptic
// RetCode=230. The confirm is declined so no instance is actually created.
