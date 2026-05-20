package intent

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/entity"

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

func extraHandlerActions() map[Intent][]string {
	return map[Intent][]string{
		IntentStockAvailability: {
			"DescribeCompShareImages",
			"CheckCompShareResourceCapacity",
		},
	}
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
	reply := renderGPUSpecsReply(raw, req.UserText)
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
	if reply, ok, fb := renderStockWithCapacityPrecheck(ctx, h, req, raw); fb != nil {
		return *fb
	} else if ok {
		result := HandledResult(reply)
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{})
		return result
	}
	reply := renderStockReply(raw, req.UserText)
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
	reply := renderImageListReply(raw, "ImageSet", []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"}, req.UserText)
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
	reply := renderImageListReply(raw, "ImageSet", []string{"CompShareImageId", "Name", "ImageName", "Status"}, req.UserText)
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
	reply := renderCommunityImageReply(raw, req.UserText)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

// ---- Renderers --------------------------------------------------------------
//
// L0 deterministic NL filter (PR A round 2, 2026-05-18):
//
// Capability replies do NOT pass through an LLM (engine.go's groundedRenderer
// short-circuits when Envelope == nil, which is the case for all capability
// HandlerResults). To make replies "answer the question" rather than "dump the
// full API response", each renderer applies a deterministic filter using
// req.UserText:
//
//   1. Tokenize UserText (ASCII + CJK), drop stopwords + single-char noise.
//   2. For GPU paths: match user tokens against the API-returned Name set
//      (the API drives the vocabulary; no hand-maintained GPU dictionary).
//   3. For image paths: match user tokens against entry.Name / ImageName /
//      CompShareImageName / Author (substring, case-insensitive).
//   4. Fallback rules:
//      - user mentioned a known-unavailable GPU (H100/H200) -> explicit "not
//        provided" prefix + show available list
//      - user provided keywords but none matched API result -> "not found"
//        prefix + show available list
//      - user provided no effective keywords -> show all (current behavior)
//   5. Community renderer expands Data[] inside each CompshareImageGroup to
//      include the top 3 version names, with a global 20-line cap.

const (
	noGPUSpecsReply          = "未获取到 GPU 机型规格数据。"
	noStockReply             = "未获取到机型库存数据。"
	noImageListReply         = "未获取到镜像列表。"
	noImageListNoMatchReply  = "未找到匹配的镜像。"
	noCommunityReply         = "未获取到社区镜像数据。"
	soldOutDisclaimer        = "（CompShare 平台不公开精确剩余数量，仅 Normal/SoldOut 两态。）"
	communityImageGroupLimit = 20 // upper bound on community renderer output lines
	communityVersionPerGroup = 3  // versions to show per CompshareImageGroup
)

// knownUnavailableGPUNames is a minimal list of GPU models confirmed to NOT be
// offered by CompShare. Used to produce explicit "not provided" replies. Keep
// this list intentionally small (no maintenance burden); the primary matching
// vocabulary comes from the API response itself. Expand only when a business
// owner confirms a new model belongs here.
var knownUnavailableGPUNames = []string{"H100", "H200"}

// cjkStopwords are multi-character CJK runs commonly seen in capability queries
// that should not become matchable keywords. We strip these from the user text
// BEFORE tokenizing, since Go regexp + RE2 cannot segment Chinese without a
// dictionary. After stripping, remaining CJK runs (proper nouns, image names)
// survive as tokens.
var cjkStopwords = []string{
	// query verbs / structural words
	"查询", "平台", "镜像", "列表", "有吗", "支持", "系统", "自制", "社区",
	"官方", "什么", "哪些", "请问", "是否", "我的", "我自己", "上面", "下面",
	"我有", "我账", "账下", "可以", "可不可以",
	// zone / region / locale words (capability handlers don't yet take Zone)
	"机房", "地域", "可用区",
	// common GPU question phrasing (multi-char, would otherwise survive
	// tokenization as a single CJK run and match nothing — but the test
	// surface expects them stripped so the remaining token set is clean)
	"显存", "多大", "多少", "几张", "几张卡", "张卡", "几个", "配比", "配置",
	"库存", "售罄", "价格", "价钱", "收费", "扣费", "还有", "没货",
	// common image-list question filler
	"哪种", "用过", "用什么", "好用",
}

// asciiStopwords applies to ASCII tokens (post-tokenization, post-lowercase).
var asciiStopwords = map[string]struct{}{
	"list": {}, "image": {}, "images": {}, "of": {}, "the": {}, "a": {},
	"an": {}, "what": {}, "any": {}, "is": {}, "are": {}, "have": {}, "has": {},
	"do": {}, "does": {}, "show": {}, "me": {}, "my": {}, "for": {}, "to": {},
}

var tokenSplitRegex = regexp.MustCompile(`[A-Za-z0-9_.]+|\p{Han}+`)

// pureNumericTokenRegex matches ASCII tokens consisting only of digits (no dot,
// no letters). These are too generic to use for image-name substring matching
// (e.g. "Debian 12" -> "12" silently matches "py312", "vLLM v0.12.0"). Version
// strings with dots like "22.04" are NOT pure-numeric (the dot makes them
// version-shaped) and remain useful as filter keywords.
var pureNumericTokenRegex = regexp.MustCompile(`^\d+$`)

// extractUserTokens tokenizes the user text and drops stopwords + 1-char noise.
// Returns case-normalized lowercase tokens for downstream matching. Multi-char
// CJK stopwords are removed by literal substring stripping before tokenization
// (RE2 lacks Chinese segmentation, so dictionary lookup would be the next step
// — kept out of L0 scope).
func extractUserTokens(userText string) []string {
	if strings.TrimSpace(userText) == "" {
		return nil
	}
	cleaned := userText
	for _, sw := range cjkStopwords {
		cleaned = strings.ReplaceAll(cleaned, sw, " ")
	}
	raw := tokenSplitRegex.FindAllString(cleaned, -1)
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, tok := range raw {
		if len([]rune(tok)) < 2 {
			continue
		}
		lower := strings.ToLower(tok)
		if _, ok := asciiStopwords[lower]; ok {
			continue
		}
		// Drop pure-numeric tokens (e.g. "12", "2022") — they substring-match
		// too many image names ("py312", "vLLM v0.12.0", "Windows 2022 64位").
		// Version-shaped tokens like "22.04" survive because the dot makes
		// them non-pure-numeric.
		if pureNumericTokenRegex.MatchString(lower) {
			continue
		}
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, lower)
	}
	return out
}

// detectKnownUnavailableGPUs returns names from knownUnavailableGPUNames that
// appear (case-insensitively) in the user text.
func detectKnownUnavailableGPUs(userText string) []string {
	if userText == "" {
		return nil
	}
	upper := strings.ToUpper(userText)
	out := []string{}
	for _, name := range knownUnavailableGPUNames {
		if strings.Contains(upper, name) {
			out = append(out, name)
		}
	}
	return out
}

// matchUserTokensToAPINames returns the subset of API Names (preserving case)
// that the user mentioned anywhere in their question. The API name set is the
// matching vocabulary — no hand-maintained GPU dictionary required.
func matchUserTokensToAPINames(userText string, apiNames []string) []string {
	if userText == "" || len(apiNames) == 0 {
		return nil
	}
	upper := strings.ToUpper(userText)
	matched := []string{}
	seen := map[string]struct{}{}
	for _, name := range apiNames {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		if strings.Contains(upper, strings.ToUpper(name)) {
			matched = append(matched, name)
			seen[name] = struct{}{}
		}
	}
	return matched
}

// collectAPINamesFromInstanceTypes returns the deduped set of "Name" fields
// from a DescribeAvailableCompShareInstanceTypes response.
func collectAPINamesFromInstanceTypes(items []any) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// userMentionedGPULikeToken returns true when the user text contains a token
// shaped like a GPU model (letter prefix + digits, or 4-digit number with
// optional GB suffix). Used to distinguish "user asked about a GPU but none
// matched" from "user did not ask about a specific GPU".
var gpuLikeTokenRegex = regexp.MustCompile(`(?i)\b([a-z]{1,3}\d{2,4}[a-z0-9_]*|\d{4}(?:_\d+g)?)\b`)

func userMentionedGPULikeToken(userText string) bool {
	if userText == "" {
		return false
	}
	return gpuLikeTokenRegex.MatchString(userText)
}

func renderGPUSpecsReply(raw map[string]any, userText string) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noGPUSpecsReply
	}
	apiNames := collectAPINamesFromInstanceTypes(items)
	matched := matchUserTokensToAPINames(userText, apiNames)
	unavailable := detectKnownUnavailableGPUs(userText)

	var prefix string
	filterTo := map[string]struct{}{}
	if len(unavailable) > 0 {
		prefix = strings.Join(unavailable, "、") + " 当前未在 CompShare 平台提供。以下是当前可售机型规格：\n"
	}
	if len(matched) > 0 {
		for _, m := range matched {
			filterTo[m] = struct{}{}
		}
	} else if len(unavailable) == 0 && userMentionedGPULikeToken(userText) {
		prefix = "未在当前可售机型里找到您提到的型号。以下是当前可售机型规格：\n"
	}

	lines := buildGPUSpecLines(items, filterTo)
	if len(lines) == 0 {
		if prefix != "" {
			return strings.TrimRight(prefix, "\n")
		}
		return noGPUSpecsReply
	}
	return prefix + strings.Join(lines, "\n")
}

func buildGPUSpecLines(items []any, filterTo map[string]struct{}) []string {
	lines := make([]string, 0, len(items))
	seenNames := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		if len(filterTo) > 0 {
			if _, ok := filterTo[name]; !ok {
				continue
			}
		}
		// Dedupe by Name (API returns duplicates across zones with identical
		// MachineSizes for the same model in this account/region).
		if _, ok := seenNames[name]; ok {
			continue
		}
		seenNames[name] = struct{}{}
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
	return lines
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

func renderStockReply(raw map[string]any, userText string) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noStockReply
	}
	apiNames := collectAPINamesFromInstanceTypes(items)
	matched := matchUserTokensToAPINames(userText, apiNames)
	unavailable := detectKnownUnavailableGPUs(userText)

	var prefix string
	filterTo := map[string]struct{}{}
	if len(unavailable) > 0 {
		prefix = strings.Join(unavailable, "、") + " 当前未在 CompShare 平台提供。以下是当前可售机型库存：\n"
	}
	if len(matched) > 0 {
		for _, m := range matched {
			filterTo[m] = struct{}{}
		}
	} else if len(unavailable) == 0 && userMentionedGPULikeToken(userText) {
		prefix = "未在当前可售机型里找到您提到的型号。以下是当前可售机型库存：\n"
	}

	lines := make([]string, 0, len(items))
	seenNames := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		if len(filterTo) > 0 {
			if _, ok := filterTo[name]; !ok {
				continue
			}
		}
		if _, ok := seenNames[name]; ok {
			continue // dedupe API duplicates across zones
		}
		seenNames[name] = struct{}{}
		status := safeString(entry, "Status")
		if status == "" {
			// Some prod responses omit Status; "appears in available list" ≈ available.
			status = "Normal"
		}
		lines = append(lines, renderStockStatusLine(name, status))
	}
	if len(lines) == 0 {
		if prefix != "" {
			return strings.TrimRight(prefix, "\n") + "\n" + soldOutDisclaimer
		}
		return noStockReply
	}
	return prefix + strings.Join(lines, "\n") + "\n" + soldOutDisclaimer
}

func renderStockStatusLine(name, status string) string {
	switch {
	case strings.EqualFold(status, "Normal"):
		return fmt.Sprintf("机型=%s, 状态=Normal（机型开售；不代表当前具体配置一定可创建，精确可创建性需做容量预检）", name)
	case strings.EqualFold(status, "SoldOut"):
		return fmt.Sprintf("机型=%s, 状态=SoldOut（售罄）", name)
	default:
		return fmt.Sprintf("机型=%s, 状态=%s", name, status)
	}
}

type stockInstanceTypeEntry struct {
	Name   string
	Status string
	Zone   string
}

type stockCapacityCheck struct {
	Name        string
	Zone        string
	CheckedSpec int
	EnoughSpecs []string
	Failed      bool
}

func renderStockWithCapacityPrecheck(ctx context.Context, h *DemoHandler, req HandlerRequest, stockRaw map[string]any) (string, bool, *HandlerResult) {
	entries := matchedNormalStockEntries(stockRaw, req.UserText)
	entries = filterStockEntriesToResolverZones(entries, req.Resolver)
	entries = firstStockEntryPerModel(entries)
	if len(entries) == 0 {
		return "", false, nil
	}
	imageRaw, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, "DescribeCompShareImages", map[string]any{
		"ImageType": "System",
		"Limit":     20,
	})
	if fb != nil {
		return "", false, fb
	}
	imageID := selectCapacityPrecheckImageID(imageRaw)
	if imageID == "" {
		return renderStockReply(stockRaw, req.UserText) + "\n容量预检未执行：未获取到可用于预检的系统镜像。", true, nil
	}

	checks := make([]stockCapacityCheck, 0, len(entries))
	if req.Plan.Intent != IntentStockAvailability {
		result := FallbackBeforeTool(FallbackActionNotAllowed)
		return "", false, &result
	}
	for _, entry := range entries {
		if entry.Zone == "" {
			continue
		}
		args := capacityPrecheckArgs(entry, imageID)
		capacityRaw, err := h.executor.Execute(ctx, "CheckCompShareResourceCapacity", args)
		if err != nil {
			checks = append(checks, stockCapacityCheck{Name: entry.Name, Zone: entry.Zone, Failed: true})
			continue
		}
		checks = append(checks, summarizeStockCapacity(entry, capacityRaw))
	}
	if len(checks) == 0 {
		return renderStockReply(stockRaw, req.UserText) + "\n容量预检未执行：当前接口结果缺少可用区信息。", true, nil
	}
	return renderStockCapacityReply(checks), true, nil
}

func matchedNormalStockEntries(raw map[string]any, userText string) []stockInstanceTypeEntry {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return nil
	}
	matchedNames := matchUserTokensToAPINames(userText, collectAPINamesFromInstanceTypes(items))
	if len(matchedNames) == 0 {
		return nil
	}
	wanted := map[string]struct{}{}
	for _, name := range matchedNames {
		wanted[name] = struct{}{}
	}
	out := []stockInstanceTypeEntry{}
	seen := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if _, ok := wanted[name]; !ok {
			continue
		}
		status := safeString(entry, "Status")
		if status == "" {
			status = "Normal"
		}
		if !strings.EqualFold(status, "Normal") {
			continue
		}
		zone := safeString(entry, "Zone")
		key := name + "\x00" + zone
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, stockInstanceTypeEntry{Name: name, Status: status, Zone: zone})
	}
	return out
}

func filterStockEntriesToResolverZones(entries []stockInstanceTypeEntry, resolver EntityResolver) []stockInstanceTypeEntry {
	if len(entries) == 0 {
		return entries
	}
	zones := preferredZonesFromResolver(resolver)
	if len(zones) == 0 {
		return entries
	}
	filtered := make([]stockInstanceTypeEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := zones[entry.Zone]; ok {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == 0 {
		return entries
	}
	return filtered
}

func firstStockEntryPerModel(entries []stockInstanceTypeEntry) []stockInstanceTypeEntry {
	out := make([]stockInstanceTypeEntry, 0, len(entries))
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if _, ok := seen[entry.Name]; ok {
			continue
		}
		seen[entry.Name] = struct{}{}
		out = append(out, entry)
	}
	return out
}

func preferredZonesFromResolver(resolver EntityResolver) map[string]struct{} {
	snapshot, ok := resolver.(entity.RegistrySnapshot)
	if !ok {
		return nil
	}
	out := map[string]struct{}{}
	for _, inst := range snapshot.Instances {
		if inst.Zone != "" {
			out[inst.Zone] = struct{}{}
		}
	}
	return out
}

func capacityPrecheckArgs(entry stockInstanceTypeEntry, imageID string) map[string]any {
	return map[string]any{
		"Zone":               entry.Zone,
		"GpuType":            entry.Name,
		"MachineType":        "G",
		"MinimalCpuPlatform": "Auto",
		"CompShareImageId":   imageID,
		"ChargeType":         "Dynamic",
		"Disks": []any{
			map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": 60},
		},
	}
}

func selectCapacityPrecheckImageID(raw map[string]any) string {
	items := mapSliceAt(raw, "ImageSet")
	bestID := ""
	bestScore := -1
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := safeString(entry, "CompShareImageId")
		if id == "" {
			continue
		}
		status := safeString(entry, "Status")
		if status != "" && !strings.EqualFold(status, "Available") && !strings.EqualFold(status, "Normal") {
			continue
		}
		text := strings.ToLower(strings.Join([]string{
			safeString(entry, "Name"),
			safeString(entry, "ImageName"),
			safeString(entry, "CompShareImageName"),
		}, " "))
		score := 0
		if strings.EqualFold(safeString(entry, "ImageType"), "System") {
			score += 4
		}
		if strings.Contains(text, "ubuntu") {
			score += 4
		}
		if strings.Contains(text, "nvidia") || strings.Contains(text, "cuda") {
			score += 3
		}
		if status != "" {
			score++
		}
		if score > bestScore {
			bestScore = score
			bestID = id
		}
	}
	return bestID
}

func summarizeStockCapacity(entry stockInstanceTypeEntry, raw map[string]any) stockCapacityCheck {
	check := stockCapacityCheck{Name: entry.Name, Zone: entry.Zone}
	for _, item := range mapSliceAt(raw, "Specs") {
		spec, ok := item.(map[string]any)
		if !ok {
			continue
		}
		check.CheckedSpec++
		if resourceEnough(spec["ResourceEnough"]) {
			if label := capacitySpecLabel(spec); label != "" {
				check.EnoughSpecs = append(check.EnoughSpecs, label)
			}
		}
	}
	return check
}

func resourceEnough(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func capacitySpecLabel(spec map[string]any) string {
	gpu := fmt.Sprint(spec["Gpu"])
	cpu := fmt.Sprint(spec["Cpu"])
	mem := fmt.Sprint(spec["Mem"])
	parts := []string{}
	if gpu != "" && gpu != "<nil>" {
		parts = append(parts, gpu+"卡")
	}
	if cpu != "" && cpu != "<nil>" {
		parts = append(parts, cpu+"C")
	}
	if mem != "" && mem != "<nil>" {
		parts = append(parts, mem+"G")
	}
	return strings.Join(parts, "/")
}

func renderStockCapacityReply(checks []stockCapacityCheck) string {
	names := make([]string, 0, len(checks))
	seenNames := map[string]struct{}{}
	var enough []string
	var failedZones []string
	checkedSpecs := 0
	for _, check := range checks {
		if _, ok := seenNames[check.Name]; !ok {
			seenNames[check.Name] = struct{}{}
			names = append(names, check.Name)
		}
		if check.Failed {
			failedZones = append(failedZones, check.Zone)
			continue
		}
		checkedSpecs += check.CheckedSpec
		for _, spec := range check.EnoughSpecs {
			enough = append(enough, fmt.Sprintf("%s/%s/%s", check.Name, check.Zone, spec))
		}
	}
	sort.Strings(names)
	models := strings.Join(names, "、")
	if len(enough) > 0 {
		sort.Strings(enough)
		reply := fmt.Sprintf("%s 当前有可创建库存，可以新建实例。", models)
		return appendCapacityFailureNote(reply, failedZones)
	}
	if checkedSpecs == 0 {
		reply := fmt.Sprintf("%s 当前暂时无法确认是否有可创建库存。", models)
		return appendCapacityFailureNote(reply, failedZones)
	}
	reply := fmt.Sprintf("%s 当前暂无可创建库存，暂时不能新建实例。", models)
	return appendCapacityFailureNote(reply, failedZones)
}

func appendCapacityFailureNote(reply string, failedZones []string) string {
	if len(failedZones) == 0 {
		return reply
	}
	sort.Strings(failedZones)
	return reply + " 另有部分可用区暂时无法确认。"
}

// entryMatchesAnyKeyword returns true when any of the user keywords appears
// (substring, case-insensitive) in any of the named entry fields.
func entryMatchesAnyKeyword(entry map[string]any, keywords []string, fields []string) bool {
	for _, k := range keywords {
		for _, f := range fields {
			v, ok := entry[f].(string)
			if !ok || v == "" {
				continue
			}
			if strings.Contains(strings.ToLower(v), k) {
				return true
			}
		}
	}
	return false
}

func renderImageListReply(raw map[string]any, listKey string, fieldOrder []string, userText string) string {
	items := mapSliceAt(raw, listKey)
	if len(items) == 0 {
		return noImageListReply
	}
	keywords := extractUserTokens(userText)
	// Match keywords against name-like fields only (not status/id/type).
	matchFields := []string{}
	for _, f := range fieldOrder {
		switch f {
		case "Name", "ImageName", "CompShareImageName", "Author":
			matchFields = append(matchFields, f)
		}
	}

	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if len(keywords) > 0 && len(matchFields) > 0 {
			if !entryMatchesAnyKeyword(entry, keywords, matchFields) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	// "keywords > 0 + 0 matches" -> explicit not-found, do not silently fall
	// through to the full list (that's what confused users in round 1 smoke).
	if len(keywords) > 0 && len(filtered) == 0 {
		return noImageListNoMatchReply
	}
	lines := make([]string, 0, len(filtered))
	for _, entry := range filtered {
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

func renderCommunityImageReply(raw map[string]any, userText string) string {
	groups := mapSliceAt(raw, "CompshareImageGroup")
	if len(groups) == 0 {
		// Fallback: some responses use a flat ImageSet shape.
		return renderImageListReply(raw, "ImageSet",
			[]string{"Name", "Author", "CompShareImageId"}, userText)
	}
	keywords := extractUserTokens(userText)
	matchFields := []string{"Name", "Author"}

	filtered := make([]map[string]any, 0, len(groups))
	for _, item := range groups {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if len(keywords) > 0 {
			// Match keywords against group-level Name/Author or any version's Name/Author.
			if !entryMatchesAnyKeyword(entry, keywords, matchFields) &&
				!anyVersionMatches(mapSliceAt(entry, "Data"), keywords, matchFields) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	if len(keywords) > 0 && len(filtered) == 0 {
		return noImageListNoMatchReply
	}

	lines := make([]string, 0, communityImageGroupLimit)
	lineBudget := communityImageGroupLimit
	for _, entry := range filtered {
		if lineBudget <= 0 {
			break
		}
		header := buildCommunityGroupHeader(entry)
		if header == "" {
			continue
		}
		lines = append(lines, header)
		lineBudget--

		versions := mapSliceAt(entry, "Data")
		shown := 0
		for _, v := range versions {
			if lineBudget <= 0 {
				break
			}
			if shown >= communityVersionPerGroup {
				if len(versions) > shown {
					lines = append(lines, fmt.Sprintf("  ... 共 %d 个版本", len(versions)))
					lineBudget--
				}
				break
			}
			ver, ok := v.(map[string]any)
			if !ok {
				continue
			}
			versionLine := buildCommunityVersionLine(ver)
			if versionLine == "" {
				continue
			}
			lines = append(lines, "  "+versionLine)
			lineBudget--
			shown++
		}
	}
	if len(lines) == 0 {
		return noCommunityReply
	}
	return strings.Join(lines, "\n")
}

func buildCommunityGroupHeader(entry map[string]any) string {
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
	return strings.Join(parts, ", ")
}

func buildCommunityVersionLine(ver map[string]any) string {
	parts := []string{}
	for _, key := range []string{"CompShareImageId", "Name", "VersionName", "Version"} {
		if v := safeString(ver, key); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

func anyVersionMatches(versions []any, keywords []string, fields []string) bool {
	for _, v := range versions {
		ver, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if entryMatchesAnyKeyword(ver, keywords, fields) {
			return true
		}
	}
	return false
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
