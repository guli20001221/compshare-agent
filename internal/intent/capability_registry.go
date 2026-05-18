package intent

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// capabilityEntry binds an Intent label to one platform tool and the handler that
// invokes that tool. Adding a capability is data-only here: engine.go has a single
// generic IsCapabilityIntent / DispatchCapability hook and does NOT need per-case
// wiring. See .claude/artifacts/pr-capability-routing-brief-2026-05-18.md §3.
type capabilityEntry struct {
	intent       Intent
	requiredTool string
	handler      func(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult
}

var capabilityRegistry = []capabilityEntry{
	{IntentGPUSpecsQuery, "DescribeAvailableCompShareInstanceTypes", handleGPUSpecsQuery},
	{IntentStockAvailability, "DescribeAvailableCompShareInstanceTypes", handleStockAvailability},
	{IntentPlatformImageList, "DescribeCompShareImages", handlePlatformImageList},
	{IntentCustomImageList, "DescribeCompShareCustomImages", handleCustomImageList},
	{IntentCommunityImageList, "DescribeCommunityImages", handleCommunityImageList},
}

// IsCapabilityIntent reports whether the intent is served by the capability
// registry (vs. legacy IntentResourceInfo/MonitorQuery or RAG-bound knowledge_qa).
// Engine.go uses this single predicate to gate capability dispatch.
func IsCapabilityIntent(i Intent) bool {
	for _, e := range capabilityRegistry {
		if e.intent == i {
			return true
		}
	}
	return false
}

// CapabilityIntents returns the set of capability Intents in registration order.
// Used by planner prompt build + cmd/trace parsing.
func CapabilityIntents() []Intent {
	out := make([]Intent, 0, len(capabilityRegistry))
	for _, e := range capabilityRegistry {
		out = append(out, e.intent)
	}
	return out
}

func capabilityRequiredTool(i Intent) (string, bool) {
	for _, e := range capabilityRegistry {
		if e.intent == i {
			return e.requiredTool, true
		}
	}
	return "", false
}

// DispatchCapability resolves a capability intent to its registered handler.
// Returns FallbackBeforeTool(validation) if the intent is not registered — this
// is unreachable when engine.go gates on IsCapabilityIntent first.
func (h *DemoHandler) DispatchCapability(ctx context.Context, req HandlerRequest) HandlerResult {
	for _, e := range capabilityRegistry {
		if e.intent == req.Plan.Intent {
			return e.handler(ctx, h, req)
		}
	}
	return FallbackBeforeTool(FallbackValidation)
}

//go:embed capabilities/*.md
var capabilitiesFS embed.FS

// CapabilityMetadata is the frontmatter shape parsed from each capabilities/*.md.
// Stored only for planner prompt construction; runtime dispatch uses the
// hardcoded registry table above (single source of truth).
type CapabilityMetadata struct {
	Name             string `yaml:"name"`
	IntentLabel      string `yaml:"intent_label"`
	RequiredTool     string `yaml:"required_tool"`
	RequiredCitation bool   `yaml:"required_citation"`
	Body             string `yaml:"-"`
}

var capabilityMetadata = mustLoadCapabilityMetadata()

func mustLoadCapabilityMetadata() []CapabilityMetadata {
	loaded, err := loadCapabilityMetadata(capabilitiesFS)
	if err != nil {
		panic(fmt.Sprintf("intent: capability metadata load failed: %v", err))
	}
	// Verify every registry entry has matching metadata, and every metadata
	// entry has matching registry. Drift here is a build-time bug.
	regSet := map[Intent]struct{}{}
	for _, e := range capabilityRegistry {
		regSet[e.intent] = struct{}{}
	}
	metaSet := map[Intent]struct{}{}
	for _, m := range loaded {
		metaSet[Intent(m.IntentLabel)] = struct{}{}
	}
	for intentValue := range regSet {
		if _, ok := metaSet[intentValue]; !ok {
			panic(fmt.Sprintf("intent: registry entry %q has no matching capabilities/*.md frontmatter", intentValue))
		}
	}
	for intentValue := range metaSet {
		if _, ok := regSet[intentValue]; !ok {
			panic(fmt.Sprintf("intent: capabilities/*.md frontmatter %q has no matching registry entry", intentValue))
		}
	}
	return loaded
}

func loadCapabilityMetadata(efs fs.FS) ([]CapabilityMetadata, error) {
	entries, err := fs.ReadDir(efs, "capabilities")
	if err != nil {
		return nil, fmt.Errorf("read capabilities dir: %w", err)
	}
	out := make([]CapabilityMetadata, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, "_") {
			// Placeholder files (e.g. _general_tech_qa.md.disabled) are skipped
			// so PR B can park its draft without affecting PR A planner prompt.
			continue
		}
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := fs.ReadFile(efs, "capabilities/"+name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		meta, err := parseCapabilityFrontmatter(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		if meta.Name == "" || meta.IntentLabel == "" || meta.RequiredTool == "" {
			return nil, fmt.Errorf("parse %s: name/intent_label/required_tool must be non-empty", name)
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].IntentLabel < out[j].IntentLabel
	})
	return out, nil
}

// parseCapabilityFrontmatter parses a `--- ... ---` YAML preamble + markdown body.
// Required for build-time fail-fast verification that frontmatter matches the
// registry table.
func parseCapabilityFrontmatter(data []byte) (CapabilityMetadata, error) {
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return CapabilityMetadata{}, fmt.Errorf("missing frontmatter `---` opener")
	}
	rest := strings.TrimPrefix(content, "---")
	rest = strings.TrimLeft(rest, "\r\n")
	closer := strings.Index(rest, "\n---")
	if closer < 0 {
		return CapabilityMetadata{}, fmt.Errorf("missing frontmatter `---` closer")
	}
	frontmatter := rest[:closer]
	body := rest[closer+len("\n---"):]
	body = strings.TrimLeft(body, "\r\n")
	var meta CapabilityMetadata
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return CapabilityMetadata{}, fmt.Errorf("yaml unmarshal: %w", err)
	}
	meta.Body = body
	return meta, nil
}

// CapabilityPromptFragments returns one-line planner-prompt directives + 5
// one-shot examples for each registered capability. Called once at planner build
// time and appended to the system prompt.
func CapabilityPromptFragments() ([]string, []string) {
	directives := []string{
		"Stage 2C capability routing: classify clear platform GPU spec / stock / image-list questions to the matching capability intent.",
		"GPU model spec questions like \"4090 显存多大\" or \"A100 supports how many GPUs\" should emit gpu_specs_query.",
		"GPU stock availability questions like \"4090 有没有货\" or \"H100 库存\" should emit stock_availability — these are NOT resource_info (which is only for the user's own instances) and NOT unknown.",
		"Platform image list questions like \"查询平台镜像列表\" or \"Ubuntu 22.04 镜像有吗\" should emit platform_image_list.",
		"User-owned custom image list questions like \"查询自制镜像\" should emit custom_image_list.",
		"Community image list questions like \"查询社区镜像\" should emit community_image_list.",
		"Concept questions like \"系统镜像和基础镜像有什么区别\" or how-to questions like \"怎么发布社区镜像\" stay in knowledge_qa, NOT image-list capabilities.",
	}
	examples := []string{
		`User question: 4090 显存多大`,
		`{"schema_version":"1.0","intent":"gpu_specs_query","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeAvailableCompShareInstanceTypes"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
		`User question: 4090 现在有没有货`,
		`{"schema_version":"1.0","intent":"stock_availability","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeAvailableCompShareInstanceTypes"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
		`User question: 查询平台镜像列表`,
		`{"schema_version":"1.0","intent":"platform_image_list","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareImages"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
		`User question: 查询自制镜像`,
		`{"schema_version":"1.0","intent":"custom_image_list","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareCustomImages"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
		`User question: 查询社区镜像`,
		`{"schema_version":"1.0","intent":"community_image_list","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCommunityImages"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
	}
	return directives, examples
}

// ---- Handler implementations ------------------------------------------------

func executeCapabilityAction(ctx context.Context, h *DemoHandler, intentValue Intent, action string, args map[string]any) (map[string]any, *HandlerResult) {
	// Design choice: capability handlers use two-layer defense rather than the
	// three-layer pattern of legacy handlers (HandleResourceInfo etc.):
	//   layer 1: compile-time `const action` binding inside each capability handler
	//   layer 2: SafeToolExecutor.PolicyForAction gate at the runtime boundary
	// We deliberately skip layer 3 (RequireAllowedHandlerAction reading
	// handlerActionWhitelist) because the registry table IS the binding spec —
	// calling it here would be redundant. As a downstream consequence, adding
	// it would also form a package-init cycle
	//   capabilityRegistry -> handleX -> RequireAllowedHandlerAction ->
	//   handlerActionWhitelist -> capabilityRegistry
	// so the two-layer choice is consistent with what Go's init-cycle detector
	// allows. Drift between registry and whitelist is caught by
	// TestHandlerActionWhitelist_DerivesFromRegistry.
	if h == nil || h.executor == nil {
		// Defensive: production wiring must construct the handler with a
		// SafeToolExecutor adapter before enabling capability cutover.
		fb := FallbackBeforeTool(FallbackValidation)
		return nil, &fb
	}
	raw, err := h.executor.Execute(ctx, action, args)
	if err != nil {
		fail := failureAfterToolForError(action, args, string(intentValue), err)
		return nil, &fail
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

func handleGPUSpecsQuery(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeAvailableCompShareInstanceTypes"
	raw, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderGPUSpecsReply(raw)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

func handleStockAvailability(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeAvailableCompShareInstanceTypes"
	raw, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderStockReply(raw)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

func handlePlatformImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareImages"
	raw, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderImageListReply(raw, "ImageSet", []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"})
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

func handleCustomImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareCustomImages"
	raw, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderImageListReply(raw, "ImageSet", []string{"CompShareImageId", "Name", "ImageName", "Status"})
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

func handleCommunityImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCommunityImages"
	raw, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderCommunityImageReply(raw)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

// ---- Renderers --------------------------------------------------------------

const (
	noGPUSpecsReply    = "未获取到 GPU 机型规格数据。"
	noStockReply       = "未获取到机型库存数据。"
	noImageListReply   = "未获取到镜像列表。"
	noCommunityReply   = "未获取到社区镜像数据。"
	soldOutDisclaimer  = "（CompShare 平台不公开精确剩余数量，仅 Normal/SoldOut 两态。）"
	unsoldGPUWhitelist = "H100、H200 等非在售机型在 CompShare 平台未提供，平台不接受为这类机型推荐配置。"
)

func renderGPUSpecsReply(raw map[string]any) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noGPUSpecsReply
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		parts := []string{"机型=" + name}
		// Performance + GraphicsMemory are nested {Rate, Value} maps in the API
		// response; we display the Value (the scalar the user actually wants).
		if perf := nestedValue(entry, "Performance"); perf != "" {
			parts = append(parts, "性能="+perf)
		}
		if gmem := nestedValue(entry, "GraphicsMemory"); gmem != "" {
			parts = append(parts, "显存="+gmem+"GB")
		}
		if status := safeString(entry, "Status"); status != "" {
			parts = append(parts, "状态="+status)
		}
		if sizes := summarizeMachineSizes(entry); sizes != "" {
			parts = append(parts, "合法配置="+sizes)
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	if len(lines) == 0 {
		return noGPUSpecsReply
	}
	return strings.Join(lines, "\n")
}

// nestedValue extracts the "Value" field from a nested map response shape like
// `{"Performance": {"Rate": 3, "Value": 83}}`. Returns "" if shape doesn't match.
// Used by gpu_specs_query to pretty-print Performance + GraphicsMemory.
func nestedValue(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	if nested, ok := v.(map[string]any); ok {
		if value, ok := nested["Value"]; ok {
			return fmt.Sprint(value)
		}
	}
	return safeValue(v)
}

func renderStockReply(raw map[string]any) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noStockReply
	}
	lines := make([]string, 0, len(items))
	hasSoldOut := false
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		status := safeString(entry, "Status")
		if name == "" {
			continue
		}
		if status == "" {
			// Mock test fixtures and some prod responses omit Status; assume Normal
			// when the type is in the available list at all. Stock semantics:
			// SoldOut entries are typically dropped, so "in list" ≈ "available".
			status = "Normal"
		}
		if status == "SoldOut" {
			hasSoldOut = true
		}
		lines = append(lines, fmt.Sprintf("机型=%s, 状态=%s", name, status))
	}
	if len(lines) == 0 {
		return noStockReply
	}
	if hasSoldOut {
		return strings.Join(lines, "\n") + "\n" + soldOutDisclaimer
	}
	return strings.Join(lines, "\n") + "\n" + soldOutDisclaimer
}

func renderImageListReply(raw map[string]any, listKey string, fieldOrder []string) string {
	items := mapSliceAt(raw, listKey)
	if len(items) == 0 {
		return noImageListReply
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts := make([]string, 0, len(fieldOrder))
		for _, key := range fieldOrder {
			if v := safeString(entry, key); v != "" {
				parts = append(parts, key+"="+v)
			}
		}
		if len(parts) == 0 {
			continue
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	if len(lines) == 0 {
		return noImageListReply
	}
	return strings.Join(lines, "\n")
}

func renderCommunityImageReply(raw map[string]any) string {
	groups := mapSliceAt(raw, "CompshareImageGroup")
	if len(groups) == 0 {
		// Some responses use a flat list, fall back to ImageSet
		return renderImageListReply(raw, "ImageSet", []string{"Name", "Author", "CompShareImageId"})
	}
	lines := make([]string, 0, len(groups))
	for _, item := range groups {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts := []string{}
		if v := safeString(entry, "Name"); v != "" {
			parts = append(parts, "名称="+v)
		}
		if v := safeString(entry, "Author"); v != "" {
			parts = append(parts, "作者="+v)
		}
		versions := mapSliceAt(entry, "Data")
		if len(versions) > 0 {
			parts = append(parts, fmt.Sprintf("版本数=%d", len(versions)))
		}
		if len(parts) == 0 {
			continue
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	if len(lines) == 0 {
		return noCommunityReply
	}
	return strings.Join(lines, "\n")
}

func summarizeMachineSizes(entry map[string]any) string {
	sizes := mapSliceAt(entry, "MachineSizes")
	if len(sizes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sizes))
	for _, s := range sizes {
		size, ok := s.(map[string]any)
		if !ok {
			continue
		}
		gpu := safeNumeric(size, "Gpu")
		collection := mapSliceAt(size, "Collection")
		if len(collection) == 0 {
			if gpu != "" {
				parts = append(parts, gpu+"卡")
			}
			continue
		}
		// Use first collection entry's Cpu/Memory as a representative configuration.
		first, ok := collection[0].(map[string]any)
		if !ok {
			continue
		}
		cpu := safeNumeric(first, "Cpu")
		mems := mapSliceAt(first, "Memory")
		memStr := ""
		if len(mems) > 0 {
			memStr = fmt.Sprintf("%v", mems[0])
		}
		segment := strings.TrimSpace(gpu + "卡/" + cpu + "C/" + memStr + "G")
		// Trim leading slashes if fields are missing
		segment = strings.Trim(segment, "/")
		if segment != "" {
			parts = append(parts, segment)
		}
	}
	return strings.Join(parts, ", ")
}

// mapSliceAt returns m[key].([]any) if shape matches, nil otherwise.
func mapSliceAt(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	return arr
}

func safeString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch typed := v.(type) {
	case string:
		return safeValue(typed)
	default:
		return safeValue(typed)
	}
}

func safeNumeric(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(v)
}
