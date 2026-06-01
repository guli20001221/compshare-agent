package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/skills"
)

// fastTierContractExecutor returns minimally-populated success data for every
// capability tool so each fast-tier handler exercises its TYPED-envelope path
// (gpu_specs→KindGPUSpecsQuery, stock→KindStockAvailability, image lists→KindImageList),
// not just the empty→nil path. Whatever envelope results MUST be bypass-eligible.
type fastTierContractExecutor struct{}

func (fastTierContractExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"GraphicsMemory": map[string]any{"Value": float64(24)},
				"MachineSizes":   []any{map[string]any{"Gpu": float64(1)}}},
		}}, nil
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch 2.9", "ImageType": "App"},
		}}, nil
	case "DescribeCompShareCustomImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-2", "Name": "my-image", "Status": "Available"},
		}}, nil
	case "DescribeCommunityImages":
		return map[string]any{"CompshareImageGroup": []any{
			map[string]any{"ImageName": "LiveTalking", "Data": []any{map[string]any{"CompShareImageId": "img-3"}}},
		}}, nil
	case "GetCompShareInstancePrice":
		return map[string]any{"PriceSet": []any{map[string]any{"Price": float64(1.5)}}}, nil
	case "CheckCompShareResourceCapacity":
		return map[string]any{"Specs": []any{map[string]any{"Gpu": float64(1), "ResourceEnough": true}}}, nil
	}
	return map[string]any{}, nil
}

// TestFastTierSkills_HandlerEnvelopeBypassesRenderer locks the RENDER half of the
// fast-tier determinism contract (P3a-1): every applicable_tiers:[fast] skill's
// handler produces an envelope that bypasses the LLM renderer — either nil
// (pricing_query / stock capacity-precheck branch) OR a fast-tier Kind (gpu_specs /
// stock / image_list). All THREE bypass situations are covered by the single
// `nil || isFastTierEnvelope(Kind)` predicate, which is exactly the bypass logic in
// renderGroundedHandlerResult (engine.go:1603 nil-envelope, :1611 fast-tier-kind).
// A new fast skill wired to a handler that emits a non-fast typed envelope (which
// would silently reach the LLM renderer) fails here.
func TestFastTierSkills_HandlerEnvelopeBypassesRenderer(t *testing.T) {
	h := intent.NewDemoHandler(fastTierContractExecutor{})
	fast := 0
	for _, s := range skills.GeneratedSkills() {
		if !slices.Contains(s.ApplicableTiers, skills.TierFast) {
			continue
		}
		fast++
		res := h.DispatchCapability(context.Background(),
			intent.HandlerRequest{Plan: intent.Plan{Intent: intent.Intent(s.IntentLabel)}})
		if res.Envelope != nil {
			assert.Truef(t, isFastTierEnvelope(res.Envelope.Kind),
				"fast-tier skill %q produced envelope Kind %q — NOT fast-tier, would reach the LLM renderer", s.Name, res.Envelope.Kind)
		}
	}
	require.GreaterOrEqualf(t, fast, 6,
		"expected the 6 catalog capability skills to be fast-tier (got %d) — non-vacuity guard", fast)
}
