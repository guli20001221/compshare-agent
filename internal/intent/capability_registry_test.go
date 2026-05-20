package intent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/compshare-agent/internal/entity"
)

// TestIsCapabilityIntent_KnownLabels verifies all 5 registered capability intents
// return true. New capabilities must be picked up by IsCapabilityIntent without
// any code change in callers (engine.go etc.) — this is the v1 contract.
func TestIsCapabilityIntent_KnownLabels(t *testing.T) {
	wanted := []Intent{
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
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

// TestCapabilityRegistry_NoDuplicateIntents ensures the registry table has no
// shadowed entries (a duplicate would silently mask the second handler).
func TestCapabilityRegistry_NoDuplicateIntents(t *testing.T) {
	seen := map[Intent]struct{}{}
	for _, e := range capabilityRegistry {
		if _, ok := seen[e.intent]; ok {
			t.Errorf("duplicate intent %q in capabilityRegistry", e.intent)
		}
		seen[e.intent] = struct{}{}
	}
}

// TestCapabilityRegistry_BindsToRealTool guards against typo'd tool names that
// would lookup-miss in handlerActionWhitelist or fail at SafeToolExecutor.
func TestCapabilityRegistry_BindsToRealTool(t *testing.T) {
	expected := map[Intent]string{
		IntentGPUSpecsQuery:      "DescribeAvailableCompShareInstanceTypes",
		IntentStockAvailability:  "DescribeAvailableCompShareInstanceTypes",
		IntentPlatformImageList:  "DescribeCompShareImages",
		IntentCustomImageList:    "DescribeCompShareCustomImages",
		IntentCommunityImageList: "DescribeCommunityImages",
	}
	for _, e := range capabilityRegistry {
		want, ok := expected[e.intent]
		if !ok {
			t.Errorf("unexpected intent %q in registry", e.intent)
			continue
		}
		if e.requiredTool != want {
			t.Errorf("registry[%q].requiredTool = %q, want %q", e.intent, e.requiredTool, want)
		}
	}
}

// TestHandlerActionWhitelist_DerivesFromRegistry enforces single-source-of-truth
// (memory: feedback_cross_pr_contract_drift_check). If a new capability is
// added to the registry, the whitelist must auto-include it; nothing should be
// hardcoded twice.
func TestHandlerActionWhitelist_DerivesFromRegistry(t *testing.T) {
	wl := handlerActionWhitelist()
	for _, e := range capabilityRegistry {
		actions, ok := wl[e.intent]
		if !ok {
			t.Errorf("registry intent %q missing from handlerActionWhitelist (derivation bug)", e.intent)
			continue
		}
		if _, ok := actions[e.requiredTool]; !ok {
			t.Errorf("registry[%q].requiredTool=%q not in whitelist[%q]", e.intent, e.requiredTool, e.intent)
		}
	}
}

// TestCapabilityPromptFragments_ContainsAllIntents ensures every registered
// intent has both a directive AND a planner one-shot example. Missing either
// = planner LLM unaware of the intent enum → routing degrades silently.
func TestCapabilityPromptFragments_ContainsAllIntents(t *testing.T) {
	directives, examples := CapabilityPromptFragments()
	combined := strings.Join(append(append([]string{}, directives...), examples...), "\n")
	for _, e := range capabilityRegistry {
		if !strings.Contains(combined, string(e.intent)) {
			t.Errorf("capability fragments missing intent label %q (planner won't know to emit it)", e.intent)
		}
	}
}

func TestCapabilityPromptFragments_DeriveFromMarkdownFrontmatter(t *testing.T) {
	directives, examples := CapabilityPromptFragments()
	combinedDirectives := strings.Join(directives, "\n")
	combinedExamples := strings.Join(examples, "\n")
	for _, meta := range capabilityMetadata {
		if len(meta.PlannerDirectives) == 0 {
			t.Fatalf("capability %q must declare planner_directives in markdown frontmatter", meta.Name)
		}
		if len(meta.PlannerExamples) == 0 {
			t.Fatalf("capability %q must declare planner_examples in markdown frontmatter", meta.Name)
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

func TestCapabilityMetadata_RequiresPromptFragments(t *testing.T) {
	_, err := loadCapabilityMetadata(fstest.MapFS{
		"capabilities/demo.md": {
			Data: []byte(`---
name: demo
intent_label: gpu_specs_query
required_tool: DescribeAvailableCompShareInstanceTypes
required_citation: false
---

# demo
`),
		},
	})
	if err == nil {
		t.Fatal("loadCapabilityMetadata should reject capabilities without planner prompt fragments")
	}
	if !strings.Contains(err.Error(), "planner_directives") || !strings.Contains(err.Error(), "planner_examples") {
		t.Fatalf("error should mention missing planner fragments, got: %v", err)
	}
}

func TestCapabilityMetadata_RejectsEmptyPromptFragments(t *testing.T) {
	_, err := loadCapabilityMetadata(fstest.MapFS{
		"capabilities/demo.md": {
			Data: []byte(`---
name: demo
intent_label: gpu_specs_query
required_tool: DescribeAvailableCompShareInstanceTypes
required_citation: false
planner_directives:
  - "   "
planner_examples:
  - question: ""
    confidence: 0
---

# demo
`),
		},
	})
	if err == nil {
		t.Fatal("loadCapabilityMetadata should reject empty planner prompt fragments")
	}
}

func TestCapabilityMetadata_RejectsUnknownFrontmatterFields(t *testing.T) {
	_, err := loadCapabilityMetadata(fstest.MapFS{
		"capabilities/demo.md": {
			Data: []byte(`---
name: demo
intent_label: gpu_specs_query
required_tool: DescribeAvailableCompShareInstanceTypes
required_citation: false
planner_directive:
  - typo should fail
planner_examples:
  - question: "4090 显存多大"
    confidence: 0.85
---

# demo
`),
		},
	})
	if err == nil {
		t.Fatal("loadCapabilityMetadata should reject unknown frontmatter fields")
	}
}

func TestCapabilityMetadataRequiredToolsMatchRegistry(t *testing.T) {
	byIntent := map[Intent]CapabilityMetadata{}
	for _, meta := range capabilityMetadata {
		byIntent[Intent(meta.IntentLabel)] = meta
	}
	for _, entry := range capabilityRegistry {
		meta, ok := byIntent[entry.intent]
		if !ok {
			t.Fatalf("missing metadata for capability intent %q", entry.intent)
		}
		if meta.RequiredTool != entry.requiredTool {
			t.Fatalf("metadata required_tool for %q = %q, registry has %q", entry.intent, meta.RequiredTool, entry.requiredTool)
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
	for _, e := range capabilityRegistry {
		req := HandlerRequest{Plan: Plan{Intent: e.intent}}
		result := h.DispatchCapability(context.Background(), req)
		// With empty mock response, handlers should return a HandledResult
		// (their renderers produce "未获取到..." replies on empty data).
		if result.Status != HandlerStatusHandled {
			t.Errorf("DispatchCapability(%q) status = %q, want %q", e.intent, result.Status, HandlerStatusHandled)
		}
		if result.ToolAction != e.requiredTool {
			t.Errorf("DispatchCapability(%q) ToolAction = %q, want %q", e.intent, result.ToolAction, e.requiredTool)
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

// TestCapabilityMetadata_LoadedAtBuild verifies the embed.FS frontmatter parser
// produced one entry per registry intent. Fail-fast at init() would have already
// panicked, but this test makes the requirement visible in the test report.
func TestCapabilityMetadata_LoadedAtBuild(t *testing.T) {
	if got, want := len(capabilityMetadata), len(capabilityRegistry); got != want {
		t.Fatalf("capabilityMetadata count = %d, want %d (registry size)", got, want)
	}
	regSet := map[Intent]struct{}{}
	for _, e := range capabilityRegistry {
		regSet[e.intent] = struct{}{}
	}
	for _, m := range capabilityMetadata {
		if _, ok := regSet[Intent(m.IntentLabel)]; !ok {
			t.Errorf("capabilityMetadata has intent_label %q with no matching registry entry", m.IntentLabel)
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
	apiNames := []string{"4090", "4090_48G", "5090", "A100", "A800", "V100S"}
	cases := []struct {
		text string
		want []string
	}{
		{"4090 显存多大", []string{"4090"}}, // user mentioned "4090" -> exact; "4090_48G" is a different model
		{"a100 几张卡", []string{"A100"}},  // case-insensitive
		{"v100s 配置", []string{"V100S"}},
		{"H100 库存", nil}, // H100 not in API set — caller handles via known-unavailable
		{"未指定", nil},
	}
	for _, c := range cases {
		got := matchUserTokensToAPINames(c.text, apiNames)
		if len(got) != len(c.want) {
			t.Errorf("matchUserTokensToAPINames(%q) = %v, want %v", c.text, got, c.want)
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

func TestStockAvailabilityPrefersAccountSnapshotZoneForCapacityPrecheck(t *testing.T) {
	exec := &stockCapacityZoneExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.DispatchCapability(context.Background(), HandlerRequest{
		Plan:     Plan{Intent: IntentStockAvailability},
		Resolver: stockZoneSnapshot(t, "cn-wlcb-01"),
		UserText: "4090 现在有没有货",
	})

	if result.Status != HandlerStatusHandled {
		t.Fatalf("status = %q, want %q", result.Status, HandlerStatusHandled)
	}
	if strings.Contains(result.Reply, "部分可用区容量预检未完成") {
		t.Fatalf("reply should not include skipped out-of-project zone failure, got: %s", result.Reply)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("calls = %#v, want 3 calls", exec.calls)
	}
	if exec.calls[2].args["Zone"] != "cn-wlcb-01" {
		t.Fatalf("capacity Zone = %#v, want cn-wlcb-01", exec.calls[2].args["Zone"])
	}
}

func stockZoneSnapshot(t *testing.T, zone string) entity.RegistrySnapshot {
	t.Helper()
	reg := entity.NewRegistry()
	row := instanceRow("uhost-a", "train-a")
	row["Zone"] = zone
	if err := reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1),
		"UHostSet":   []any{row},
	}, "test"); err != nil {
		t.Fatal(err)
	}
	return reg.Snapshot()
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
// adding a capability must NOT require any change to engine.go. We simulate
// this by exercising the registry surface that engine.go depends on
// (IsCapabilityIntent + DispatchCapability), with a temporary entry, and verify
// the surface picks it up without engine.go knowing the intent's name.
//
// This is a function-scope insertion (not a permanent registry mutation): if
// any test runs concurrently with a real production engine, isolation is
// preserved because the registry is a package-level slice and Go test execution
// within one package is single-threaded by default.
func TestRegistry_FutureProof_AcceptanceNumberEight(t *testing.T) {
	const mockIntent = Intent("__test_future_proof_mock__")
	original := capabilityRegistry
	t.Cleanup(func() { capabilityRegistry = original })
	called := false
	mockHandler := func(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
		called = true
		return HandlerResult{
			Status:        HandlerStatusHandled,
			Reply:         "mock OK",
			CutoverStatus: CutoverStatusDispatched,
			ToolAction:    "MockTool",
		}
	}
	capabilityRegistry = append(append([]capabilityEntry{}, original...), capabilityEntry{
		intent:       mockIntent,
		requiredTool: "MockTool",
		handler:      mockHandler,
	})

	// The engine.go dispatch surface uses ONLY these two functions to decide
	// "is this a capability intent? if so, hand it to the registry". Both must
	// pick up the new entry without engine.go changing.
	if !IsCapabilityIntent(mockIntent) {
		t.Fatal("future-proof: IsCapabilityIntent did not pick up new registry entry")
	}
	h := NewDemoHandler(stubFailingExecutor{})
	result := h.DispatchCapability(context.Background(), HandlerRequest{Plan: Plan{Intent: mockIntent}})
	if !called {
		t.Fatal("future-proof: DispatchCapability did not invoke mock handler")
	}
	if result.Status != HandlerStatusHandled || result.Reply != "mock OK" {
		t.Fatalf("future-proof: handler result = %+v, want handled with mock reply", result)
	}
}
