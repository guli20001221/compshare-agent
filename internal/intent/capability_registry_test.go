package intent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/skills"
)

// capabilityIntentSet returns the capability intents declared by the generated
// skill registry (non-empty intent_label), keyed by Intent for membership checks.
func capabilityIntentSet() map[Intent]struct{} {
	out := map[Intent]struct{}{}
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel != "" {
			out[Intent(s.IntentLabel)] = struct{}{}
		}
	}
	return out
}

// skillRequiredTool returns RequiredTools[0] for the capability skill bound to
// the given intent, or "" if none.
func skillRequiredTool(i Intent) string {
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel == string(i) && len(s.RequiredTools) > 0 {
			return s.RequiredTools[0]
		}
	}
	return ""
}

// TestIsCapabilityIntent_KnownLabels verifies all 6 registered capability intents
// return true. New capabilities must be picked up by IsCapabilityIntent without
// any code change in callers (engine.go etc.) — this is the v1 contract.
func TestIsCapabilityIntent_KnownLabels(t *testing.T) {
	wanted := []Intent{
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		IntentPricingQuery,
	}
	for _, intent := range wanted {
		if !IsCapabilityIntent(intent) {
			t.Errorf("IsCapabilityIntent(%q) = false, want true", intent)
		}
	}
}

// TestIsCapabilityIntent_UnknownReturnsFalse guards against accidental "capture
// everything" predicates that would break the routing OR-list in engine.go.
func TestIsCapabilityIntent_UnknownReturnsFalse(t *testing.T) {
	notCapability := []Intent{
		IntentResourceInfo,
		IntentMonitorQuery,
		IntentKnowledgeQA,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
		IntentDiagnosis,
		IntentUnknown,
		Intent("not_a_real_intent"),
	}
	for _, intent := range notCapability {
		if IsCapabilityIntent(intent) {
			t.Errorf("IsCapabilityIntent(%q) = true, want false", intent)
		}
	}
}

// TestCapabilityIntentOrder_NoDuplicates ensures the byte-identity-pinned
// capabilityIntentOrder has no shadowed entries (a duplicate would emit a
// duplicate planner-prompt fragment).
func TestCapabilityIntentOrder_NoDuplicates(t *testing.T) {
	seen := map[Intent]struct{}{}
	for _, i := range capabilityIntentOrder {
		if _, ok := seen[i]; ok {
			t.Errorf("duplicate intent %q in capabilityIntentOrder", i)
		}
		seen[i] = struct{}{}
	}
}

// TestCapabilityRequiredTool_BindsToRealTool guards against typo'd tool names that
// would lookup-miss in handlerActionWhitelist or fail at SafeToolExecutor. The
// required tool now comes from the generated skill registry (RequiredTools[0]).
func TestCapabilityRequiredTool_BindsToRealTool(t *testing.T) {
	expected := map[Intent]string{
		IntentGPUSpecsQuery:      "DescribeAvailableCompShareInstanceTypes",
		IntentStockAvailability:  "DescribeAvailableCompShareInstanceTypes",
		IntentPlatformImageList:  "DescribeCompShareImages",
		IntentCustomImageList:    "DescribeCompShareCustomImages",
		IntentCommunityImageList: "DescribeCommunityImages",
		IntentPricingQuery:       "GetCompShareInstancePrice",
	}
	for _, i := range capabilityIntentOrder {
		want := expected[i]
		if want == "" {
			t.Errorf("unexpected intent %q in capabilityIntentOrder", i)
			continue
		}
		got, ok := capabilityRequiredTool(i)
		if !ok {
			t.Errorf("capabilityRequiredTool(%q) = (_, false), want a tool", i)
			continue
		}
		if got != want {
			t.Errorf("capabilityRequiredTool(%q) = %q, want %q", i, got, want)
		}
	}
}

// TestHandlerActionWhitelist_DerivesFromSkillRegistry enforces single-source-of-truth
// (memory: feedback_cross_pr_contract_drift_check). Every capability skill's
// required tool (RequiredTools[0]) must be auto-included in the whitelist; nothing
// should be hardcoded twice. The exact set is separately pinned by
// TestHandlerActionWhitelist_ExactGoldenSet.
func TestHandlerActionWhitelist_DerivesFromSkillRegistry(t *testing.T) {
	wl := handlerActionWhitelist()
	for i := range capabilityIntentSet() {
		want := skillRequiredTool(i)
		if want == "" {
			continue
		}
		actions, ok := wl[i]
		if !ok {
			t.Errorf("capability intent %q missing from handlerActionWhitelist (derivation bug)", i)
			continue
		}
		if _, ok := actions[want]; !ok {
			t.Errorf("capability %q required tool %q not in whitelist[%q]", i, want, i)
		}
	}
}

// TestHandlerActionWhitelist_ExactGoldenSet is the SECURITY gate against silent
// widening of the SafeToolExecutor boundary. handlerActionWhitelist() must equal
// EXACTLY this golden set — set-equality, no missing/extra entries. The
// per-capability action is the required tool (RequiredTools[0]), NOT the broader
// react_tool_subset (which would add e.g. GetGPUSpecs to gpu_specs). If any intent
// gains or loses an action, this test fails loudly.
func TestHandlerActionWhitelist_ExactGoldenSet(t *testing.T) {
	golden := map[Intent]map[string]struct{}{
		IntentResourceInfo:       {"DescribeCompShareInstance": {}},
		IntentMonitorQuery:       {"GetCompShareInstanceMonitor": {}},
		IntentGPUSpecsQuery:      {"DescribeAvailableCompShareInstanceTypes": {}},
		IntentStockAvailability:  {"DescribeAvailableCompShareInstanceTypes": {}, "DescribeCompShareImages": {}, "CheckCompShareResourceCapacity": {}},
		IntentPlatformImageList:  {"DescribeCompShareImages": {}},
		IntentCustomImageList:    {"DescribeCompShareCustomImages": {}},
		IntentCommunityImageList: {"DescribeCommunityImages": {}},
		IntentPricingQuery:       {"GetCompShareInstancePrice": {}},
	}
	got := handlerActionWhitelist()
	if !reflect.DeepEqual(got, golden) {
		t.Fatalf("handlerActionWhitelist drifted from golden set (security widening guard).\n got:    %v\n golden: %v", got, golden)
	}
}

// TestCapabilityPromptFragments_ContainsAllIntents ensures every registered
// intent has both a directive AND a planner one-shot example. Missing either
// = planner LLM unaware of the intent enum → routing degrades silently.
func TestCapabilityPromptFragments_ContainsAllIntents(t *testing.T) {
	directives, examples := CapabilityPromptFragments()
	combined := strings.Join(append(append([]string{}, directives...), examples...), "\n")
	for _, i := range capabilityIntentOrder {
		if !strings.Contains(combined, string(i)) {
			t.Errorf("capability fragments missing intent label %q (planner won't know to emit it)", i)
		}
	}
}

func TestCapabilityPromptFragments_DeriveFromSkillRegistry(t *testing.T) {
	directives, examples := CapabilityPromptFragments()
	combinedDirectives := strings.Join(directives, "\n")
	combinedExamples := strings.Join(examples, "\n")
	for _, meta := range skillRegistryCapabilityMetadata() {
		if len(meta.PlannerDirectives) == 0 {
			t.Fatalf("capability %q must declare planner_directives in its skill", meta.Name)
		}
		if len(meta.PlannerExamples) == 0 {
			t.Fatalf("capability %q must declare planner_examples in its skill", meta.Name)
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

func TestCapabilityMetadataRequiredToolsMatchSkillRegistry(t *testing.T) {
	byIntent := map[Intent]CapabilityMetadata{}
	for _, meta := range skillRegistryCapabilityMetadata() {
		byIntent[Intent(meta.IntentLabel)] = meta
	}
	for _, i := range capabilityIntentOrder {
		meta, ok := byIntent[i]
		if !ok {
			t.Fatalf("missing metadata for capability intent %q", i)
		}
		want := skillRequiredTool(i)
		if meta.RequiredTool != want {
			t.Fatalf("metadata required_tool for %q = %q, skill registry has %q", i, meta.RequiredTool, want)
		}
	}
}

// TestDispatchCapability_RoutesToHandler verifies each handler is reachable via
// DispatchCapability. Uses a stub executor that fails fast so we only check
// handler routing, not full tool semantics.
type stubFailingExecutor struct{}

func (stubFailingExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

type capabilitySequenceExecutor struct {
	results map[string]map[string]any
	errs    map[string]error
	calls   []handlerExecCall
}

func (m *capabilitySequenceExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
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

func TestDispatchCapability_RoutesToHandler(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	for i := range capabilityIntentSet() {
		req := HandlerRequest{Plan: Plan{Intent: i}}
		result := h.DispatchCapability(context.Background(), req)
		// With empty mock response, handlers should return a HandledResult
		// (their renderers produce "未获取到..." replies on empty data).
		if result.Status != HandlerStatusHandled {
			t.Errorf("DispatchCapability(%q) status = %q, want %q", i, result.Status, HandlerStatusHandled)
		}
		if want := skillRequiredTool(i); result.ToolAction != want {
			t.Errorf("DispatchCapability(%q) ToolAction = %q, want %q", i, result.ToolAction, want)
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

// TestDispatchCapability_UnknownIntentFalls verifies that calling
// DispatchCapability with a non-registered intent returns a FallbackBeforeTool
// (defensive layer; engine.go gates on IsCapabilityIntent before invoking).
func TestDispatchCapability_UnknownIntentFalls(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	req := HandlerRequest{Plan: Plan{Intent: Intent("not_a_capability")}}
	result := h.DispatchCapability(context.Background(), req)
	if result.Status != HandlerStatusFallbackBeforeTool {
		t.Errorf("unknown-intent dispatch status = %q, want %q", result.Status, HandlerStatusFallbackBeforeTool)
	}
}

// TestCapabilityMetadata_LoadedFromSkillRegistry verifies the skill-registry
// projection produced one metadata entry per capability intent and that none
// declares required_citation (capabilities are NOT cited per PR A spec).
func TestCapabilityMetadata_LoadedFromSkillRegistry(t *testing.T) {
	meta := skillRegistryCapabilityMetadata()
	if got, want := len(meta), len(capabilityIntentOrder); got != want {
		t.Fatalf("skillRegistryCapabilityMetadata count = %d, want %d (capabilityIntentOrder size)", got, want)
	}
	order := map[Intent]struct{}{}
	for _, i := range capabilityIntentOrder {
		order[i] = struct{}{}
	}
	for _, m := range meta {
		if _, ok := order[Intent(m.IntentLabel)]; !ok {
			t.Errorf("capability metadata has intent_label %q not in capabilityIntentOrder", m.IntentLabel)
		}
		if m.RequiredCitation {
			t.Errorf("capability %q has required_citation=true; capabilities are NOT cited per PR A spec", m.Name)
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

func TestGPUSpecsCapabilityUsesDescribeAvailableAndExpandsFullRequest(t *testing.T) {
	exec := &capabilitySequenceExecutor{results: map[string]map[string]any{
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

	result := handler.DispatchCapability(context.Background(), HandlerRequest{
		Plan:     Plan{Intent: IntentGPUSpecsQuery},
		UserText: "4090 的所有规格",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if len(exec.calls) != 1 || exec.calls[0].action != "DescribeAvailableCompShareInstanceTypes" {
		t.Fatalf("calls = %#v, want one DescribeAvailableCompShareInstanceTypes call", exec.calls)
	}
	if len(exec.calls[0].args) != 0 {
		t.Fatalf("gpu specs capability should query full upstream data without narrowing args, got %#v", exec.calls[0].args)
	}
	if result.Envelope == nil {
		t.Fatal("gpu specs capability should attach a renderer envelope")
	}
	if result.Envelope.Kind != "gpu_specs_query" {
		t.Fatalf("envelope kind = %q, want gpu_specs_query", result.Envelope.Kind)
	}
	if len(result.RendererInputEnvelopeHashes) != 1 {
		t.Fatalf("renderer envelope hashes = %#v, want one hash", result.RendererInputEnvelopeHashes)
	}
	for _, want := range []string{"机型=4090", "16C/64G", "16C/94G"} {
		if !strings.Contains(result.Reply, want) {
			t.Fatalf("full capability reply should include %q, got: %s", want, result.Reply)
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
	exec := &capabilitySequenceExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal"},
			},
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

	result := handler.DispatchCapability(context.Background(), HandlerRequest{
		Plan:     Plan{Intent: IntentStockAvailability},
		UserText: "4090 现在有没有货",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if !strings.Contains(result.Reply, "4090 当前暂无可创建库存") {
		t.Fatalf("reply should answer concrete creatability, got: %s", result.Reply)
	}
	if strings.Contains(result.Reply, "ResourceEnough") || strings.Contains(result.Reply, "容量预检口径") {
		t.Fatalf("reply should not expose implementation details, got: %s", result.Reply)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("calls = %#v, want 3 calls", exec.calls)
	}
	if exec.calls[0].action != "DescribeAvailableCompShareInstanceTypes" ||
		exec.calls[1].action != "DescribeCompShareImages" ||
		exec.calls[2].action != "CheckCompShareResourceCapacity" {
		t.Fatalf("unexpected call sequence: %#v", exec.calls)
	}
	args := exec.calls[2].args
	if args["GpuType"] != "4090" {
		t.Fatalf("capacity GpuType = %#v, want 4090", args["GpuType"])
	}
	if args["Zone"] != "cn-wlcb-01" {
		t.Fatalf("capacity Zone = %#v, want cn-wlcb-01", args["Zone"])
	}
	if args["CompShareImageId"] != "img-ubuntu" {
		t.Fatalf("capacity CompShareImageId = %#v, want img-ubuntu", args["CompShareImageId"])
	}
	if args["ChargeType"] != "Dynamic" {
		t.Fatalf("capacity ChargeType = %#v, want Dynamic", args["ChargeType"])
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

	result := handler.DispatchCapability(context.Background(), HandlerRequest{
		Plan:     Plan{Intent: IntentStockAvailability},
		UserText: "4090 现在有没有货",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if !strings.Contains(result.Reply, "4090 当前暂无可创建库存") {
		t.Fatalf("reply should still answer from successful capacity checks, got: %s", result.Reply)
	}
	if strings.Contains(result.Reply, "部分可用区容量预检未完成") {
		t.Fatalf("reply should not expose unprobed zones, got: %s", result.Reply)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("calls = %#v, want 3 calls", exec.calls)
	}
}

func TestStockAvailabilityFallsBackToNextZoneWhenCapacityCheckFails(t *testing.T) {
	exec := &stockCapacityFallbackExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.DispatchCapability(context.Background(), HandlerRequest{
		Plan:     Plan{Intent: IntentStockAvailability},
		UserText: "4090 鐜板湪鏈夋病鏈夎揣",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if len(exec.calls) != 4 {
		t.Fatalf("calls = %#v, want fallback capacity call in second zone", exec.calls)
	}
	if exec.calls[2].action != "CheckCompShareResourceCapacity" || exec.calls[2].args["Zone"] != "cn-sh2-02" {
		t.Fatalf("first capacity call = %#v, want cn-sh2-02", exec.calls[2])
	}
	if exec.calls[3].action != "CheckCompShareResourceCapacity" || exec.calls[3].args["Zone"] != "cn-wlcb-01" {
		t.Fatalf("fallback capacity call = %#v, want cn-wlcb-01", exec.calls[3])
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
}

// ----- end L0 NL filter tests -----------------------------------------------

// TestRegistry_FutureProof_AcceptanceNumberEight is the §5 #8 acceptance test:
// adding a capability must NOT require any change to engine.go. The engine.go
// dispatch surface uses ONLY IsCapabilityIntent + DispatchCapability, both of
// which now read the generated skill registry — so a new capability skill is
// picked up without engine.go knowing the intent's name. We verify this over the
// live capability set: every registry-declared capability is recognized by
// IsCapabilityIntent and routes through DispatchCapability to a Handled result.
//
// (The legacy version injected a temporary capabilityRegistry entry; with the
// registry generated from skills.GeneratedSkills() there is no mutable in-memory
// table to inject into, so the contract is asserted over the generated set.)
func TestRegistry_FutureProof_AcceptanceNumberEight(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	saw := 0
	for i := range capabilityIntentSet() {
		if !IsCapabilityIntent(i) {
			t.Errorf("future-proof: IsCapabilityIntent(%q) = false for a generated capability skill", i)
			continue
		}
		result := h.DispatchCapability(context.Background(), HandlerRequest{Plan: Plan{Intent: i}})
		if result.Status != HandlerStatusHandled {
			t.Errorf("future-proof: DispatchCapability(%q) status = %q, want Handled", i, result.Status)
		}
		saw++
	}
	if saw == 0 {
		t.Fatal("future-proof: no capability skills found in the generated registry")
	}
}
