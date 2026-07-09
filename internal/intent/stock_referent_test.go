package intent

import (
	"context"
	"strings"
	"testing"
)

// multiModelStockExecutor returns a multi-model stock list plus the image +
// capacity responses the precheck path needs. It lets the referent tests
// assert that an ellipsis follow-up does NOT re-list every model.
type multiModelStockExecutor struct{}

func (multiModelStockExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal"},
			map[string]any{"Name": "5090", "Zone": "cn-wlcb-01", "Status": "Normal"},
			map[string]any{"Name": "H100", "Zone": "cn-wlcb-01", "Status": "Normal"},
			map[string]any{"Name": "A100", "Zone": "cn-wlcb-01", "Status": "Normal"},
		}}, nil
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
		}}, nil
	case "CheckCompShareResourceCapacity":
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}}, nil
	default:
		return map[string]any{}, nil
	}
}

// TestStockReferentText pins the RC017 referent-resolution rules: the prior
// model is reused ONLY when the route has no search_query. A route-supplied
// search_query is authoritative, even when it does not match a current model.
func TestStockReferentText(t *testing.T) {
	items := []any{
		map[string]any{"Name": "4090"},
		map[string]any{"Name": "5090"},
		map[string]any{"Name": "H100"},
	}
	cases := []struct {
		name     string
		userText string
		search   string
		fallback string
		want     string
	}{
		{"route search query is authoritative", "4090有货吗", "4090", "5090", "4090"},
		{"pure ellipsis reuses fallback", "现在还有库存吗", "", "4090", "4090"},
		{"ellipsis without fallback has no filter", "现在还有库存吗", "", "", ""},
		{"unknown route search query does not swap to fallback", "H200还有吗", "H200", "4090", "H200"},
		{"retired fallback is not resurrected", "现在还有库存吗", "", "TEST_GPU_X", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := HandlerRequest{
				Plan:             IntentRoute{Intent: IntentStockAvailability, Slots: Slots{SearchQuery: c.search}},
				UserText:         c.userText,
				FallbackGpuModel: c.fallback,
			}
			if got := stockReferentText(req, items); got != c.want {
				t.Errorf("stockReferentText(text=%q, fb=%q) = %q, want %q", c.userText, c.fallback, got, c.want)
			}
		})
	}
}

// TestSingleStockModel: only an unambiguous single match is recorded as the
// session referent — a multi-model or list-all turn must not bind one model.
func TestSingleStockModel(t *testing.T) {
	items := []any{
		map[string]any{"Name": "4090"},
		map[string]any{"Name": "5090"},
	}
	if got := singleStockModel("4090", items); got != "4090" {
		t.Errorf("single match = %q, want 4090", got)
	}
	if got := singleStockModel("4090 5090", items); got != "" {
		t.Errorf("multi match must not bind a referent, got %q", got)
	}
	if got := singleStockModel("", items); got != "" {
		t.Errorf("no match must not bind a referent, got %q", got)
	}
}

// TestHandleStockAvailability_EllipsisReusesFallbackReferent is the RC017
// gate: turn-2 "现在还有库存吗" with a prior-turn referent of 4090 must answer
// about 4090 only, never re-listing the other models, and re-record 4090.
func TestHandleStockAvailability_EllipsisReusesFallbackReferent(t *testing.T) {
	h := NewDemoHandler(multiModelStockExecutor{})
	req := HandlerRequest{
		Plan:             IntentRoute{Intent: IntentStockAvailability},
		UserText:         "现在还有库存吗",
		FallbackGpuModel: "4090",
	}
	res := handleStockAvailability(context.Background(), h, req)

	if res.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want handled", res.Status)
	}
	if res.ResolvedStockGpuModel != "4090" {
		t.Errorf("ResolvedStockGpuModel = %q, want 4090", res.ResolvedStockGpuModel)
	}
	if !strings.Contains(res.Reply, "4090") {
		t.Errorf("reply should answer about the referent 4090: %q", res.Reply)
	}
	for _, other := range []string{"5090", "H100", "A100"} {
		if strings.Contains(res.Reply, other) {
			t.Errorf("ellipsis follow-up must not re-list %s: %q", other, res.Reply)
		}
	}
}

// TestHandleStockAvailability_EllipsisWithoutFallbackListsAll proves the fix
// is load-bearing: with no prior referent the same ellipsis lists every model
// (the unfixed behavior). If this ever stops listing all models, the referent
// reuse above would be untestable noise.
func TestHandleStockAvailability_EllipsisWithoutFallbackListsAll(t *testing.T) {
	h := NewDemoHandler(multiModelStockExecutor{})
	req := HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "现在还有库存吗",
	}
	res := handleStockAvailability(context.Background(), h, req)

	if res.ResolvedStockGpuModel != "" {
		t.Errorf("no referent expected without fallback, got %q", res.ResolvedStockGpuModel)
	}
	if !strings.Contains(res.Reply, "5090") {
		t.Errorf("without a referent the reply should list all models incl 5090: %q", res.Reply)
	}
}

// TestHandleStockAvailability_NamedModelRecordsReferent covers the write side:
// a turn that names one model resolves and records that model so the NEXT
// turn can elide it.
func TestHandleStockAvailability_NamedModelRecordsReferent(t *testing.T) {
	h := NewDemoHandler(multiModelStockExecutor{})
	req := HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability, Slots: Slots{SearchQuery: "4090"}},
		UserText: "4090现在有货吗",
	}
	res := handleStockAvailability(context.Background(), h, req)

	if res.ResolvedStockGpuModel != "4090" {
		t.Errorf("ResolvedStockGpuModel = %q, want 4090", res.ResolvedStockGpuModel)
	}
	if strings.Contains(res.Reply, "5090") {
		t.Errorf("a named-4090 turn must not list 5090: %q", res.Reply)
	}
}
