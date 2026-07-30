package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsImageUnavailableError locks WHICH create failures are eligible for the
// zone-image recovery: only the upstream "image not available in this zone" 230,
// never sold-out stock / balance / other failures (those have different remedies
// and must keep their existing grounded replies).
//
// These are the same four cases this test has always made, restated against the
// typed cause now that the classifier reads Result.Err instead of grepping
// Result.Message. The sold-out case is the one that changes shape and it is worth
// naming: 库存不足 is raised by OUR capacity gate from a SUCCESSFUL upstream
// response, so it carries no upstream error at all — it is ineligible because
// there is no error to classify, which is a stronger reason than its text not
// matching.
func TestIsImageUnavailableError(t *testing.T) {
	assert.True(t, isImageUnavailableError(tools.NewUpstreamAPIError(230, "Params [CompShareImageId] not available")),
		"the upstream image-not-available rejection is the one recoverable case")
	assert.False(t, isImageUnavailableError(nil),
		"sold-out stock comes from our own capacity gate on a successful response: no upstream error, not recoverable")
	assert.False(t, isImageUnavailableError(tools.NewUpstreamAPIError(400, "insufficient balance")),
		"a balance failure needs a different remedy than swapping the image")
	assert.False(t, isImageUnavailableError(fmt.Errorf("dial tcp: connection refused")),
		"a transport error carries no RetCode and must not be read as an image rejection")
}

// TestIsImageUnavailableErrorIsAnchoredToTheCode is the regression that the
// substring form could not make. It used to be enough for "230" and
// "CompShareImageId" to appear ANYWHERE in the flattened message, so a failure
// about a different param whose text merely mentioned a 230-containing number and
// the image field triggered an image swap and a whole re-run of the create.
// Reading Code as an int makes "contains 230" and "is 230" different questions.
func TestIsImageUnavailableErrorIsAnchoredToTheCode(t *testing.T) {
	assert.False(t, isImageUnavailableError(
		tools.NewUpstreamAPIError(400, "memory 23040 MB is invalid for CompShareImageId img-abc")),
		"a 400 whose text merely contains 230 (inside 23040) is not an image-unavailable rejection")
	assert.False(t, isImageUnavailableError(
		tools.NewUpstreamAPIError(230, "Params [Zone] not available")),
		"a genuine 230 about a DIFFERENT param must not swap the image")
}

// TestCreateImageUnavailableReadsTypedCauseNotMessage pins the boundary at the
// level the recovery actually calls: a workflow Result. The sold-out failure and
// the image rejection can carry text that overlaps; only the typed cause
// separates them.
func TestCreateImageUnavailableReadsTypedCause(t *testing.T) {
	imageRejected := &workflow.Result{
		Success:   false,
		StoppedAt: "检查库存",
		Message:   "步骤「检查库存」执行失败: API error (RetCode=230): Params [CompShareImageId] not available",
		Err:       tools.NewUpstreamAPIError(230, "Params [CompShareImageId] not available"),
	}
	assert.True(t, createImageUnavailable(imageRejected))

	soldOut := &workflow.Result{
		Success:   false,
		StoppedAt: "检查库存",
		Message:   "4090 1 卡当前库存不足（售罄），请换一个规格或稍后再试。",
	}
	assert.False(t, createImageUnavailable(soldOut), "our own capacity gate sets no Err and must not trigger image recovery")

	assert.False(t, createImageUnavailable(&workflow.Result{Success: true}), "a successful create is never recovered")
	assert.False(t, createImageUnavailable(nil))
}

// TestRecoveryCandidatesRankThroughResolver locks the recovery's image-selection
// intent — now delegated to the ONE interpreter (deployment.ResolveImage), no
// private matcher: a "PyTorch" request prefers a name that contains the keyword
// over a substring-only share (cuda…torch…), DROPS unrelated images (a bare Ubuntu
// system image) so recovery never silently swaps in a wholly different kind of
// image, and excludes the id that already 230'd.
func TestRecoveryCandidatesRankThroughResolver(t *testing.T) {
	snap := deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{
		{ID: "img-bad", Name: "PyTorch:24.04-py3", Source: "platform"},       // the failed one
		{ID: "img-cuda", Name: "cuda128_torch291_py312", Source: "platform"}, // shares "torch"
		{ID: "img-ubuntu", Name: "Ubuntu 22.04", Source: "platform"},         // unrelated → dropped
		{ID: "img-pt", Name: "PyTorch 2.1 CUDA 12.1", Source: "platform"},    // contains keyword
	})
	res := deployment.ResolveImage(snap, deployment.ImageRequest{Name: "PyTorch", Source: "platform"})
	got := recoveryCandidates(res, "img-bad")

	require.Len(t, got, 2, "the failed image and the unrelated Ubuntu image must be excluded")
	assert.Equal(t, "img-pt", got[0].ID, "name containing the keyword ranks above a substring-only share")
	assert.Equal(t, "img-cuda", got[1].ID, "cuda…torch… shares a substring and stays as a fallback candidate")
}

// recoveryMockExecutor is arg-sensitive (unlike the action-keyed mockExecutor):
// CheckCompShareResourceCapacity 230s for the zone-absent image but succeeds for
// the recovered one.
//
// DescribeCompShareImages returns ONE catalog for every query. It used to return
// the unavailable image only for a narrow Name="PyTorch" query and a disjoint set
// for a broad one, which made "broad" and "narrow" name different worlds — a
// relationship upstream does not have. The query is zone-blind and the broad
// result is a SUPERSET of the narrow one (measured live: no Name = 75 rows,
// Name="PyTorch" = 1 row, and that row is among the 75). The create flow stopped
// narrowing by name, so the artifact surfaced: the flow read the broad list, never
// saw the image the user actually named, picked a substring match instead and the
// 230 recovery it is supposed to exercise never fired.
//
// With one faithful catalog the test means what it says again: the literally-named
// PyTorch image ranks first (nameSimilarity 200 for a contains-match vs 100 for
// cuda…torch…), 230s in the zone, and recovery swaps it for the available one.
type recoveryMockExecutor struct {
	calls []string
}

func (m *recoveryMockExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"TotalCount": float64(0), "UHostSet": []any{}}, nil
	case "DescribeCompShareImages":
		// One zone-blind catalog for every query: the named-but-zone-absent image
		// plus the alternatives recovery can swap to.
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-bad", "Name": "PyTorch:24.04-py3", "ImageType": "App"},
			map[string]any{"CompShareImageId": "img-good", "Name": "cuda128_torch291_py312", "ImageType": "App"},
			map[string]any{"CompShareImageId": "img-win", "Name": "Windows-nvidia 2022", "ImageType": "System"},
		}}, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A"},
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
			// This mock stands in for ExternalExecutor, which returns a TYPED
			// *UpstreamAPIError on any non-zero upstream RetCode (tools/external.go:220).
			// It used to hand back a bare fmt.Errorf whose text merely imitated that
			// error — enough to satisfy the old substring classifier, but not the
			// thing production actually produces. A fixture that forges the text of a
			// typed error tests the forgery, not the code path.
			return nil, tools.NewUpstreamAPIError(230, "Params [CompShareImageId] not available")
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

func countCalls(calls []string, action string) int {
	n := 0
	for _, c := range calls {
		if c == action {
			n++
		}
	}
	return n
}

// TestCreateZoneRecovery_SwapsToAvailableImage is the end-to-end proof of the
// fix and the P4 image-replacement contract (#6): a named platform image that
// 230s in the resolved zone is transparently recovered to an available
// same-intent image, and the user reaches a fresh confirm card for the WORKING
// image (with a FallbackNote) — a new confirmed contract — instead of a cryptic
// RetCode=230. The confirm is declined, so no instance is created: image
// replacement never bypasses the confirmation gate.
func TestCreateZoneRecovery_SwapsToAvailableImage(t *testing.T) {
	exec := &recoveryMockExecutor{}

	var confirmImage, confirmFallback any
	confirmFn := func(_ string, args map[string]any) bool {
		confirmImage = args["image"]
		confirmFallback = args["FallbackNote"]
		return false // decline — assert the recovered card without creating
	}

	eng := NewWithDeps(&mockLLM{}, exec, confirmFn)
	reply := eng.executeResolvedWorkflow(context.Background(),
		mustConfirmable("CreateInstanceWorkflow", map[string]any{"GpuType": "4090", "ImageName": "PyTorch"}, zoneRefData(eng.zoneCatalogSnapshot(context.Background()))), noopStep)

	// The cryptic upstream error must never reach the user.
	assert.NotContains(t, reply, "RetCode=230")
	assert.NotContains(t, reply, "not available")
	// Declined → honest not-executed, never a false cancel.
	assert.Contains(t, reply, "未执行")
	assert.NotContains(t, reply, "已取消")

	// The confirm card shows the RECOVERED available image, with a disclosure note.
	assert.Equal(t, "cuda128_torch291_py312", confirmImage,
		"recovery must re-confirm the available image, not the unavailable one")
	note, _ := confirmFallback.(string)
	assert.Contains(t, note, "已自动为你选择可用镜像", "the card must disclose the auto image swap")

	// The recovery path actually ran (a broad image re-query), and the declined
	// confirm created nothing.
	assert.GreaterOrEqual(t, countCalls(exec.calls, "DescribeCompShareImages"), 2)
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"),
		"declined confirm must not create")
}
