package intent

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"

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
	{IntentPricingQuery, "GetCompShareInstancePrice", handlePricingQuery},
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
	Name              string                     `yaml:"name"`
	IntentLabel       string                     `yaml:"intent_label"`
	RequiredTool      string                     `yaml:"required_tool"`
	RequiredCitation  bool                       `yaml:"required_citation"`
	PlannerDirectives []string                   `yaml:"planner_directives"`
	PlannerExamples   []CapabilityPlannerExample `yaml:"planner_examples"`
	Body              string                     `yaml:"-"`
}

type CapabilityPlannerExample struct {
	Question   string  `yaml:"question"`
	Confidence float64 `yaml:"confidence"`
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
	metaByIntent := map[Intent]CapabilityMetadata{}
	for _, m := range loaded {
		intentValue := Intent(m.IntentLabel)
		metaSet[intentValue] = struct{}{}
		metaByIntent[intentValue] = m
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
	ordered := make([]CapabilityMetadata, 0, len(capabilityRegistry))
	for _, e := range capabilityRegistry {
		meta := metaByIntent[e.intent]
		if meta.RequiredTool != e.requiredTool {
			panic(fmt.Sprintf("intent: capability %q required_tool=%q does not match registry tool %q", e.intent, meta.RequiredTool, e.requiredTool))
		}
		ordered = append(ordered, meta)
	}
	return ordered
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
		if len(meta.PlannerDirectives) == 0 || len(meta.PlannerExamples) == 0 {
			return nil, fmt.Errorf("parse %s: planner_directives and planner_examples must be non-empty", name)
		}
		for i, directive := range meta.PlannerDirectives {
			if strings.TrimSpace(directive) == "" {
				return nil, fmt.Errorf("parse %s: planner_directives[%d] must be non-empty", name, i)
			}
		}
		for i, example := range meta.PlannerExamples {
			if strings.TrimSpace(example.Question) == "" {
				return nil, fmt.Errorf("parse %s: planner_examples[%d].question must be non-empty", name, i)
			}
			if example.Confidence <= 0 || example.Confidence > 1 {
				return nil, fmt.Errorf("parse %s: planner_examples[%d].confidence must be in (0,1]", name, i)
			}
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
	decoder := yaml.NewDecoder(bytes.NewReader([]byte(frontmatter)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&meta); err != nil {
		return CapabilityMetadata{}, fmt.Errorf("yaml unmarshal: %w", err)
	}
	meta.Body = body
	return meta, nil
}

// CapabilityPromptFragments returns planner-prompt directives + one-shot
// examples derived from internal/intent/capabilities/*.md frontmatter.
func CapabilityPromptFragments() ([]string, []string) {
	names := make([]string, 0, len(capabilityMetadata))
	for _, m := range capabilityMetadata {
		names = append(names, m.Name)
	}
	directives := []string{
		fmt.Sprintf("Stage 2C capability routing: classify clear platform %s questions to the matching capability intent.", strings.Join(names, " / ")),
	}
	examples := []string{}
	for _, m := range capabilityMetadata {
		directives = append(directives, m.PlannerDirectives...)
		for _, example := range m.PlannerExamples {
			examples = append(examples, "User question: "+example.Question)
			examples = append(examples, capabilityPromptExampleJSON(m, example))
		}
	}
	return directives, examples
}

func capabilityPromptExampleJSON(meta CapabilityMetadata, example CapabilityPlannerExample) string {
	type promptSlots struct {
		TargetRefs []TargetRef `json:"target_refs"`
		Metrics    []Metric    `json:"metrics"`
		TimeWindow *TimeWindow `json:"time_window"`
	}
	type promptPlan struct {
		SchemaVersion string      `json:"schema_version"`
		Intent        Intent      `json:"intent"`
		Slots         promptSlots `json:"slots"`
		RequiredTools []string    `json:"required_tools"`
		Retrieval     Retrieval   `json:"retrieval"`
		HardBlockHint bool        `json:"hard_block_hint"`
		Confidence    float64     `json:"confidence"`
	}
	plan := promptPlan{
		SchemaVersion: SchemaVersion,
		Intent:        Intent(meta.IntentLabel),
		Slots: promptSlots{
			TargetRefs: []TargetRef{},
			Metrics:    []Metric{},
			TimeWindow: nil,
		},
		RequiredTools: []string{meta.RequiredTool},
		Retrieval:     Retrieval{Enabled: false},
		HardBlockHint: false,
		Confidence:    example.Confidence,
	}
	data, err := json.Marshal(plan)
	if err != nil {
		panic(fmt.Sprintf("intent: marshal capability planner example %q: %v", meta.Name, err))
	}
	return string(data)
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
	env := buildGPUSpecsEnvelope(raw, req.UserText)
	result.Envelope = &env
	result.RendererInputEnvelopeHashes = hashEnvelopeForRenderer(env)
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
// appear (case-insensitively, word-bounded) in the user text. Word boundaries
// avoid the same substring trap as matchUserTokensToAPINames — keeps the
// behaviour consistent if knownUnavailableGPUNames ever gains a shorter
// entry that could prefix another known name.
func detectKnownUnavailableGPUs(userText string) []string {
	if userText == "" {
		return nil
	}
	upper := strings.ToUpper(userText)
	out := []string{}
	for _, name := range knownUnavailableGPUNames {
		if containsAsWord(upper, name) {
			out = append(out, name)
		}
	}
	return out
}

// matchUserTokensToAPINames returns the subset of API Names (preserving case)
// that the user mentioned anywhere in their question. The API name set is the
// matching vocabulary — no hand-maintained GPU dictionary required.
//
// Word boundaries are required on both sides so a shorter model name does not
// substring-match a longer one — e.g. "H20" must not match "H200 96G". Word
// chars are [0-9A-Za-z_]; CJK and space are non-word, so a name surrounded by
// space/Chinese matches as expected.
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
		if containsAsWord(upper, strings.ToUpper(name)) {
			matched = append(matched, name)
			seen[name] = struct{}{}
		}
	}
	return matched
}

// containsAsWord reports whether needle appears in haystack with word
// boundaries on both sides. A word char is [0-9A-Za-z_]; any other rune
// (including CJK, space, punctuation, start/end of string) counts as a
// boundary. Substring matches like "H20" inside "H200" return false.
func containsAsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	from := 0
	for from <= len(haystack)-len(needle) {
		idx := strings.Index(haystack[from:], needle)
		if idx < 0 {
			return false
		}
		abs := from + idx
		if !isWordCharBefore(haystack, abs) && !isWordCharAfter(haystack, abs+len(needle)) {
			return true
		}
		from = abs + 1
	}
	return false
}

func isWordCharBefore(s string, pos int) bool {
	if pos <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:pos])
	return isWordRune(r)
}

func isWordCharAfter(s string, pos int) bool {
	if pos >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[pos:])
	return isWordRune(r)
}

func isWordRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

var gpuMemoryHintRegex = regexp.MustCompile(`(?i)\b(\d{2,3})\s*(?:gb|g)\b`)
var gpuMemorySuffixRegex = regexp.MustCompile(`(?i)_(\d{2,3})g$`)

func matchUserTextToInstanceTypeNames(userText string, items []any, includeFamilyMemoryVariants bool) []string {
	apiNames := collectAPINamesFromInstanceTypes(items)
	matched := matchUserTokensToAPINames(userText, apiNames)
	hints := extractGPUMemoryHints(userText)
	if len(hints) > 0 {
		// Fail-closed: if the user named a GPU-shaped token but none of them
		// matched a known API name, do NOT fall back to memory-only matching
		// (that would surface a different GPU model with the same VRAM, e.g.
		// "H200 96G" → H20_96G). The caller's renderStockReply path will then
		// apply the known-unavailable prefix when appropriate.
		if len(matched) == 0 && userMentionedGPULikeToken(userText) {
			return nil
		}
		if memoryMatched := matchMemoryHintedInstanceTypeNames(hints, items, matched); len(memoryMatched) > 0 {
			return memoryMatched
		}
		if len(matched) > 0 || userMentionedGPULikeToken(userText) {
			return nil
		}
	}
	if includeFamilyMemoryVariants {
		return expandMemoryVariantMatches(matched, apiNames)
	}
	return matched
}

func matchMemoryHintedInstanceTypeNames(hints map[string]struct{}, items []any, matchedNames []string) []string {
	wantedBases := map[string]struct{}{}
	for _, name := range matchedNames {
		if name == "" {
			continue
		}
		wantedBases[name] = struct{}{}
		wantedBases[memoryVariantBaseName(name)] = struct{}{}
	}

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
		if len(wantedBases) > 0 {
			base := memoryVariantBaseName(name)
			if _, ok := wantedBases[name]; !ok {
				if _, ok := wantedBases[base]; !ok {
					continue
				}
			}
		}
		if !memoryHintMatchesInstanceType(hints, entry) {
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

func extractGPUMemoryHints(userText string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, match := range gpuMemoryHintRegex.FindAllStringSubmatch(userText, -1) {
		if len(match) < 2 {
			continue
		}
		if normalized := normalizeMemoryGB(match[1]); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}

func memoryHintMatchesInstanceType(hints map[string]struct{}, entry map[string]any) bool {
	memory := normalizeMemoryGB(nestedValue(entry, "GraphicsMemory"))
	if memory == "" {
		memory = apiNameMemoryGB(safeString(entry, "Name"))
	}
	if memory == "" {
		return false
	}
	_, ok := hints[memory]
	return ok
}

func normalizeMemoryGB(value string) string {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	normalized = strings.TrimSuffix(normalized, "GB")
	normalized = strings.TrimSuffix(normalized, "G")
	return strings.TrimSpace(normalized)
}

func apiNameMemoryGB(name string) string {
	match := gpuMemorySuffixRegex.FindStringSubmatch(name)
	if len(match) < 2 {
		return ""
	}
	return normalizeMemoryGB(match[1])
}

func memoryVariantBaseName(name string) string {
	return gpuMemorySuffixRegex.ReplaceAllString(name, "")
}

func expandMemoryVariantMatches(matchedNames []string, apiNames []string) []string {
	if len(matchedNames) == 0 {
		return nil
	}
	wantedNames := map[string]struct{}{}
	wantedBases := map[string]struct{}{}
	for _, name := range matchedNames {
		if name == "" {
			continue
		}
		wantedNames[name] = struct{}{}
		wantedBases[memoryVariantBaseName(name)] = struct{}{}
	}

	out := []string{}
	seen := map[string]struct{}{}
	for _, name := range apiNames {
		_, exact := wantedNames[name]
		_, variant := wantedBases[memoryVariantBaseName(name)]
		if !exact && !(variant && apiNameMemoryGB(name) != "") {
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

func fullGPUSpecsRequest(userText string) bool {
	normalized := strings.ToLower(strings.TrimSpace(userText))
	if normalized == "" {
		return false
	}
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(normalized)
	hasFullQualifier := containsAny(compact, "所有", "全部", "完整", "全量", "每种") ||
		containsAnyEnglishWord(normalized, "all", "full", "complete", "entire", "every")
	hasSpecsTerm := containsAny(compact, "规格", "配置", "配比") ||
		strings.Contains(normalized, "spec") ||
		strings.Contains(normalized, "config") ||
		strings.Contains(normalized, "machine size")
	if hasFullQualifier && hasSpecsTerm {
		return true
	}
	return containsAny(compact,
		"cpu/内存",
		"cpu内存",
		"cpu和内存",
		"cpu与内存",
		"cpu及内存",
		"cpu、内存",
		"cpu核心和内存",
		"cpu核数和内存",
		"内存组合",
		"配置组合",
		"合法配置",
		"可选配置",
		"可用配置",
	) || strings.Contains(normalized, "cpu/memory") ||
		strings.Contains(normalized, "cpu memory") ||
		strings.Contains(normalized, "memory options") ||
		strings.Contains(normalized, "configuration options") ||
		strings.Contains(normalized, "machine sizes")
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func containsAnyEnglishWord(s string, words ...string) bool {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, field := range fields {
		for _, word := range words {
			if field == word {
				return true
			}
		}
	}
	return false
}

func renderGPUSpecsReply(raw map[string]any, userText string) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noGPUSpecsReply
	}
	matched := matchUserTextToInstanceTypeNames(userText, items, true)
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

	detailed := fullGPUSpecsRequest(userText)
	lines := buildGPUSpecLines(items, filterTo, detailed)
	if len(lines) == 0 {
		if prefix != "" {
			return strings.TrimRight(prefix, "\n")
		}
		return noGPUSpecsReply
	}
	return prefix + strings.Join(lines, "\n")
}

func buildGPUSpecLines(items []any, filterTo map[string]struct{}, detailed bool) []string {
	lines := make([]string, 0, len(items))
	seenNames := map[string]struct{}{}
	seenDetailed := map[string]struct{}{}
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
		if detailed {
			key := name + "\x00" + safeString(entry, "Zone") + "\x00" + expandMachineSizes(entry)
			if _, ok := seenDetailed[key]; ok {
				continue
			}
			seenDetailed[key] = struct{}{}
		} else {
			// Dedupe by Name for overview replies so a plain spec question stays
			// concise even if the API returns the same model in multiple zones.
			if _, ok := seenNames[name]; ok {
				continue
			}
			seenNames[name] = struct{}{}
		}
		parts := []string{"机型=" + name}
		if detailed {
			if zone := safeString(entry, "Zone"); zone != "" {
				parts = append(parts, "可用区="+zone)
			}
		}
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
		if detailed {
			if sizes := expandMachineSizes(entry); sizes != "" {
				parts = append(parts, "完整配置="+sizes)
			}
		} else if maxGPU := maxGPUFromMachineSizes(entry); maxGPU != "" {
			parts = append(parts, "最大卡数="+maxGPU)
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	return lines
}

func buildGPUSpecsEnvelope(raw map[string]any, userText string) envelope.Envelope {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	matched := matchUserTextToInstanceTypeNames(userText, items, true)
	filterTo := map[string]struct{}{}
	for _, m := range matched {
		filterTo[m] = struct{}{}
	}
	detailed := fullGPUSpecsRequest(userText)
	entries := selectGPUSpecEntries(items, filterTo, detailed)

	env := envelope.Envelope{
		Kind:          envelope.KindGPUSpecsQuery,
		SourceActions: []string{"DescribeAvailableCompShareInstanceTypes"},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints: envelope.Constraints{
			DoNotInventInstances:   true,
			DoNotAnswerAccountBill: true,
		},
	}
	answerMode := "overview"
	if detailed {
		answerMode = "full_specs"
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "answer_mode", Label: "Answer mode", Value: answerMode, Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "requested_gpu_specs", Label: "User question", Value: userText, Source: envelope.FactSourceComputed},
	)

	seenSubjects := map[string]struct{}{}
	for _, entry := range entries {
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		subjectID := "gpu_model:" + name
		if _, ok := seenSubjects[subjectID]; !ok {
			seenSubjects[subjectID] = struct{}{}
			env.Subjects = append(env.Subjects, envelope.Subject{
				ID:   subjectID,
				Name: name,
				Type: envelope.SubjectGPUModel,
			})
		}
		addFact := func(key, label string, value any, unit string) {
			valueString := safeValue(value)
			if strings.TrimSpace(valueString) == "" {
				return
			}
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID,
				Key:       key,
				Label:     label,
				Value:     valueString,
				Unit:      unit,
				Source:    envelope.FactSourceAPI,
			})
		}
		addFact("model_name", "机型", name, "")
		if detailed {
			addFact("zone", "可用区", safeString(entry, "Zone"), "")
		}
		addFact("performance", "性能", nestedValue(entry, "Performance"), "")
		addFact("graphics_memory", "显存", nestedValue(entry, "GraphicsMemory"), "GB")
		addFact("status", "状态", safeString(entry, "Status"), "")
		if detailed {
			addFact("machine_size_configs", "完整配置", expandMachineSizes(entry), "")
		} else {
			addFact("max_gpu_count", "最大卡数", maxGPUFromMachineSizes(entry), "卡")
		}
	}
	return env
}

func selectGPUSpecEntries(items []any, filterTo map[string]struct{}, detailed bool) []map[string]any {
	entries := make([]map[string]any, 0, len(items))
	seenNames := map[string]struct{}{}
	seenDetailed := map[string]struct{}{}
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
		if detailed {
			key := name + "\x00" + safeString(entry, "Zone") + "\x00" + expandMachineSizes(entry)
			if _, ok := seenDetailed[key]; ok {
				continue
			}
			seenDetailed[key] = struct{}{}
		} else {
			if _, ok := seenNames[name]; ok {
				continue
			}
			seenNames[name] = struct{}{}
		}
		entries = append(entries, entry)
	}
	return entries
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
	matched := matchUserTextToInstanceTypeNames(userText, items, false)
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
	entriesByModel := map[string][]stockInstanceTypeEntry{}
	modelOrder := []string{}
	for _, entry := range entries {
		if _, ok := entriesByModel[entry.Name]; !ok {
			modelOrder = append(modelOrder, entry.Name)
		}
		entriesByModel[entry.Name] = append(entriesByModel[entry.Name], entry)
	}

	for _, model := range modelOrder {
		zoneEntries := entriesByModel[model]
		var firstZone string
		var success stockCapacityCheck
		sawSuccess := false
		for _, entry := range zoneEntries {
			if entry.Zone == "" {
				continue
			}
			if firstZone == "" {
				firstZone = entry.Zone
			}
			args := capacityPrecheckArgs(entry, imageID)
			capacityRaw, err := h.executor.Execute(ctx, "CheckCompShareResourceCapacity", args)
			if err != nil {
				continue
			}
			success = summarizeStockCapacity(entry, capacityRaw)
			sawSuccess = true
			break
		}
		if sawSuccess {
			checks = append(checks, success)
		} else if firstZone != "" {
			checks = append(checks, stockCapacityCheck{Name: model, Zone: firstZone, Failed: true})
		}
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
	matchedNames := matchUserTextToInstanceTypeNames(userText, items, false)
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

func expandMachineSizes(entry map[string]any) string {
	sizes := mapSliceAt(entry, "MachineSizes")
	if len(sizes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sizes))
	seen := map[string]struct{}{}
	for _, s := range sizes {
		size, ok := s.(map[string]any)
		if !ok {
			continue
		}
		gpu := safeNumeric(size, "Gpu")
		collection := mapSliceAt(size, "Collection")
		if len(collection) == 0 {
			appendUniqueMachineSize(&parts, seen, formatMachineSizeSegment(gpu, "", ""))
			continue
		}
		for _, c := range collection {
			combo, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cpu := safeNumeric(combo, "Cpu")
			mems := mapSliceAt(combo, "Memory")
			if len(mems) == 0 {
				appendUniqueMachineSize(&parts, seen, formatMachineSizeSegment(gpu, cpu, ""))
				continue
			}
			for _, mem := range mems {
				appendUniqueMachineSize(&parts, seen, formatMachineSizeSegment(gpu, cpu, fmt.Sprint(mem)))
			}
		}
	}
	return strings.Join(parts, ", ")
}

func maxGPUFromMachineSizes(entry map[string]any) string {
	sizes := mapSliceAt(entry, "MachineSizes")
	if len(sizes) == 0 {
		return ""
	}
	maxLabel := ""
	var maxValue float64
	hasNumeric := false
	for _, s := range sizes {
		size, ok := s.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := size["Gpu"]
		if !ok {
			continue
		}
		label := fmt.Sprint(raw)
		value, numeric := numericValue(raw)
		if numeric {
			if !hasNumeric || value > maxValue {
				maxValue = value
				maxLabel = label
				hasNumeric = true
			}
			continue
		}
		if maxLabel == "" {
			maxLabel = label
		}
	}
	return maxLabel
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func appendUniqueMachineSize(parts *[]string, seen map[string]struct{}, segment string) {
	segment = strings.Trim(segment, "/")
	if segment == "" {
		return
	}
	if _, ok := seen[segment]; ok {
		return
	}
	seen[segment] = struct{}{}
	*parts = append(*parts, segment)
}

func formatMachineSizeSegment(gpu, cpu, memory string) string {
	parts := []string{}
	if gpu != "" {
		parts = append(parts, gpu+"卡")
	}
	if cpu != "" {
		parts = append(parts, cpu+"C")
	}
	if memory != "" {
		parts = append(parts, memory+"G")
	}
	return strings.Join(parts, "/")
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
