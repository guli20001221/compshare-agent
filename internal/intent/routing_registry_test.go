package intent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/routing"
	"github.com/compshare-agent/internal/zones"
)

// routingIntentSet returns the route intents declared by the generated
// skill registry (non-empty intent_label), keyed by Intent for membership checks.
func routingIntentSet() map[Intent]struct{} {
	out := map[Intent]struct{}{}
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel != "" {
			out[Intent(route.IntentLabel)] = struct{}{}
		}
	}
	return out
}

// skillRequiredTool returns RequiredTools[0] for the route skill bound to
// the given intent, or "" if none.
func skillRequiredTool(i Intent) string {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == string(i) && len(route.RequiredTools) > 0 {
			return route.RequiredTools[0]
		}
	}
	return ""
}

// TestIsRoutingIntent_KnownLabels verifies all registered route intents
// return true. New routes must be picked up by IsRoutingIntent without
// any code change in callers (engine.go etc.) — this is the v1 contract.
func TestIsRoutingIntent_KnownLabels(t *testing.T) {
	wanted := []Intent{
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentNetAcceleratorStatus,
		IntentRefundEstimate,
		IntentCFSInfo,
		IntentImageTagCatalog,
		IntentModelRepositoryBrowse,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		IntentSharedImageList,
		IntentPricingQuery,
	}
	for _, intent := range wanted {
		if !IsRoutingIntent(intent) {
			t.Errorf("IsRoutingIntent(%q) = false, want true", intent)
		}
	}
}

// TestIsRoutingIntent_UnknownReturnsFalse guards against accidental "capture
// everything" predicates that would break the routing OR-list in engine.go.
func TestIsRoutingIntent_UnknownReturnsFalse(t *testing.T) {
	notRoute := []Intent{
		IntentResourceInfo,
		IntentMonitorQuery,
		IntentKnowledgeQA,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
		IntentDiagnosis,
		IntentUnknown,
		Intent("not_a_real_intent"),
	}
	for _, intent := range notRoute {
		if IsRoutingIntent(intent) {
			t.Errorf("IsRoutingIntent(%q) = true, want false", intent)
		}
	}
}

// TestRouteIntentOrder_NoDuplicates ensures the byte-identity-pinned
// routingIntentOrder has no shadowed entries (a duplicate would emit a
// duplicate planner-prompt fragment).
func TestRouteIntentOrder_NoDuplicates(t *testing.T) {
	seen := map[Intent]struct{}{}
	for _, i := range routingIntentOrder {
		if _, ok := seen[i]; ok {
			t.Errorf("duplicate intent %q in routingIntentOrder", i)
		}
		seen[i] = struct{}{}
	}
}

// TestRouteRequiredTool_BindsToRealTool guards against typo'd tool names that
// would lookup-miss in handlerActionWhitelist or fail at SafeToolExecutor. The
// required tool now comes from the generated skill registry (RequiredTools[0]).
func TestRouteRequiredTool_BindsToRealTool(t *testing.T) {
	expected := map[Intent]string{
		IntentGPUSpecsQuery:         "DescribeAvailableCompShareInstanceTypes",
		IntentStockAvailability:     "DescribeAvailableCompShareInstanceTypes",
		IntentNetAcceleratorStatus:  "CheckCompShareNetOptimizer",
		IntentRefundEstimate:        "GetCompShareRefundPrice",
		IntentCFSInfo:               "DescribeCFS",
		IntentImageTagCatalog:       "DescribeCompShareImageTags",
		IntentModelRepositoryBrowse: "DescribeModelRepositoryModels",
		IntentPlatformImageList:     "DescribeCompShareImages",
		IntentCustomImageList:       "DescribeCompShareCustomImages",
		IntentCommunityImageList:    "DescribeCommunityImages",
		IntentSharedImageList:       "DescribeCompShareSharingImages",
		IntentPricingQuery:          "GetCompShareInstanceUserPrice",
	}
	for _, i := range routingIntentOrder {
		want := expected[i]
		if want == "" {
			t.Errorf("unexpected intent %q in routingIntentOrder", i)
			continue
		}
		got, ok := routingRequiredTool(i)
		if !ok {
			t.Errorf("routingRequiredTool(%q) = (_, false), want a tool", i)
			continue
		}
		if got != want {
			t.Errorf("routingRequiredTool(%q) = %q, want %q", i, got, want)
		}
	}
}

// TestHandlerActionWhitelist_DerivesFromSkillRegistry enforces single-source-of-truth
// (memory: feedback_cross_pr_contract_drift_check). Every route skill's
// required tool (RequiredTools[0]) must be auto-included in the whitelist; nothing
// should be hardcoded twice. The exact set is separately pinned by
// TestHandlerActionWhitelist_ExactGoldenSet.
func TestHandlerActionWhitelist_DerivesFromSkillRegistry(t *testing.T) {
	wl := handlerActionWhitelist()
	for i := range routingIntentSet() {
		want := skillRequiredTool(i)
		if want == "" {
			continue
		}
		actions, ok := wl[i]
		if !ok {
			t.Errorf("route intent %q missing from handlerActionWhitelist (derivation bug)", i)
			continue
		}
		if _, ok := actions[want]; !ok {
			t.Errorf("route %q required tool %q not in whitelist[%q]", i, want, i)
		}
	}
}

// TestHandlerActionWhitelist_ExactGoldenSet is the SECURITY gate against silent
// widening of the SafeToolExecutor boundary. handlerActionWhitelist() must equal
// EXACTLY this golden set — set-equality, no missing/extra entries. The
// per-route action is the required tool (RequiredTools[0]), NOT the broader
// react_tool_subset (which would add e.g. GetGPUSpecs to gpu_specs). If any intent
// gains or loses an action, this test fails loudly.
func TestHandlerActionWhitelist_ExactGoldenSet(t *testing.T) {
	golden := map[Intent]map[string]struct{}{
		IntentResourceInfo:          {"DescribeCompShareInstance": {}},
		IntentMonitorQuery:          {"GetCompShareInstanceMonitor": {}},
		IntentMonitorHistory:        {"GetCompShareInstanceMonitor": {}},
		IntentGPUSpecsQuery:         {"DescribeAvailableCompShareInstanceTypes": {}},
		IntentStockAvailability:     {"DescribeAvailableCompShareInstanceTypes": {}, "DescribeCompShareSupportZone": {}, "DescribeCompShareGpuInventory": {}, "DescribeCompShareImages": {}, "CheckCompShareResourceCapacity": {}},
		IntentNetAcceleratorStatus:  {"CheckCompShareNetOptimizer": {}},
		IntentRefundEstimate:        {"GetCompShareRefundPrice": {}},
		IntentCFSInfo:               {"DescribeCFS": {}},
		IntentImageTagCatalog:       {"DescribeCompShareImageTags": {}},
		IntentModelRepositoryBrowse: {"DescribeModelRepositoryModels": {}, "DescribeModelRepositoryTags": {}},
		IntentPlatformImageList:     {"DescribeCompShareImages": {}},
		IntentCustomImageList:       {"DescribeCompShareCustomImages": {}},
		IntentCommunityImageList:    {"DescribeCommunityImages": {}},
		IntentSharedImageList:       {"DescribeCompShareSharingImages": {}},
		IntentPricingQuery:          {"GetCompShareInstanceUserPrice": {}},
	}
	got := handlerActionWhitelist()
	if !reflect.DeepEqual(got, golden) {
		t.Fatalf("handlerActionWhitelist drifted from golden set (security widening guard).\n got:    %v\n golden: %v", got, golden)
	}
}

// TestRoutingPromptFragments_ContainsAllIntents ensures every registered
// intent has both a directive AND a planner one-shot example. Missing either
// = planner LLM unaware of the intent enum → routing degrades silently.
func TestRoutingPromptFragments_ContainsAllIntents(t *testing.T) {
	directives, examples := RoutingPromptFragments()
	combined := strings.Join(append(append([]string{}, directives...), examples...), "\n")
	for _, i := range routingIntentOrder {
		if !strings.Contains(combined, string(i)) {
			t.Errorf("route fragments missing intent label %q (planner won't know to emit it)", i)
		}
	}
}

func TestRoutingPromptFragments_DeriveFromSkillRegistry(t *testing.T) {
	directives, examples := RoutingPromptFragments()
	combinedDirectives := strings.Join(directives, "\n")
	combinedExamples := strings.Join(examples, "\n")
	for _, meta := range skillRegistryRouteMetadata() {
		if len(meta.PlannerDirectives) == 0 {
			t.Fatalf("route %q must declare planner_directives in its skill", meta.Name)
		}
		if len(meta.PlannerExamples) == 0 {
			t.Fatalf("route %q must declare planner_examples in its skill", meta.Name)
		}
		for _, directive := range meta.PlannerDirectives {
			if !strings.Contains(combinedDirectives, directive) {
				t.Fatalf("planner directive for %q not emitted from metadata: %q", meta.Name, directive)
			}
		}
		for _, example := range meta.PlannerExamples {
			if !strings.Contains(combinedExamples, example.Question) {
				t.Fatalf("planner example question for %q not emitted from metadata: %q", meta.Name, example.Question)
			}
			if !strings.Contains(combinedExamples, meta.IntentLabel) {
				t.Fatalf("planner examples missing metadata intent %q", meta.IntentLabel)
			}
			if !strings.Contains(combinedExamples, meta.RequiredTool) {
				t.Fatalf("planner examples missing metadata required tool %q", meta.RequiredTool)
			}
		}
	}
}

func TestRouteMetadataRequiredToolsMatchSkillRegistry(t *testing.T) {
	byIntent := map[Intent]RouteMetadata{}
	for _, meta := range skillRegistryRouteMetadata() {
		byIntent[Intent(meta.IntentLabel)] = meta
	}
	for _, i := range routingIntentOrder {
		meta, ok := byIntent[i]
		if !ok {
			t.Fatalf("missing metadata for route intent %q", i)
		}
		want := skillRequiredTool(i)
		if meta.RequiredTool != want {
			t.Fatalf("metadata required_tool for %q = %q, skill registry has %q", i, meta.RequiredTool, want)
		}
	}
}

// TestDispatchRoute_RoutesToHandler verifies each handler is reachable via
// DispatchRoute. Uses a stub executor that fails fast so we only check
// handler routing, not full tool semantics.
type stubFailingExecutor struct{}

func (stubFailingExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

type routeSequenceExecutor struct {
	results       map[string]map[string]any
	errs          map[string]error
	calls         []handlerExecCall
	internalCalls int
}

func (m *routeSequenceExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, handlerExecCall{action: action, args: copyArgs(args)})
	if m.errs != nil {
		if err, ok := m.errs[action]; ok {
			return nil, err
		}
	}
	if m.results == nil {
		return map[string]any{}, nil
	}
	if result, ok := m.results[action]; ok {
		return result, nil
	}
	return map[string]any{}, nil
}

func (m *routeSequenceExecutor) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	m.internalCalls++
	return m.Execute(ctx, action, args)
}

func stockSupportZonesFixture() map[string]any {
	return map[string]any{"ZoneInfo": []any{
		map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(1), "Describe": "华北二A"},
		map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "RegionId": float64(3002), "ZoneId": float64(2), "Describe": "上海二B"},
	}}
}

func TestDispatchRoute_RoutesToHandler(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	for i := range routingIntentSet() {
		req := HandlerRequest{Plan: IntentRoute{Intent: i}}
		result := h.DispatchRoute(context.Background(), req)
		// With empty mock response, handlers should return a HandledResult
		// (their renderers produce "未获取到..." replies on empty data).
		if result.Status != HandlerStatusHandled {
			t.Errorf("DispatchRoute(%q) status = %q, want %q", i, result.Status, HandlerStatusHandled)
		}
		if want := skillRequiredTool(i); result.ToolAction != want {
			t.Errorf("DispatchRoute(%q) ToolAction = %q, want %q", i, result.ToolAction, want)
		}
	}
}

type stockCapacityZoneExecutor struct {
	calls []handlerExecCall
}

func (m *stockCapacityZoneExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, handlerExecCall{action: action, args: copyArgs(args)})
	switch action {
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal"},
			map[string]any{"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal"},
		}}, nil
	case "DescribeCompShareSupportZone":
		return stockSupportZonesFixture(), nil
	case "DescribeCompShareGpuInventory":
		return map[string]any{"GpuInventory": map[string]any{"Exclusive": map[string]any{
			"1": map[string]any{"4090": float64(0)},
			"2": map[string]any{"4090": float64(0)},
		}}}, nil
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
		}}, nil
	case "CheckCompShareResourceCapacity":
		if args["Zone"] == "cn-sh2-02" {
			return nil, errors.New("Params [Zone] not available")
		}
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
		}}, nil
	default:
		return map[string]any{}, nil
	}
}

type stockCapacityFallbackExecutor struct {
	calls []handlerExecCall
}

func (m *stockCapacityFallbackExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, handlerExecCall{action: action, args: copyArgs(args)})
	switch action {
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal"},
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal"},
		}}, nil
	case "DescribeCompShareSupportZone":
		return stockSupportZonesFixture(), nil
	case "DescribeCompShareGpuInventory":
		return map[string]any{"GpuInventory": map[string]any{"Exclusive": map[string]any{
			"1": map[string]any{"4090": float64(0)},
			"2": map[string]any{"4090": float64(0)},
		}}}, nil
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
		}}, nil
	case "CheckCompShareResourceCapacity":
		if args["Zone"] == "cn-sh2-02" {
			return nil, errors.New("Params [Zone] not available")
		}
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
		}}, nil
	default:
		return map[string]any{}, nil
	}
}

// TestDispatchRoute_UnknownIntentFalls verifies that calling
// DispatchRoute with a non-registered intent returns a FallbackBeforeTool
// (defensive layer; engine.go gates on IsRoutingIntent before invoking).
func TestDispatchRoute_UnknownIntentFalls(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	req := HandlerRequest{Plan: IntentRoute{Intent: Intent("not_a_route")}}
	result := h.DispatchRoute(context.Background(), req)
	if result.Status != HandlerStatusFallbackBeforeTool {
		t.Errorf("unknown-intent dispatch status = %q, want %q", result.Status, HandlerStatusFallbackBeforeTool)
	}
}

// TestRouteMetadata_LoadedFromSkillRegistry verifies the skill-registry
// projection produced one metadata entry per route intent and that none
// declares required_citation (routes are NOT cited per PR A spec).
func TestRouteMetadata_LoadedFromSkillRegistry(t *testing.T) {
	meta := skillRegistryRouteMetadata()
	if got, want := len(meta), len(routingIntentOrder); got != want {
		t.Fatalf("skillRegistryRouteMetadata count = %d, want %d (routingIntentOrder size)", got, want)
	}
	order := map[Intent]struct{}{}
	for _, i := range routingIntentOrder {
		order[i] = struct{}{}
	}
	for _, m := range meta {
		if _, ok := order[Intent(m.IntentLabel)]; !ok {
			t.Errorf("route metadata has intent_label %q not in routingIntentOrder", m.IntentLabel)
		}
		if m.RequiredCitation {
			t.Errorf("route %q has required_citation=true; routes are NOT cited per PR A spec", m.Name)
		}
	}
}

// ----- L0 deterministic NL filter tests (PR A round 2) ----------------------

func TestExtractUserTokens_StripsStopwordsAndShortRunes(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		// Pure-numeric tokens ("4090", "12", "2022") are intentionally dropped
		// here — extractUserTokens is used by image renderers (substring match
		// against image names), where short numerics would produce false
		// positives like "Debian 12" -> "py312". GPU/stock paths use
		// matchUserTokensToAPINames on the raw user text instead.
		{"4090 显存多大", nil},
		{"A100 支持几张卡", []string{"a100"}},
		{"查询平台镜像列表", nil},
		{"Ubuntu 22.04 镜像有吗", []string{"ubuntu", "22.04"}},
		{"Debian 12 镜像有吗", []string{"debian"}},
		{"Windows 2022", []string{"windows"}},
		{"", nil},
		// Q10 modifier stop-list: image-category words must not survive as
		// the sole remaining token, otherwise isImageListAllIntent's empty-
		// token guard mis-fires and the keyword filter rejects every match.
		// Each of these phrasings should collapse to empty tokens so that
		// list-all detection runs and the renderer returns the full set.
		{"我的自定义镜像有哪些", nil},
		{"自定义镜像列表", nil},
		{"私有镜像有哪些", nil},
		{"公共镜像列表", nil},
		{"共享镜像有哪些", nil},
	}
	for _, c := range cases {
		got := extractUserTokens(c.text)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("extractUserTokens(%q) = %v, want %v", c.text, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("extractUserTokens(%q)[%d] = %q, want %q", c.text, i, got[i], c.want[i])
			}
		}
	}
}

func TestDetectKnownUnavailableGPUs(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"上海机房 H100 库存", []string{"H100"}},
		{"h100 显存多大", []string{"H100"}}, // case-insensitive
		{"H200 有货吗", []string{"H200"}},
		{"4090 显存", nil}, // 4090 is available, not in the "known unavailable" list
		{"5090 显存", nil},
		// Word-boundary symmetry: "H10" or "H20" as user text should NOT match
		// "H100"/"H200" if those entries were ever shortened — guard against the
		// same substring trap matchUserTokensToAPINames fixed.
		{"H10 是什么", nil},
		{"H20 库存", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := detectKnownUnavailableGPUs(c.text)
		if len(got) != len(c.want) {
			t.Errorf("detectKnownUnavailableGPUs(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestMatchUserTokensToAPINames_Subset(t *testing.T) {
	// The API drives the matching vocabulary — no hand-maintained GPU dictionary.
	apiNames := []string{"4090", "4090_48G", "5090", "A100", "A800", "V100S", "H20"}
	cases := []struct {
		text string
		want []string
	}{
		{"4090 显存多大", []string{"4090"}}, // user mentioned "4090" -> exact; "4090_48G" is a different model
		{"a100 几张卡", []string{"A100"}},  // case-insensitive
		{"v100s 配置", []string{"V100S"}},
		{"H100 库存", nil}, // H100 not in API set — caller handles via known-unavailable
		// Word-boundary regression: "H20" must NOT substring-match inside "H200".
		{"你们有 H200 96G 这种规格吗", nil},
		{"H200 还有货吗", nil},
		// "H20" as a standalone token still matches.
		{"H20 还有货吗", []string{"H20"}},
		// "4090_48G" requires the exact suffix in user text (underscore is a word char).
		{"4090_48G 多少钱", []string{"4090_48G"}},
		{"未指定", nil},
	}
	for _, c := range cases {
		got := matchUserTokensToAPINames(c.text, apiNames)
		if len(got) != len(c.want) {
			t.Errorf("matchUserTokensToAPINames(%q) = %v, want %v", c.text, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("matchUserTokensToAPINames(%q)[%d] = %q, want %q", c.text, i, got[i], c.want[i])
			}
		}
	}
}

func TestMatchUserTextToInstanceTypeNames_FailsClosedForUnknownGPU(t *testing.T) {
	// Regression for the H200→H20 confusion: "H200 96G" must NOT fall back to
	// memory-only matching when no API name matched. Otherwise the caller
	// surfaces H20_96G (or similar same-memory variant) as a confident answer.
	items := []any{
		map[string]any{"Name": "H20", "GraphicsMemory": "96"},
		map[string]any{"Name": "4090", "GraphicsMemory": "24"},
		map[string]any{"Name": "4090_48G", "GraphicsMemory": "48"},
	}
	cases := []struct {
		text                       string
		includeFamilyMemoryVariant bool
		want                       []string
	}{
		{"你们有 H200 96G 这种规格吗", false, nil},
		{"你们有 H200 96G 这种规格吗", true, nil},
		{"H200 还有货吗", false, nil},
		// Legitimate 4090 + 48G expansion still works.
		{"4090 48G 多少钱", true, []string{"4090_48G"}},
	}
	for _, c := range cases {
		got := matchUserTextToInstanceTypeNames(c.text, items, c.includeFamilyMemoryVariant)
		if len(got) != len(c.want) {
			t.Errorf("matchUserTextToInstanceTypeNames(%q, includeFamily=%v) = %v, want %v", c.text, c.includeFamilyMemoryVariant, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("matchUserTextToInstanceTypeNames(%q)[%d] = %q, want %q", c.text, i, got[i], c.want[i])
			}
		}
	}
}

func TestContainsAsWord_BoundaryCases(t *testing.T) {
	cases := []struct {
		hay, needle string
		want        bool
	}{
		{"H200", "H20", false},
		{"H200 96G", "H20", false},
		{"你们有 H20 还有货吗", "H20", true},
		{"H20 96G", "H20", true},
		// Underscore is a word char → boundary fails inside.
		{"4090_48G", "4090", false},
		{"4090 48G", "4090", true},
		{"我想要 4090_48G", "4090_48G", true},
		{"H20", "H20", true},
		{"H20 ", "H20", true},
		{" H20", "H20", true},
		{"anything", "", false},
	}
	for _, c := range cases {
		got := containsAsWord(c.hay, c.needle)
		if got != c.want {
			t.Errorf("containsAsWord(%q, %q) = %v, want %v", c.hay, c.needle, got, c.want)
		}
	}
}

func TestUserMentionedGPULikeToken(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"4090 显存多大", true},
		{"A100 几张卡", true},
		{"H100 库存", true},
		{"5070 有货吗", true}, // not in API but GPU-shaped
		{"查询社区镜像", false},
		{"查询自制镜像", false},
		{"Ubuntu 镜像有吗", false}, // no digit-heavy GPU shape (Ubuntu alone)
	}
	for _, c := range cases {
		got := userMentionedGPULikeToken(c.text)
		if got != c.want {
			t.Errorf("userMentionedGPULikeToken(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestRenderGPUSpecs_FilterToMentionedModel(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name":           "4090",
				"GraphicsMemory": map[string]any{"Value": 24},
				"Status":         "Normal",
			},
			map[string]any{
				"Name":           "A100",
				"GraphicsMemory": map[string]any{"Value": 80},
				"Status":         "Normal",
			},
		},
	}
	reply := renderGPUSpecsReply(raw, "A100 支持几张卡")
	if strings.Contains(reply, "机型=4090") {
		t.Errorf("filter should exclude 4090 when user asked A100; got: %s", reply)
	}
	if !strings.Contains(reply, "机型=A100") {
		t.Errorf("filter should keep A100 when user asked A100; got: %s", reply)
	}
}

func TestRenderGPUSpecs_OverviewDoesNotExpandEveryMachineSize(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name":           "4090",
				"GraphicsMemory": map[string]any{"Value": 24},
				"Performance":    map[string]any{"Value": 83},
				"Status":         "Normal",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(94)}},
							map[string]any{"Cpu": float64(24), "Memory": []any{float64(96)}},
						},
					},
					map[string]any{
						"Gpu": float64(2),
						"Collection": []any{
							map[string]any{"Cpu": float64(32), "Memory": []any{float64(128), float64(192)}},
						},
					},
				},
			},
		},
	}

	reply := renderGPUSpecsReply(raw, "4090 显存多大")

	if !strings.Contains(reply, "机型=4090") || !strings.Contains(reply, "显存=24GB") {
		t.Fatalf("overview should include basic GPU facts, got: %s", reply)
	}
	for _, notWant := range []string{"16C/64G", "16C/94G", "24C/96G", "32C/128G", "32C/192G"} {
		if strings.Contains(reply, notWant) {
			t.Fatalf("overview query should not expand full machine-size combos; found %q in: %s", notWant, reply)
		}
	}
}

func TestRenderGPUSpecs_FullModelRequestExpandsEveryMachineSize(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name":           "4090",
				"Zone":           "cn-wlcb-01",
				"GraphicsMemory": map[string]any{"Value": 24},
				"Performance":    map[string]any{"Value": 83},
				"Status":         "Normal",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(94)}},
							map[string]any{"Cpu": float64(24), "Memory": []any{float64(96)}},
						},
					},
					map[string]any{
						"Gpu": float64(2),
						"Collection": []any{
							map[string]any{"Cpu": float64(32), "Memory": []any{float64(128), float64(192)}},
						},
					},
				},
			},
			map[string]any{
				"Name":           "A100",
				"GraphicsMemory": map[string]any{"Value": 80},
				"Status":         "Normal",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(20), "Memory": []any{float64(160)}},
						},
					},
				},
			},
		},
	}

	reply := renderGPUSpecsReply(raw, "4090 的所有规格")

	for _, want := range []string{"16C/64G", "16C/94G", "24C/96G", "32C/128G", "32C/192G"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("full specs should include %q, got: %s", want, reply)
		}
	}
	if strings.Contains(reply, "A100") {
		t.Fatalf("full model request should still filter unrelated GPU models, got: %s", reply)
	}
}

func TestRenderGPUSpecs_CPUAndMemoryQuestionExpandsEveryMachineSize(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name":           "4090",
				"GraphicsMemory": map[string]any{"Value": 24},
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(94)}},
							map[string]any{"Cpu": float64(24), "Memory": []any{float64(96)}},
						},
					},
				},
			},
		},
	}

	reply := renderGPUSpecsReply(raw, "4090 支持哪些 CPU 和内存")

	for _, want := range []string{"16C/64G", "16C/94G", "24C/96G"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("CPU/memory wording should expand %q, got: %s", want, reply)
		}
	}
}

func TestRenderGPUSpecs_FullAllRequestExpandsAllModels(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name":           "4090",
				"GraphicsMemory": map[string]any{"Value": 24},
				"Status":         "Normal",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(94)}},
						},
					},
				},
			},
			map[string]any{
				"Name":           "A100",
				"GraphicsMemory": map[string]any{"Value": 80},
				"Status":         "Normal",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(20), "Memory": []any{float64(160)}},
						},
					},
				},
			},
		},
	}

	reply := renderGPUSpecsReply(raw, "列出所有 GPU 规格")

	for _, want := range []string{"机型=4090", "16C/64G", "16C/94G", "机型=A100", "20C/160G"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("full all-specs request should include %q, got: %s", want, reply)
		}
	}
}

func TestGPUSpecsRouteUsesDescribeAvailableAndExpandsFullRequest(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{
					"Name":           "4090",
					"GraphicsMemory": map[string]any{"Value": 24},
					"MachineSizes": []any{
						map[string]any{
							"Gpu": float64(1),
							"Collection": []any{
								map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(94)}},
							},
						},
					},
				},
			},
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentGPUSpecsQuery},
		UserText: "4090 的所有规格",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if len(exec.calls) != 1 || exec.calls[0].action != "DescribeAvailableCompShareInstanceTypes" {
		t.Fatalf("calls = %#v, want one DescribeAvailableCompShareInstanceTypes call", exec.calls)
	}
	if len(exec.calls[0].args) != 0 {
		t.Fatalf("gpu specs route should query full upstream data without narrowing args, got %#v", exec.calls[0].args)
	}
	if result.Envelope == nil {
		t.Fatal("gpu specs route should attach a renderer envelope")
	}
	if result.Envelope.Kind != "gpu_specs_query" {
		t.Fatalf("envelope kind = %q, want gpu_specs_query", result.Envelope.Kind)
	}
	if len(result.RendererInputEnvelopeHashes) != 1 {
		t.Fatalf("renderer envelope hashes = %#v, want one hash", result.RendererInputEnvelopeHashes)
	}
	for _, want := range []string{"机型=4090", "16C/64G", "16C/94G"} {
		if !strings.Contains(result.Reply, want) {
			t.Fatalf("full route reply should include %q, got: %s", want, result.Reply)
		}
	}
}

func TestRenderGPUSpecs_IncludesMemoryVariantForFamilyQuestion(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "GraphicsMemory": map[string]any{"Value": 24}, "Status": "Normal"},
			map[string]any{"Name": "4090_48G", "GraphicsMemory": map[string]any{"Value": 48}, "Status": "Normal"},
			map[string]any{"Name": "A100", "GraphicsMemory": map[string]any{"Value": 80}, "Status": "Normal"},
		},
	}

	reply := renderGPUSpecsReply(raw, "4090有哪些规格")

	if !strings.Contains(reply, "机型=4090,") {
		t.Errorf("family question should include base 4090; got: %s", reply)
	}
	if !strings.Contains(reply, "机型=4090_48G") {
		t.Errorf("family question should include 4090_48G variant; got: %s", reply)
	}
	if strings.Contains(reply, "机型=A100") {
		t.Errorf("family question should not include unrelated models; got: %s", reply)
	}
}

func TestRenderGPUSpecs_MemoryHintMatchesMemoryVariant(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "GraphicsMemory": map[string]any{"Value": 24}, "Status": "Normal"},
			map[string]any{"Name": "4090_48G", "GraphicsMemory": map[string]any{"Value": 48}, "Status": "Normal"},
		},
	}

	reply := renderGPUSpecsReply(raw, "是否有48G的4090")

	if strings.Contains(reply, "机型=4090,") {
		t.Errorf("48G question should not answer with plain 4090; got: %s", reply)
	}
	if !strings.Contains(reply, "机型=4090_48G") {
		t.Errorf("48G question should include 4090_48G; got: %s", reply)
	}
}

func TestRenderGPUSpecs_MemoryHintWithoutMatchDoesNotFallBackToBase(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "GraphicsMemory": map[string]any{"Value": 24}, "Status": "Normal"},
		},
	}

	reply := renderGPUSpecsReply(raw, "有没有128G的4090")

	if !strings.Contains(reply, "未在当前可售机型里找到") {
		t.Errorf("unavailable memory variant should be reported as not found; got: %s", reply)
	}
}

func TestRenderGPUSpecs_KnownUnavailableFallback(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "GraphicsMemory": map[string]any{"Value": 24}},
		},
	}
	reply := renderGPUSpecsReply(raw, "上海机房 H100 库存")
	if !strings.Contains(reply, "H100") {
		t.Errorf("known-unavailable reply should mention H100; got: %s", reply)
	}
	if !strings.Contains(reply, "未在 CompShare 平台提供") {
		t.Errorf("known-unavailable reply should explain not provided; got: %s", reply)
	}
}

func TestRenderGPUSpecs_GPULikeButNoMatchFallback(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "GraphicsMemory": map[string]any{"Value": 24}},
		},
	}
	reply := renderGPUSpecsReply(raw, "5070 显存多大") // 5070 doesn't exist + not in known-unavailable
	if !strings.Contains(reply, "未在当前可售机型里找到") {
		t.Errorf("not-found fallback should explain; got: %s", reply)
	}
	if !strings.Contains(reply, "机型=4090") {
		t.Errorf("not-found fallback should still show available list; got: %s", reply)
	}
}

func TestRenderStock_FilterAndDedupe(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Status": "Normal"},
			map[string]any{"Name": "4090", "Status": "Normal"}, // duplicate across zones
			map[string]any{"Name": "A100", "Status": "Normal"},
		},
	}
	reply := renderStockReply(raw, "4090 有货吗")
	if strings.Contains(reply, "A100") {
		t.Errorf("stock filter should exclude A100; got: %s", reply)
	}
	if c := strings.Count(reply, "机型=4090"); c != 1 {
		t.Errorf("stock filter should dedupe 4090 to 1 line, got %d in: %s", c, reply)
	}
}

func TestRenderStock_MemoryHintMatchesMemoryVariant(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "GraphicsMemory": map[string]any{"Value": 24}, "Status": "Normal"},
			map[string]any{"Name": "4090_48G", "GraphicsMemory": map[string]any{"Value": 48}, "Status": "Normal"},
		},
	}

	reply := renderStockReply(raw, "是否有48G的4090")

	if strings.Contains(reply, "机型=4090,") {
		t.Errorf("48G stock question should not answer with plain 4090; got: %s", reply)
	}
	if !strings.Contains(reply, "机型=4090_48G") {
		t.Errorf("48G stock question should include 4090_48G; got: %s", reply)
	}
}

func TestRenderStock_NormalStatusDoesNotClaimConcreteCapacity(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Status": "Normal"},
		},
	}

	reply := renderStockReply(raw, "4090 现在有没有货")

	if !strings.Contains(reply, "不代表当前具体配置一定可创建") {
		t.Errorf("Normal stock reply should explain capacity caveat; got: %s", reply)
	}
	if !strings.Contains(reply, "容量预检") {
		t.Errorf("Normal stock reply should point to capacity precheck; got: %s", reply)
	}
}

func TestStockAvailabilityUsesCapacityPrecheckForMentionedNormalGPU(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal"},
			},
		},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareGpuInventory": {
			"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"1": map[string]any{"4090": float64(0)},
			}},
		},
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
			},
		},
		"CheckCompShareResourceCapacity": {
			"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
			},
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "4090 现在有没有货",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if !strings.Contains(result.Reply, "默认创建配置暂未通过容量预检") || !strings.Contains(result.Reply, "机型状态：开售") {
		t.Fatalf("reply should answer concrete creatability, got: %s", result.Reply)
	}
	if strings.Contains(result.Reply, "ResourceEnough") || strings.Contains(result.Reply, "容量预检口径") {
		t.Fatalf("reply should not expose implementation details, got: %s", result.Reply)
	}
	if len(exec.calls) != 5 {
		t.Fatalf("calls = %#v, want 5 calls", exec.calls)
	}
	if exec.calls[0].action != "DescribeAvailableCompShareInstanceTypes" ||
		exec.calls[1].action != "DescribeCompShareSupportZone" ||
		exec.calls[2].action != "DescribeCompShareGpuInventory" ||
		exec.calls[3].action != "DescribeCompShareImages" ||
		exec.calls[4].action != "CheckCompShareResourceCapacity" {
		t.Fatalf("unexpected call sequence: %#v", exec.calls)
	}
	args := exec.calls[4].args
	if args["GpuType"] != "4090" {
		t.Fatalf("capacity GpuType = %#v, want 4090", args["GpuType"])
	}
	if args["Zone"] != "cn-wlcb-01" {
		t.Fatalf("capacity Zone = %#v, want cn-wlcb-01", args["Zone"])
	}
	if args["Region"] != "cn-wlcb" {
		t.Fatalf("capacity Region = %#v, want cn-wlcb", args["Region"])
	}
	if args["zone_id"] != uint32(1) {
		t.Fatalf("capacity zone_id = %#v, want 1", args["zone_id"])
	}
	if args["CompShareImageId"] != "img-ubuntu" {
		t.Fatalf("capacity CompShareImageId = %#v, want img-ubuntu", args["CompShareImageId"])
	}
	if args["ChargeType"] != "Postpay" {
		t.Fatalf("capacity ChargeType = %#v, want Postpay", args["ChargeType"]) // 按量 = Postpay (Dynamic retired, #246)
	}
}

func TestStockAvailabilityReportsRawGPUInventoryAndCapacitySeparately(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "2080Ti", "Zone": "cn-sh2-02", "Status": "Normal"},
			},
		},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareGpuInventory": {
			"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"2": map[string]any{"2080Ti": float64(3)},
			}},
		},
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
			},
		},
		"CheckCompShareResourceCapacity": {
			"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
			},
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "2080ti有库存吗",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	for _, want := range []string{
		"机型状态：开售",
		"上海二B 库存约 3 张 GPU",
		"默认创建配置暂未通过容量预检",
	} {
		if !strings.Contains(result.Reply, want) {
			t.Fatalf("reply missing %q: %s", want, result.Reply)
		}
	}
	if strings.Contains(result.Reply, "本次容量预检未能确认具体配置的可创建性") {
		t.Fatalf("reply should not collapse raw inventory into old generic fallback: %s", result.Reply)
	}
	if len(exec.calls) != 5 {
		t.Fatalf("calls = %#v, want 5 calls", exec.calls)
	}
	if exec.calls[2].action != "DescribeCompShareGpuInventory" {
		t.Fatalf("third call = %#v, want DescribeCompShareGpuInventory", exec.calls[2])
	}
}

func TestStockAvailabilityFiltersByLiveZoneDescribe(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal"},
				map[string]any{"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal"},
			},
		},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareGpuInventory": {
			"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"1": map[string]any{"4090": float64(0)},
				"2": map[string]any{"4090": float64(5)},
			}},
		},
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
			},
		},
		"CheckCompShareResourceCapacity": {
			"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			},
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "上海有4090库存吗",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if !strings.Contains(result.Reply, "上海二B 库存约 5 张 GPU") {
		t.Fatalf("reply should use live Describe zone label and count, got: %s", result.Reply)
	}
	if !strings.Contains(result.Reply, "默认创建配置已通过容量预检，可以新建实例") {
		t.Fatalf("reply should state positive capacity verdict, got: %s", result.Reply)
	}
	if strings.Contains(result.Reply, "华北二A") || strings.Contains(result.Reply, "cn-wlcb-01") {
		t.Fatalf("reply should be narrowed to the requested Shanghai zone, got: %s", result.Reply)
	}
	if got := exec.calls[len(exec.calls)-1].args["Zone"]; got != "cn-sh2-02" {
		t.Fatalf("capacity precheck zone = %#v, want cn-sh2-02", got)
	}
	if got := exec.calls[len(exec.calls)-1].args["Region"]; got != "cn-sh2" {
		t.Fatalf("capacity precheck Region = %#v, want cn-sh2", got)
	}
	if got := exec.calls[len(exec.calls)-1].args["zone_id"]; got != uint32(2) {
		t.Fatalf("capacity precheck zone_id = %#v, want 2", got)
	}
	if exec.internalCalls == 0 {
		t.Fatalf("capacity precheck must use internal executor so backend-derived zone_id is not filtered")
	}
}

func TestStockAvailabilityMissingInventoryKeyIsUnknownNotZero(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "V100S", "Zone": "cn-wlcb-01", "Status": "Normal"},
			},
		},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareGpuInventory": {
			"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"1": map[string]any{"4090": float64(2)},
			}},
		},
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
			},
		},
		"CheckCompShareResourceCapacity": {
			"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(10), "Mem": float64(64), "ResourceEnough": true},
			},
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "v100有库存吗",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if !strings.Contains(result.Reply, "原始 GPU 库存：华北二A 接口未返回 V100S 的库存数量") {
		t.Fatalf("missing inventory key should be unknown, got: %s", result.Reply)
	}
	if strings.Contains(result.Reply, "库存约 0 张 GPU") || strings.Contains(result.Reply, "原始 GPU 库存：暂无") {
		t.Fatalf("missing inventory key must not be rendered as zero stock: %s", result.Reply)
	}
}

func TestStockAvailabilityMissingSupportZoneMappingIsUnknownNotZero(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "2080Ti", "Zone": "cn-sh2-02", "Status": "Normal"},
			},
		},
		"DescribeCompShareSupportZone": {
			"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(1), "Describe": "华北二A"},
			},
		},
		"DescribeCompShareGpuInventory": {
			"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"2": map[string]any{"2080Ti": float64(3)},
			}},
		},
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
			},
		},
		"CheckCompShareResourceCapacity": {
			"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
			},
		},
	}}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "2080ti有库存吗",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if !strings.Contains(result.Reply, "cn-sh2-02 接口未返回 2080Ti 的库存数量") {
		t.Fatalf("missing support-zone mapping should be unknown, got: %s", result.Reply)
	}
	if strings.Contains(result.Reply, "库存约 0 张 GPU") || strings.Contains(result.Reply, "zone_id=") {
		t.Fatalf("missing support-zone mapping must not render zero stock or internal zone_id: %s", result.Reply)
	}
}

func TestStockZoneFilterIgnoresAmbiguousShortChineseRegionPrefix(t *testing.T) {
	supportZones := []zones.ZoneInfo{
		{Zone: "cn-wlcb-01", Describe: "华北二A"},
		{Zone: "cn-bj2-03", Describe: "华北一C"},
		{Zone: "cn-sh2-02", Describe: "上海二B"},
	}

	if got := stockZoneFilterFromText("华北有4090库存吗", supportZones); got != nil {
		t.Fatalf("ambiguous short prefix should not narrow zones, got: %#v", got)
	}
	if got := stockZoneFilterFromText("华北二有4090库存吗", supportZones); len(got) != 1 {
		t.Fatalf("unique longer prefix should narrow one zone, got: %#v", got)
	} else if _, ok := got["cn-wlcb-01"]; !ok {
		t.Fatalf("unique longer prefix should resolve cn-wlcb-01, got: %#v", got)
	}
	if got := stockZoneFilterFromText("上海有4090库存吗", supportZones); len(got) != 1 {
		t.Fatalf("unique two-rune prefix should still resolve when unambiguous, got: %#v", got)
	} else if _, ok := got["cn-sh2-02"]; !ok {
		t.Fatalf("unique Shanghai prefix should resolve cn-sh2-02, got: %#v", got)
	}
}

func TestRenderStockCapacityReply_PrecheckFailureFallsBackToCatalogOpen(t *testing.T) {
	// #3b: every model reaching renderStockCapacityReply came from
	// matchedNormalStockEntries, so it is catalog-Normal (机型开售) by construction.
	// When every zone's capacity precheck fails (e.g. RetCode 230 with an empty
	// CLI project_id, or RetCode 292 when HTTP omits ProjectId) the reply must
	// surface that 开售 truth — NOT collapse to "无法确认是否有可创建库存", which wrongly
	// implies we can't even tell it is on sale, and NOT claim it is sold out.
	reply := renderStockCapacityReply([]stockCapacityCheck{
		{Name: "V100S", Zone: "cn-wlcb-01", Failed: true},
	})
	if !strings.Contains(reply, "开售") {
		t.Errorf("failed-precheck reply should surface the catalog 开售 truth; got: %s", reply)
	}
	if strings.Contains(reply, "无法确认是否有可创建库存") {
		t.Errorf("failed-precheck reply must not bury the catalog truth under 无法确认; got: %s", reply)
	}
	if strings.Contains(reply, "暂无可创建库存") {
		t.Errorf("a failed precheck is not the same as sold-out; got: %s", reply)
	}
}

func TestStockAvailabilityUsesFirstMatchedZoneForCapacityPrecheck(t *testing.T) {
	exec := &stockCapacityZoneExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "4090 现在有没有货",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if !strings.Contains(result.Reply, "默认创建配置暂未通过容量预检") || !strings.Contains(result.Reply, "机型状态：开售") {
		t.Fatalf("reply should still answer from successful capacity checks, got: %s", result.Reply)
	}
	if strings.Contains(result.Reply, "部分可用区容量预检未完成") {
		t.Fatalf("reply should not expose unprobed zones, got: %s", result.Reply)
	}
	if len(exec.calls) != 5 {
		t.Fatalf("calls = %#v, want 5 calls", exec.calls)
	}
}

func TestStockAvailabilityFallsBackToNextZoneWhenCapacityCheckFails(t *testing.T) {
	exec := &stockCapacityFallbackExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan:     IntentRoute{Intent: IntentStockAvailability},
		UserText: "4090 鐜板湪鏈夋病鏈夎揣",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if len(exec.calls) != 6 {
		t.Fatalf("calls = %#v, want fallback capacity call in second zone", exec.calls)
	}
	if exec.calls[4].action != "CheckCompShareResourceCapacity" || exec.calls[4].args["Zone"] != "cn-sh2-02" {
		t.Fatalf("first capacity call = %#v, want cn-sh2-02", exec.calls[4])
	}
	if exec.calls[5].action != "CheckCompShareResourceCapacity" || exec.calls[5].args["Zone"] != "cn-wlcb-01" {
		t.Fatalf("fallback capacity call = %#v, want cn-wlcb-01", exec.calls[5])
	}
}

func TestRenderImageList_KeywordFilter(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "ImageName": "Ubuntu 22.04 LTS", "ImageType": "System"},
			map[string]any{"CompShareImageId": "img-2", "ImageName": "PyTorch 2.1", "ImageType": "App"},
			map[string]any{"CompShareImageId": "img-3", "ImageName": "CentOS 7", "ImageType": "System"},
		},
	}
	reply := renderImageListReply(raw, "ImageSet",
		[]string{"CompShareImageId", "ImageName", "ImageType"},
		"Ubuntu 22.04 镜像有吗")
	if strings.Contains(reply, "CentOS") || strings.Contains(reply, "PyTorch") {
		t.Errorf("image filter should exclude non-Ubuntu; got: %s", reply)
	}
	if !strings.Contains(reply, "Ubuntu 22.04 LTS") {
		t.Errorf("image filter should keep Ubuntu match; got: %s", reply)
	}
}

func TestRenderImageList_PyTorchMatchesTorchNamedImages(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04 64位", "ImageType": "System"},
			map[string]any{"CompShareImageId": "img-torch", "Name": "cuda128_torch291_py312", "ImageType": "App"},
			map[string]any{"CompShareImageId": "img-vllm", "Name": "vLLM v0.12.0", "ImageType": "App"},
		},
	}
	reply := renderImageListReply(raw, "ImageSet",
		[]string{"CompShareImageId", "Name", "ImageType"},
		"有哪些 PyTorch 镜像")

	if !strings.Contains(reply, "cuda128_torch291_py312") {
		t.Errorf("PyTorch query should match torch/cuda image names; got: %s", reply)
	}
	if strings.Contains(reply, "Ubuntu") || strings.Contains(reply, "vLLM") {
		t.Errorf("PyTorch query should not include unrelated images; got: %s", reply)
	}
}

func TestRenderImageList_NoMatchFallback(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "ImageName": "Ubuntu 22.04 LTS"},
		},
	}
	reply := renderImageListReply(raw, "ImageSet",
		[]string{"CompShareImageId", "ImageName"},
		"Debian 12 镜像有吗")
	if !strings.Contains(reply, "未找到匹配的镜像") {
		t.Errorf("no-match should produce explicit not-found reply; got: %s", reply)
	}
}

func TestRenderCommunityImage_DigitalHumanQueryMatchesRelevantGroups(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"ImageName":    "LiveTalking",
			"CreatedCount": float64(200),
			"Data": []any{
				map[string]any{"CompShareImageId": "live-1", "Name": "数字人实时对话版"},
			},
		},
		map[string]any{
			"ImageName":    "LTX-2.3视频生成合集！支持文生视频、图生视频、数字人视频等",
			"CreatedCount": float64(500),
			"Data": []any{
				map[string]any{"CompShareImageId": "ltx-1", "Name": "LTX-v1"},
			},
		},
		map[string]any{
			"ImageName":    "RAGFlow Ubuntu 22.04",
			"CreatedCount": float64(900),
			"Data": []any{
				map[string]any{"CompShareImageId": "rag-1", "Name": "RAGFlow-v1"},
			},
		},
	}}

	reply := renderCommunityImageReply(raw, "有哪些社区镜像适合数字人")

	if !strings.Contains(reply, "LiveTalking") || !strings.Contains(reply, "LTX-2.3") {
		t.Errorf("digital-human query should include relevant community image groups; got: %s", reply)
	}
	if strings.Contains(reply, "RAGFlow") {
		t.Errorf("digital-human query should not include unrelated community image groups; got: %s", reply)
	}
}

func TestRenderImageList_StopwordsOnlyShowsAll(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "ImageName": "Ubuntu 22.04 LTS"},
			map[string]any{"CompShareImageId": "img-2", "ImageName": "PyTorch 2.1"},
		},
	}
	reply := renderImageListReply(raw, "ImageSet",
		[]string{"CompShareImageId", "ImageName"},
		"查询平台镜像列表") // all tokens are stopwords -> no filter
	if !strings.Contains(reply, "Ubuntu") || !strings.Contains(reply, "PyTorch") {
		t.Errorf("stopwords-only query should show all images; got: %s", reply)
	}
}

func TestRenderImageTagCatalog_Categorized(t *testing.T) {
	raw := map[string]any{
		"TagIndex": []any{"框架", "场景"},
		"TagsMap": map[string]any{
			"框架": []any{"PyTorch", "TensorFlow"},
			"场景": []any{"LLM", "图像生成"},
		},
	}
	reply := renderImageTagCatalogReply(raw)
	for _, want := range []string{"镜像标签分类", "框架: PyTorch、TensorFlow", "场景: LLM、图像生成"} {
		if !strings.Contains(reply, want) {
			t.Errorf("image tag reply missing %q: %s", want, reply)
		}
	}
}

func TestRenderImageTagCatalog_FlatFallback(t *testing.T) {
	raw := map[string]any{
		"Tags": []any{"深度学习", "ComfyUI", "Stable Diffusion"},
	}
	reply := renderImageTagCatalogReply(raw)
	for _, want := range []string{"镜像标签", "深度学习", "ComfyUI"} {
		if !strings.Contains(reply, want) {
			t.Errorf("flat tag reply missing %q: %s", want, reply)
		}
	}
}

func TestRenderImageTagCatalog_Empty(t *testing.T) {
	reply := renderImageTagCatalogReply(map[string]any{})
	if !strings.Contains(reply, "未获取到镜像标签") {
		t.Errorf("empty tag reply should be explicit not-found; got: %s", reply)
	}
}

func TestModelRepositoryArgs_MatchesTag(t *testing.T) {
	tagRaw := map[string]any{"Tags": []any{"LLM", "图像生成"}}
	args := modelRepositoryArgsFromUserText("有哪些 LLM 模型", tagRaw)
	if args["tags"] != "LLM" {
		t.Fatalf("model repository args = %#v, want tags=LLM", args)
	}
	if _, ok := args["name"]; ok {
		t.Fatalf("tag query should not also set name; got %#v", args)
	}
}

func TestModelRepositoryArgs_MatchesModelName(t *testing.T) {
	args := modelRepositoryArgsFromUserText("Qwen 模型仓库里有吗", map[string]any{"Tags": []any{"LLM"}})
	if args["name"] != "qwen" {
		t.Fatalf("model repository args = %#v, want name=qwen", args)
	}
}

func TestRenderModelRepositoryReply_ListAll(t *testing.T) {
	modelRaw := map[string]any{"Models": []any{
		map[string]any{"Name": "Qwen2.5-7B", "Path": "/models/qwen", "Tag": "LLM,Qwen", "Size": "15GB", "Deleted": float64(0)},
		map[string]any{"Name": "DeletedModel", "Path": "/models/deleted", "Tag": "LLM", "Size": "1GB", "Deleted": float64(1)},
	}}
	tagRaw := map[string]any{"Tags": []any{"LLM", "图像生成", "LLM"}}
	reply := renderModelRepositoryReply(modelRaw, tagRaw, "模型仓库里有哪些模型可以用")
	for _, want := range []string{"模型仓库标签", "LLM", "模型仓库列表", "Qwen2.5-7B", "/models/qwen"} {
		if !strings.Contains(reply, want) {
			t.Errorf("model repository reply missing %q: %s", want, reply)
		}
	}
	if strings.Contains(reply, "LLM、图像生成、LLM") {
		t.Errorf("model repository tags should be de-duplicated: %s", reply)
	}
	if strings.Contains(reply, "DeletedModel") {
		t.Errorf("deleted model should not render: %s", reply)
	}
	// A found listing must bridge to the two real follow-ups (user asked for this):
	// deploy a pre-downloaded repo model, or self-pull a model the repo lacks.
	for _, want := range []string{"无需重新下载", "想部署", "自行拉取"} {
		if !strings.Contains(reply, want) {
			t.Errorf("found listing missing deploy/self-pull guidance %q: %s", want, reply)
		}
	}
}

func TestRenderModelRepositoryReply_NameFilterNoMatch(t *testing.T) {
	modelRaw := map[string]any{"Models": []any{
		map[string]any{"Name": "Qwen2.5-7B", "Path": "/models/qwen", "Tag": "LLM", "Size": "15GB"},
	}}
	tagRaw := map[string]any{"Tags": []any{"LLM"}}
	reply := renderModelRepositoryReply(modelRaw, tagRaw, "llama 模型有吗")
	if !strings.Contains(reply, "未找到匹配的模型") {
		t.Errorf("name-filter no-match should be explicit; got: %s", reply)
	}
	// A repo miss must guide the user to self-pull rather than dead-end (the model
	// the user wants simply isn't pre-downloaded).
	for _, want := range []string{"自行拉取", "ollama pull"} {
		if !strings.Contains(reply, want) {
			t.Errorf("no-match reply missing self-pull guidance %q: %s", want, reply)
		}
	}
	// The pre-download note belongs only to a FOUND listing, not a miss.
	if strings.Contains(reply, "无需重新下载") {
		t.Errorf("no-match reply should not claim models are pre-downloaded: %s", reply)
	}
}

func TestRenderCommunityImage_DataExpansionAndCap(t *testing.T) {
	// One group with 5 versions: cap should keep first 3 + "... 共 5 个版本" hint.
	group := map[string]any{
		"Name":   "ComfyUI 镜像",
		"Author": "alice",
		"Data": []any{
			map[string]any{"CompShareImageId": "g1v1", "Name": "v0.3.66"},
			map[string]any{"CompShareImageId": "g1v2", "Name": "v0.3.65"},
			map[string]any{"CompShareImageId": "g1v3", "Name": "v0.3.64"},
			map[string]any{"CompShareImageId": "g1v4", "Name": "v0.3.63"},
			map[string]any{"CompShareImageId": "g1v5", "Name": "v0.3.62"},
		},
	}
	raw := map[string]any{"CompshareImageGroup": []any{group}}
	reply := renderCommunityImageReply(raw, "查询社区镜像")
	if !strings.Contains(reply, "名称=ComfyUI 镜像") {
		t.Errorf("community renderer should show group header; got: %s", reply)
	}
	for _, want := range []string{"v0.3.66", "v0.3.65", "v0.3.64"} {
		if !strings.Contains(reply, want) {
			t.Errorf("community renderer should show first 3 versions; missing %s in: %s", want, reply)
		}
	}
	if strings.Contains(reply, "v0.3.62") {
		t.Errorf("community renderer should cap at 3 versions per group; got: %s", reply)
	}
	if !strings.Contains(reply, "共 5 个版本") {
		t.Errorf("community renderer should add 'remaining N' hint when capped; got: %s", reply)
	}
	// Footer bridges the list to the deploy flow so users can hand an image straight
	// to deploy_model — the live alternative to ingesting per-image READMEs into RAG.
	if !strings.Contains(reply, "我来帮你选 GPU 配置并创建") {
		t.Errorf("community renderer should append the deploy-bridge footer; got: %s", reply)
	}
}

func TestRenderCommunityImage_Popularity(t *testing.T) {
	// Live DescribeCommunityImages carries the group name in ImageName and
	// CreatedCount (部署次数) as the popularity signal. The header must surface the
	// deploy count so users can judge popularity.
	group := map[string]any{
		"ImageName":    "最强AI数字人InfiniteTalk",
		"Author":       "与AI同行",
		"CreatedCount": float64(13517),
		"Data": []any{
			map[string]any{"CompShareImageId": "g1v1", "Name": "v26.0201"},
		},
	}
	raw := map[string]any{"CompshareImageGroup": []any{group}}
	reply := renderCommunityImageReply(raw, "查询社区镜像")
	if !strings.Contains(reply, "部署次数=13517") {
		t.Errorf("header should surface CreatedCount popularity; got: %s", reply)
	}
	if !strings.Contains(reply, "名称=最强AI数字人InfiniteTalk") {
		t.Errorf("header should use group-level ImageName (live shape); got: %s", reply)
	}

	// A group with no CreatedCount must NOT fabricate a 部署次数 figure.
	noCount := map[string]any{"ImageName": "x", "Data": []any{map[string]any{"Name": "v1"}}}
	rawNoCount := map[string]any{"CompshareImageGroup": []any{noCount}}
	if got := renderCommunityImageReply(rawNoCount, "查询社区镜像"); strings.Contains(got, "部署次数=") {
		t.Errorf("must not show 部署次数 when CreatedCount is absent; got: %s", got)
	}
}

func TestRenderCommunityImage_SortedByDeployCount(t *testing.T) {
	// Live API default order is recommend-weighted, not deploy-count. The renderer
	// reorders by CreatedCount desc so the most-deployed images surface first and
	// the 部署次数 figures read monotonically (jumbled numbers look broken).
	mk := func(name string, created int) map[string]any {
		return map[string]any{
			"ImageName":    name,
			"CreatedCount": float64(created),
			"Data":         []any{map[string]any{"Name": name + "-v1", "CompShareImageId": name}},
		}
	}
	raw := map[string]any{"CompshareImageGroup": []any{
		mk("low", 100), mk("high", 9000), mk("mid", 3000),
	}}
	reply := renderCommunityImageReply(raw, "查询社区镜像")
	hi := strings.Index(reply, "名称=high")
	mi := strings.Index(reply, "名称=mid")
	lo := strings.Index(reply, "名称=low")
	if !(hi >= 0 && mi > hi && lo > mi) {
		t.Errorf("expected deploy-count-desc order (high<mid<low); got idx hi=%d mid=%d low=%d in:\n%s", hi, mi, lo, reply)
	}
}

func TestRenderCommunityImage_CapsDefaultOutputAtTen(t *testing.T) {
	total := communityImageGroupLimit + 5
	groups := make([]any, 0, total)
	for i := 0; i < total; i++ {
		groups = append(groups, map[string]any{
			"ImageName":    fmt.Sprintf("community-image-%02d", i),
			"CreatedCount": float64(total - i),
		})
	}
	raw := map[string]any{"CompshareImageGroup": groups}

	reply := renderCommunityImageReply(raw, "查询社区镜像")

	if got := strings.Count(reply, "名称=community-image-"); got != communityImageGroupLimit {
		t.Fatalf("expected %d community image candidates, got %d in:\n%s", communityImageGroupLimit, got, reply)
	}
	if strings.Contains(reply, "community-image-10") || strings.Contains(reply, "community-image-14") {
		t.Fatalf("reply should not include candidates beyond the cap; got:\n%s", reply)
	}
}

func TestBuildCommunityImageEnvelope_PopularityFactsAndOrder(t *testing.T) {
	// Prod renders community_image via the LLM grounded renderer, which works off
	// THIS envelope (not the deterministic reply). So the popularity signal, the
	// most-deployed-first ordering, and the deploy-bridge footer must live here.
	mk := func(name string, created int) map[string]any {
		return map[string]any{
			"ImageName":    name,
			"CreatedCount": float64(created),
			"Data":         []any{map[string]any{"Name": name + "-v1", "CompShareImageId": name + "-v1"}},
		}
	}
	noCount := map[string]any{"ImageName": "nocount", "Data": []any{map[string]any{"Name": "nc-v1", "CompShareImageId": "nc-v1"}}}
	raw := map[string]any{"CompshareImageGroup": []any{
		mk("low", 100), mk("high", 9000), mk("mid", 3000), noCount,
	}}
	env := buildCommunityImageEnvelope(raw, "查询社区镜像")

	// Subjects sorted by deploy count desc; the unsized group sorts last.
	got := make([]string, 0, len(env.Subjects))
	for _, s := range env.Subjects {
		got = append(got, s.Name)
	}
	want := []string{"high", "mid", "low", "nocount"}
	if len(got) != len(want) {
		t.Fatalf("subject names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("subject[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// deploy_count fact carries CreatedCount for sized groups; absent for unsized.
	deployFor := map[string]int{}
	for _, f := range env.Facts {
		if f.Key == "deploy_count" {
			if n, ok := f.Value.(int); ok {
				deployFor[f.SubjectID] = n
			}
		}
	}
	if deployFor["image_group:high"] != 9000 {
		t.Errorf("high deploy_count fact = %d, want 9000", deployFor["image_group:high"])
	}
	if _, ok := deployFor["image_group:nocount"]; ok {
		t.Errorf("unsized group must not carry a deploy_count fact")
	}

	// disclaimer computed fact carries the deploy-bridge footer verbatim (the
	// renderer prompt emits computed.disclaimer as the last line, unmodified).
	var disclaimer string
	for _, f := range env.Computed {
		if f.Key == "disclaimer" {
			disclaimer, _ = f.Value.(string)
		}
	}
	if disclaimer != communityImageDeployFooter() {
		t.Errorf("disclaimer computed fact = %q, want deploy footer %q", disclaimer, communityImageDeployFooter())
	}
}

func TestBuildCommunityImageEnvelope_CapsDefaultSubjectsAtTen(t *testing.T) {
	total := communityImageGroupLimit + 5
	groups := make([]any, 0, total)
	for i := 0; i < total; i++ {
		groups = append(groups, map[string]any{
			"ImageName":    fmt.Sprintf("community-image-%02d", i),
			"CreatedCount": float64(total - i),
		})
	}
	raw := map[string]any{"CompshareImageGroup": groups}

	env := buildCommunityImageEnvelope(raw, "查询社区镜像")

	if got := len(env.Subjects); got != communityImageGroupLimit {
		t.Fatalf("expected %d community image subjects, got %d: %#v", communityImageGroupLimit, got, env.Subjects)
	}
	for _, subject := range env.Subjects {
		if strings.Contains(subject.Name, "community-image-10") || strings.Contains(subject.Name, "community-image-14") {
			t.Fatalf("envelope should not include subjects beyond the cap; got %#v", env.Subjects)
		}
	}
}

func TestRenderSharedImageListReply_ListAll(t *testing.T) {
	raw := map[string]any{
		"TotalCount": float64(1),
		"ImageSet": []any{map[string]any{
			"CompShareImageId": "img-shared-1",
			"Name":             "shared-env",
			"ImageType":        "Custom",
			"Status":           "Available",
			"Container":        "True",
			"Owner":            map[string]any{"AccountName": "team-a", "AccountId": float64(123)},
		}},
	}
	reply := renderSharedImageListReply(raw, "别人共享给我的镜像在哪看")
	// Clean display (③): 名称-first + 中文 labels + owner; the raw CompShareImageId is
	// dropped from the default view (用户按名称引用即可) — assert it is ABSENT.
	for _, want := range []string{"共享给你的镜像", "名称=shared-env", "所有者=team-a"} {
		if !strings.Contains(reply, want) {
			t.Errorf("shared image reply missing %q: %s", want, reply)
		}
	}
	if strings.Contains(reply, "img-shared-1") {
		t.Errorf("raw CompShareImageId must NOT appear in the clean shared-image display: %s", reply)
	}
}

func TestRenderSharedImageListReply_NameFilterNoMatch(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{map[string]any{"CompShareImageId": "img-shared-1", "Name": "shared-env"}},
	}
	reply := renderSharedImageListReply(raw, "llama 共享镜像")
	if !strings.Contains(reply, "未找到匹配的共享镜像") {
		t.Errorf("shared image no-match should be explicit; got: %s", reply)
	}
}

func TestRenderSharedImageListReply_Empty(t *testing.T) {
	reply := renderSharedImageListReply(map[string]any{}, "别人共享给我的镜像")
	if !strings.Contains(reply, "未获取到共享给你的镜像") {
		t.Errorf("empty shared image reply should be explicit; got: %s", reply)
	}
}

// TestRenderImageListReply_CleanDisplay (③) proves the platform image list renders
// 名称-first with 中文 labels and DROPS the raw CompShareImageId from the default
// view — the user's "像镜像一样输出来的比较乱" complaint (raw English Key=Value dump
// led by the image id).
func TestRenderImageListReply_CleanDisplay(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-pt", "Name": "PyTorch 2.9", "ImageType": "App"},
	}}
	fieldOrder := []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"}
	reply := renderImageListReply(raw, "ImageSet", fieldOrder, "有哪些镜像")
	if !strings.Contains(reply, "名称=PyTorch 2.9") {
		t.Errorf("clean display must lead with 名称; got: %s", reply)
	}
	if !strings.Contains(reply, "镜像类型=App") {
		t.Errorf("clean display must use 中文 field labels; got: %s", reply)
	}
	if strings.Contains(reply, "img-pt") || strings.Contains(reply, "CompShareImageId") {
		t.Errorf("raw CompShareImageId must NOT appear in the clean image display; got: %s", reply)
	}
}

// TestRenderImageListReply_CapAndOverflow (③) proves a list-all over the cap shows
// only imageListDisplayCap rows + a "共 N 个" overflow note inviting a keyword filter.
func TestRenderImageListReply_CapAndOverflow(t *testing.T) {
	total := imageListDisplayCap + 7
	items := make([]any, 0, total)
	for i := 0; i < total; i++ {
		items = append(items, map[string]any{"CompShareImageId": fmt.Sprintf("img-%d", i), "Name": fmt.Sprintf("img-name-%d", i)})
	}
	reply := renderImageListReply(map[string]any{"ImageSet": items}, "ImageSet", []string{"CompShareImageId", "Name"}, "列出全部镜像")
	if got := strings.Count(reply, "名称="); got != imageListDisplayCap {
		t.Errorf("expected %d capped rows, got %d", imageListDisplayCap, got)
	}
	if !strings.Contains(reply, fmt.Sprintf("共 %d 个镜像", total)) || !strings.Contains(reply, "可补充关键词") {
		t.Errorf("over-cap reply must carry the overflow note; got: %s", reply)
	}
}

// TestBuildImageListEnvelope_NoRawIDFacts (③) proves the grounded-render envelope no
// longer emits CompShareImageId as a per-row display Fact (which made the LLM dump
// ids) — the id is preserved structurally in Subject.ID.
func TestBuildImageListEnvelope_NoRawIDFacts(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-pt", "Name": "PyTorch 2.9", "ImageType": "App"},
	}}
	fieldOrder := []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"}
	env := buildImageListEnvelope(raw, "ImageSet", fieldOrder, "有哪些镜像", "DescribeCompShareImages", "platform")
	for _, f := range env.Facts {
		if f.Key == "CompShareImageId" || f.Key == "Name" {
			t.Errorf("display Fact %q should be dropped (id→Subject.ID, name→Subject.Name); facts=%v", f.Key, env.Facts)
		}
	}
	if len(env.Subjects) != 1 || env.Subjects[0].ID != "image:img-pt" || env.Subjects[0].Name != "PyTorch 2.9" {
		t.Errorf("Subject must still carry id+name: %+v", env.Subjects)
	}
}

func TestBuildImageListEnvelope_PyTorchMatchesTorchNamedImages(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04 64位", "ImageType": "System"},
		map[string]any{"CompShareImageId": "img-torch", "Name": "cuda128_torch291_py312", "ImageType": "App"},
	}}

	env := buildImageListEnvelope(raw, "ImageSet", []string{"CompShareImageId", "Name", "ImageType"},
		"有哪些 PyTorch 镜像", "DescribeCompShareImages", "platform")

	if len(env.Subjects) != 1 || env.Subjects[0].Name != "cuda128_torch291_py312" {
		t.Fatalf("PyTorch envelope should keep only torch-compatible image, got %#v", env.Subjects)
	}
}

func TestBuildCommunityImageEnvelope_DigitalHumanQueryMatchesRelevantGroups(t *testing.T) {
	raw := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "LiveTalking", "CreatedCount": float64(10), "Data": []any{
			map[string]any{"CompShareImageId": "live-1", "Name": "数字人实时对话版"},
		}},
		map[string]any{"ImageName": "RAGFlow Ubuntu 22.04", "CreatedCount": float64(100), "Data": []any{
			map[string]any{"CompShareImageId": "rag-1", "Name": "RAGFlow-v1"},
		}},
	}}

	env := buildCommunityImageEnvelope(raw, "有哪些社区镜像适合数字人")

	if len(env.Subjects) != 1 || env.Subjects[0].Name != "LiveTalking" {
		t.Fatalf("digital-human envelope should keep relevant community group only, got %#v", env.Subjects)
	}
}

// ----- end L0 NL filter tests -----------------------------------------------

// TestRegistry_FutureProof_AcceptanceNumberEight is the §5 #8 acceptance test:
// adding a route must NOT require any change to engine.go. The engine.go
// dispatch surface uses ONLY IsRoutingIntent + DispatchRoute, both of
// which now read the generated skill registry — so a new route skill is
// picked up without engine.go knowing the intent's name. We verify this over the
// live route set: every registry-declared route is recognized by
// IsRoutingIntent and routes through DispatchRoute to a Handled result.
//
// (The legacy version injected a temporary routeRegistry entry; with the
// registry generated from skills.GeneratedSkills() there is no mutable in-memory
// table to inject into, so the contract is asserted over the generated set.)
func TestRegistry_FutureProof_AcceptanceNumberEight(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	saw := 0
	for i := range routingIntentSet() {
		if !IsRoutingIntent(i) {
			t.Errorf("future-proof: IsRoutingIntent(%q) = false for a generated route skill", i)
			continue
		}
		result := h.DispatchRoute(context.Background(), HandlerRequest{Plan: IntentRoute{Intent: i}})
		if result.Status != HandlerStatusHandled {
			t.Errorf("future-proof: DispatchRoute(%q) status = %q, want Handled", i, result.Status)
		}
		saw++
	}
	if saw == 0 {
		t.Fatal("future-proof: no route skills found in the generated registry")
	}
}
