package intent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectRouteEnvelopesNeverPromoteUserTextToEvidence(t *testing.T) {
	const falsePremise = "UNTRUSTED_USER_PREMISE_4090_IS_10_CARDS"
	imageFields := []string{"CompShareImageId", "Name", "ImageType"}
	tests := map[string]envelope.Envelope{
		"gpu_specs_query":    buildGPUSpecsEnvelope(map[string]any{}, Slots{}),
		"stock_availability": buildStockEnvelope(map[string]any{}, falsePremise),
		"platform_image_list": buildImageListEnvelope(
			map[string]any{}, "ImageSet", imageFields, Slots{},
			"DescribeCompShareImages", "platform",
		),
		"community_image_list": buildCommunityImageEnvelope(map[string]any{}, Slots{}),
	}

	for name, env := range tests {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(env)
			require.NoError(t, err)
			assert.NotContains(t, string(payload), falsePremise)
			for _, fact := range append(append([]envelope.Fact{}, env.Facts...), env.Computed...) {
				assert.NotEqual(t, "user_question", fact.Key)
				assert.NotEqual(t, "requested_gpu_specs", fact.Key)
			}
		})
	}
}

func TestAllProductionDirectIntentResultsKeepUserTextOutOfEvidence(t *testing.T) {
	const falsePremise = "UNTRUSTED_USER_PREMISE_MUST_NOT_BE_EVIDENCE"
	handler := NewDemoHandler(stubFailingExecutor{})
	request := func(value Intent) HandlerRequest {
		return HandlerRequest{
			Plan: IntentRoute{SchemaVersion: SchemaVersion, Intent: value},
		}
	}
	tests := map[Intent]func() HandlerResult{
		IntentResourceInfo: func() HandlerResult {
			return handler.HandleResourceInfo(context.Background(), request(IntentResourceInfo))
		},
		IntentMonitorQuery: func() HandlerResult {
			return handler.HandleMonitorQuery(context.Background(), request(IntentMonitorQuery))
		},
		IntentGPUSpecsQuery: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentGPUSpecsQuery))
		},
		IntentStockAvailability: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentStockAvailability))
		},
		IntentPricingQuery: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentPricingQuery))
		},
		IntentRefundEstimate: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentRefundEstimate))
		},
		IntentImageTagCatalog: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentImageTagCatalog))
		},
		IntentModelRepositoryBrowse: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentModelRepositoryBrowse))
		},
		IntentImageList: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentImageList))
		},
		IntentNetAcceleratorStatus: func() HandlerResult {
			return handler.DispatchRoute(context.Background(), request(IntentNetAcceleratorStatus))
		},
	}
	require.Len(t, tests, 10)

	for value, run := range tests {
		t.Run(string(value), func(t *testing.T) {
			result := run()
			if result.Envelope == nil {
				return
			}
			payload, err := json.Marshal(result.Envelope)
			require.NoError(t, err)
			assert.NotContains(t, string(payload), falsePremise)
		})
	}
}
