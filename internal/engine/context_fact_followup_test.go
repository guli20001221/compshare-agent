package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestRecentFactFollowupPriceRequeriesWithContextDecisionSlot(t *testing.T) {

	var priceArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{
					"Name": "5090",
					"Zone": "cn-wlcb-01",
					"MachineSizes": []any{map[string]any{
						"Gpu": float64(1),
						"Collection": []any{map[string]any{
							"Cpu":    float64(16),
							"Memory": []any{float64(64)},
						}},
					}},
				},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			priceArgs = cloneTestArgs(args)
			return map[string]any{"Postpay": float64(2.58)}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "should not answer"}}}, exec, nil)
	eng.Init(context.Background())
	now := time.Now()
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		RecentFacts: []ToolFact{{
			Kind:           FactKindPriceQuote,
			SubjectID:      "price:GetCompShareInstanceUserPrice:4090",
			Payload:        map[string]any{"action": "GetCompShareInstanceUserPrice", "gpu_type": "4090", "price": float64(1.58)},
			ProducedAtTurn: 1,
			ProducedAtUnix: now.Unix(),
			TTLSeconds:     factTTLSecondsPriceQuote,
		}},
	}, 2)
	eng.SetSessionFactContextEnabled(true)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionAnswerFollowup,
		Target:      ContextDecisionTargetPricing,
		SlotUpdates: map[string]string{"gpu_type": "5090"},
		Reason:      "user asks for the same price question with a new GPU",
	}})
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Confidence:    0.9,
		},
	}}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentUnknown, intent.IntentPricingQuery}})

	reply, err := eng.Chat(context.Background(), "那它呢", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "5090")
	require.Contains(t, reply, "2.58")
	require.NotNil(t, priceArgs, "price follow-up must re-call the pricing API")
	require.Equal(t, "5090", priceArgs["GpuType"])
}

func TestRecentFactFollowupStockRequeriesWithContextDecisionSlot(t *testing.T) {

	var capacityArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "5090", "Zone": "cn-wlcb-01", "Status": "Normal"},
			}}, nil
		case "DescribeCompShareSupportZone":
			return contextFactFollowupSupportZones(), nil
		case "DescribeCompShareGpuInventory":
			return map[string]any{"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"1": map[string]any{"5090": float64(9)},
			}}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
			}}, nil
		case "CheckCompShareResourceCapacity":
			capacityArgs = cloneTestArgs(args)
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "should not answer"}}}, exec, nil)
	eng.Init(context.Background())
	now := time.Now()
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		RecentFacts: []ToolFact{{
			Kind:           FactKindStockSnapshot,
			SubjectID:      "stock:4090",
			Payload:        map[string]any{"action": "stock_availability", "model": "4090", "status": "Normal"},
			ProducedAtTurn: 1,
			ProducedAtUnix: now.Unix(),
			TTLSeconds:     factTTLSecondsStockSnapshot,
		}},
	}, 2)
	eng.SetSessionFactContextEnabled(true)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionAnswerFollowup,
		Target:      ContextDecisionTargetStock,
		SlotUpdates: map[string]string{"gpu_type": "5090"},
		Reason:      "user asks for the same stock question with a new GPU",
	}})
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Confidence:    0.9,
		},
	}}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentUnknown, intent.IntentStockAvailability}})

	reply, err := eng.Chat(context.Background(), "那它呢", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "5090")
	require.Contains(t, reply, "可以新建实例")
	require.NotNil(t, capacityArgs, "stock follow-up must re-run capacity precheck")
	require.Equal(t, "5090", capacityArgs["GpuType"])
}

func TestRecentFactFollowupRefundRevalidatesSelectedInstance(t *testing.T) {

	var refundCalled bool
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{
				"UHostId": "uhost-selected",
				"Name":    "selected-host",
				"State":   "Running",
				"GpuType": "4090",
				"GPU":     float64(1),
				"CPU":     float64(16),
				"Memory":  float64(65536),
				"Zone":    "cn-wlcb-01",
			}}}, nil
		case "GetCompShareRefundPrice":
			refundCalled = true
			require.Equal(t, []string{"uhost-selected"}, stringSliceArg(args["UHostIds"]))
			return map[string]any{"RefundPriceSet": []any{map[string]any{
				"UHostId":     "uhost-selected",
				"RefundPrice": float64(42.5),
			}}}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "should not answer"}}}, exec, nil)
	eng.Init(context.Background())
	now := time.Now()
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-selected",
		SelectedInstanceName: "selected-host",
		RecentFacts: []ToolFact{{
			Kind:           FactKindBillingQuote,
			SubjectID:      "billing:DiagnoseBilling:uhost-selected",
			Payload:        map[string]any{"action": "DiagnoseBilling", "resource_id": "uhost-selected", "amount": float64(18.8)},
			ProducedAtTurn: 1,
			ProducedAtUnix: now.Unix(),
			TTLSeconds:     factTTLSecondsBillingQuote,
		}},
	}, 2)
	eng.SetSessionFactContextEnabled(true)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:     ContextDecisionAnswerFollowup,
		Target:       ContextDecisionTargetBilling,
		BillingTopic: "refund",
		Reason:       "user asks for refund estimate for the selected instance",
	}})
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Confidence:    0.9,
		},
	}}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentUnknown, intent.IntentRefundEstimate}})

	reply, err := eng.Chat(context.Background(), "那现在退费多少", noopStep)

	require.NoError(t, err)
	require.True(t, refundCalled, "refund follow-up must revalidate visibility and call refund estimate")
	require.Contains(t, reply, "42.50")
}

func TestRecentFactFollowupBillingRerunsDiagnosis(t *testing.T) {

	var describeCalls int
	var billingArgs map[string]any
	var refundCalled bool
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			describeCalls++
			if _, ok := args["UHostIds"]; ok {
				billingArgs = cloneTestArgs(args)
			}
			return map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{
				"UHostId":       "uhost-selected",
				"Name":          "selected-host",
				"State":         "Running",
				"ChargeType":    "Dynamic",
				"GpuType":       "4090",
				"GPU":           float64(1),
				"CPU":           float64(16),
				"Memory":        float64(65536),
				"Zone":          "cn-wlcb-01",
				"InstancePrice": float64(1.58),
				"DiskPrice":     float64(0.05),
			}}}, nil
		case "GetCompShareRefundPrice":
			refundCalled = true
			return map[string]any{"RefundPrice": float64(42.5)}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "should not answer"}}}, exec, nil)
	eng.Init(context.Background())
	now := time.Now()
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-selected",
		SelectedInstanceName: "selected-host",
		RecentFacts: []ToolFact{{
			Kind:           FactKindBillingQuote,
			SubjectID:      "billing:DiagnoseBilling:uhost-selected",
			Payload:        map[string]any{"action": "DiagnoseBilling", "resource_id": "uhost-selected", "amount": float64(99.99)},
			ProducedAtTurn: 1,
			ProducedAtUnix: now.Unix(),
			TTLSeconds:     factTTLSecondsBillingQuote,
		}},
	}, 2)
	eng.SetSessionFactContextEnabled(true)
	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:     ContextDecisionAnswerFollowup,
		Target:       ContextDecisionTargetBilling,
		BillingTopic: "cost",
		Reason:       "user asks for a fresh billing diagnosis for the selected instance",
	}}
	eng.SetContextDecisionLayer(layer)
	unknownPlan := intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentUnknown,
		Confidence:    0.9,
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{
		unknownPlan, // account-level billing precheck
		unknownPlan, // normal planner dispatch
	}}
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentUnknown, intent.IntentBillingInstance, intent.IntentRefundEstimate}})

	reply, err := eng.Chat(context.Background(), "那现在费用多少", noopStep)

	require.NoError(t, err)
	require.NotEmpty(t, layer.calls, "billing fact follow-up must consult the context decision layer")
	require.GreaterOrEqual(t, describeCalls, 1, "billing follow-up must re-run billing diagnosis")
	require.NotNil(t, billingArgs, "billing follow-up must query prices for the selected instance")
	require.Equal(t, []string{"uhost-selected"}, stringSliceArg(billingArgs["UHostIds"]))
	require.False(t, refundCalled, "non-refund billing follow-up must not call refund estimate")
	require.Contains(t, reply, "selected-host")
	require.Contains(t, reply, "费用")
	require.NotContains(t, reply, "99.99", "reply must not reuse stale billing fact amount")
}

func TestRecentFactBillingReadFailureFallsBackToContextAwareReadOnlyAgent(t *testing.T) {

	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action != "DescribeCompShareInstance" {
			return map[string]any{"RetCode": 0}, nil
		}
		if _, targeted := args["UHostIds"]; targeted {
			return nil, fmt.Errorf("billing read unavailable")
		}
		return map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			"UHostId": "uhost-selected", "Name": "selected-host", "State": "Running",
			"ChargeType": "Dynamic", "GpuType": "4090", "GPU": float64(1),
			"CPU": float64(16), "Memory": float64(65536), "Zone": "cn-wlcb-01",
		}}}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.Init(context.Background())
	now := time.Now()
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-selected",
		SelectedInstanceName: "selected-host",
		RecentFacts: []ToolFact{{
			Kind: FactKindBillingQuote, SubjectID: "billing:DiagnoseBilling:uhost-selected",
			Payload:        map[string]any{"action": "DiagnoseBilling", "resource_id": "uhost-selected", "amount": float64(99.99)},
			ProducedAtTurn: 1, ProducedAtUnix: now.Unix(), TTLSeconds: factTTLSecondsBillingQuote,
		}},
	}, 2)
	eng.SetSessionFactContextEnabled(true)
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "selected-host 上轮费用是多少"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "上轮记录是 99.99 元。"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "那现在呢"},
	)
	reply, handled := eng.tryRecentFactBillingFollowup(context.Background(), routerDispatchResult{
		result:   intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentBillingInstance}},
		snapshot: eng.RegistrySnapshot(),
	}, ContextDecision{Target: ContextDecisionTargetBilling, BillingTopic: "cost"}, noopStep)

	require.False(t, handled)
	require.Empty(t, reply)
	require.True(t, eng.routeReadFailureThisTurn, "diagnosis read failure must mark fallback; executor calls=%v", exec.calls)
	requestText := renderTestMessages(eng.buildMessagesForLLM())
	require.Contains(t, requestText, "selected-host 上轮费用是多少")
	require.Contains(t, requestText, "上轮记录是 99.99 元")
	require.Contains(t, requestText, routeReadFailureNote)
}

func TestRecentFactFollowupRequiresSessionFactContextFlag(t *testing.T) {

	var priceCalled bool
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "GetCompShareInstanceUserPrice" {
			priceCalled = true
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionAnswerFollowup,
		Target:      ContextDecisionTargetPricing,
		SlotUpdates: map[string]string{"gpu_type": "5090"},
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "普通回答"}}}, exec, nil)
	eng.Init(context.Background())
	now := time.Now()
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		RecentFacts: []ToolFact{{
			Kind:           FactKindPriceQuote,
			SubjectID:      "price:GetCompShareInstanceUserPrice:4090",
			Payload:        map[string]any{"action": "GetCompShareInstanceUserPrice", "gpu_type": "4090", "price": float64(1.58)},
			ProducedAtTurn: 1,
			ProducedAtUnix: now.Unix(),
			TTLSeconds:     factTTLSecondsPriceQuote,
		}},
	}, 2)
	eng.SetSessionFactContextEnabled(false)
	eng.SetContextDecisionLayer(layer)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Confidence:    0.9,
		},
	}}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentUnknown, intent.IntentPricingQuery}})

	reply, err := eng.Chat(context.Background(), "那它呢", noopStep)

	require.NoError(t, err)
	require.Equal(t, "普通回答", reply)
	require.False(t, priceCalled, "flag-off must not consume RecentFacts")
	require.Empty(t, layer.calls, "flag-off must not call the context decision layer for RecentFacts")
}

func cloneTestArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func contextFactFollowupSupportZones() map[string]any {
	return map[string]any{"ZoneInfo": []any{
		map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(1), "Describe": "华北二A"},
	}}
}
