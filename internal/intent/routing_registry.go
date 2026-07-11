package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/routing"
	"github.com/compshare-agent/internal/zones"
)

// routingIntentOrder is the registration order of the catalog/status route
// intents. It is the ONLY remnant of the deleted routeRegistry: the planner
// prompt fragments are emitted in this order and the order is byte-identity-pinned
// (NOT alphabetical). Handler binding, required tool, and metadata now come from
// the generated skill registry (skills.GeneratedSkills()); only the order lives here.
var routingIntentOrder = []Intent{
	IntentGPUSpecsQuery,
	IntentStockAvailability,
	IntentNetAcceleratorStatus,
	IntentRefundEstimate,
	IntentCFSInfo,
	IntentImageTagCatalog,
	IntentModelRepositoryBrowse,
	IntentImageList,
	IntentPricingQuery,
}

func extraHandlerActions() map[Intent][]string {
	return map[Intent][]string{
		IntentStockAvailability: {
			"DescribeCompShareSupportZone",
			"DescribeCompShareGpuInventory",
			"DescribeCompShareImages",
			"CheckCompShareResourceCapacity",
		},
		IntentModelRepositoryBrowse: {
			"DescribeModelRepositoryTags",
		},
		IntentImageList: {
			"DescribeCompShareCustomImages",
			"DescribeCommunityImages",
			"DescribeCompShareSharingImages",
		},
	}
}

// IsRoutingIntent reports whether the intent is served by the route
// registry (vs. legacy IntentResourceInfo/MonitorQuery or RAG-bound knowledge_qa).
// Engine.go uses this single predicate to gate route dispatch.
func IsRoutingIntent(i Intent) bool {
	return isRoutingIntentRoute(i)
}

// RoutingIntents returns the set of route Intents in registration order.
// Used by planner prompt build + cmd/trace parsing.
func RoutingIntents() []Intent {
	return append([]Intent(nil), routingIntentOrder...)
}

func routingRequiredTool(i Intent) (string, bool) {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == string(i) && len(route.RequiredTools) > 0 {
			return route.RequiredTools[0], true
		}
	}
	return "", false
}

// DispatchRoute resolves a route intent to its registered handler.
// Returns FallbackBeforeTool(validation) if the intent is not registered — this
// is unreachable when engine.go gates on IsRoutingIntent first.
func (h *DemoHandler) DispatchRoute(ctx context.Context, req HandlerRequest) HandlerResult {
	return h.dispatchRoute(ctx, req)
}

// RouteMetadata is the planner-prompt projection of a route skill.
// Stored only for planner prompt construction; runtime dispatch resolves the
// handler via the generated skill registry (skills.GeneratedSkills()).
type RouteMetadata struct {
	Name              string
	IntentLabel       string
	SkillGroup        string
	RequiredTool      string
	ToolSubset        []string
	RequiredCitation  bool
	PlannerDirectives []string
	PlannerExamples   []RoutePlannerExample
}

type RoutePlannerExample struct {
	Question    string
	Confidence  float64
	ImageSource ImageSource
	SearchQuery string
	ListMode    ListMode
	PriceKind   PriceKind
	CFSKind     CFSKind
	SizeGB      int
	Zone        string
	ChargeType  string
	DetailLevel DetailLevel
}

// RoutingPromptFragments returns planner-prompt directives + one-shot
// examples derived from the generated skill registry (the sole route
// source). The fragment order follows routingIntentOrder.
func RoutingPromptFragments() ([]string, []string) {
	return routingPromptFragmentsFrom(skillRegistryRouteMetadata())
}

// routingPromptFragmentsFrom is the pure builder underlying
// RoutingPromptFragments: it derives the directives + one-shot examples from
// the given metadata slice (in slice order). Kept source-parameterized so tests
// can feed it a known metadata slice independently of the live skill registry.
func routingPromptFragmentsFrom(meta []RouteMetadata) ([]string, []string) {
	names := make([]string, 0, len(meta))
	for _, m := range meta {
		names = append(names, m.Name)
	}
	directives := []string{
		fmt.Sprintf("Stage 2C platform routing: classify clear platform %s questions to the matching route intent.", strings.Join(names, " / ")),
	}
	examples := []string{}
	for _, m := range meta {
		directives = append(directives, m.PlannerDirectives...)
		for _, example := range m.PlannerExamples {
			examples = append(examples, "User question: "+example.Question)
			examples = append(examples, routingPromptExampleJSON(m, example))
		}
	}
	return directives, examples
}

func routingPromptExampleJSON(meta RouteMetadata, example RoutePlannerExample) string {
	type promptSlots struct {
		TargetRefs  []TargetRef `json:"target_refs"`
		Metrics     []Metric    `json:"metrics"`
		TimeWindow  *TimeWindow `json:"time_window"`
		ImageSource ImageSource `json:"image_source,omitempty"`
		SearchQuery string      `json:"search_query,omitempty"`
		ListMode    ListMode    `json:"list_mode,omitempty"`
		PriceKind   PriceKind   `json:"price_kind,omitempty"`
		CFSKind     CFSKind     `json:"cfs_kind,omitempty"`
		SizeGB      int         `json:"size_gb,omitempty"`
		Zone        string      `json:"zone,omitempty"`
		ChargeType  string      `json:"charge_type,omitempty"`
		DetailLevel DetailLevel `json:"detail_level,omitempty"`
	}
	type promptPlan struct {
		SchemaVersion string      `json:"schema_version"`
		Intent        Intent      `json:"intent"`
		Slots         promptSlots `json:"slots"`
		Confidence    float64     `json:"confidence"`
	}
	plan := promptPlan{
		SchemaVersion: SchemaVersion,
		Intent:        Intent(meta.IntentLabel),
		Slots: promptSlots{
			TargetRefs:  []TargetRef{},
			Metrics:     []Metric{},
			TimeWindow:  nil,
			ImageSource: ImageSource(example.ImageSource),
			SearchQuery: example.SearchQuery,
			ListMode:    example.ListMode,
			PriceKind:   example.PriceKind,
			CFSKind:     example.CFSKind,
			SizeGB:      example.SizeGB,
			Zone:        example.Zone,
			ChargeType:  example.ChargeType,
			DetailLevel: example.DetailLevel,
		},
		Confidence: example.Confidence,
	}
	data, err := json.Marshal(plan)
	if err != nil {
		panic(fmt.Sprintf("intent: marshal route planner example %q: %v", meta.Name, err))
	}
	return string(data)
}

// ---- Handler implementations ------------------------------------------------

func executeRouteAction(ctx context.Context, h *DemoHandler, intentValue Intent, action string, args map[string]any) (map[string]any, *HandlerResult) {
	// Design choice: route handlers use two-layer defense rather than the
	// three-layer pattern of legacy handlers (HandleResourceInfo etc.):
	//   layer 1: compile-time `const action` binding inside each route handler
	//   layer 2: SafeToolExecutor.PolicyForAction gate at the runtime boundary
	// We deliberately skip layer 3 (RequireAllowedHandlerAction reading
	// handlerActionWhitelist) because the generated skill registry IS the binding
	// spec — calling it here would be redundant. The whitelist is derived from the
	// same skill registry, and its exact contents are pinned by
	// TestHandlerActionWhitelist_ExactGoldenSet.
	if h == nil || h.executor == nil {
		// Defensive: production wiring must construct the handler with a
		// SafeToolExecutor adapter before enabling route dispatch.
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

func executeRouteActionInternal(ctx context.Context, h *DemoHandler, intentValue Intent, action string, args map[string]any) (map[string]any, *HandlerResult) {
	if h == nil || h.executor == nil {
		fb := FallbackBeforeTool(FallbackValidation)
		return nil, &fb
	}
	if exec, ok := h.executor.(internalHandlerExecutor); ok {
		raw, err := exec.ExecuteInternal(ctx, action, args)
		if err != nil {
			fail := failureAfterToolForError(action, args, string(intentValue), err)
			return nil, &fail
		}
		if raw == nil {
			raw = map[string]any{}
		}
		return raw, nil
	}
	return executeRouteAction(ctx, h, intentValue, action, args)
}

func slotSearchQuery(slots Slots) string {
	return strings.TrimSpace(slots.SearchQuery)
}

func slotFilterQuery(slots Slots) string {
	if slots.ListMode == ListModeAll {
		return ""
	}
	return slotSearchQuery(slots)
}

func entryMatchesSlotQuery(entry map[string]any, query string, fields []string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(safeString(entry, field)), query) {
			return true
		}
	}
	return false
}

func anyVersionMatchesSlotQuery(versions []any, query string, fields []string) bool {
	for _, item := range versions {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if entryMatchesSlotQuery(entry, query, fields) {
			return true
		}
	}
	return false
}

func handleGPUSpecsQuery(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeAvailableCompShareInstanceTypes"
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderGPUSpecsReply(raw, req.Plan.Slots, req.UserText)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	env := buildGPUSpecsEnvelope(raw, req.Plan.Slots, req.UserText)
	result.Envelope = &env
	result.RendererInputEnvelopeHashes = hashEnvelopeForRenderer(env)
	return result
}

func handleStockAvailability(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeAvailableCompShareInstanceTypes"
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	// RC017: resolve a subject-eliding follow-up ("现在还有库存吗") to the GPU
	// model a prior stock turn resolved to (req.FallbackGpuModel), so it is
	// not re-expanded to every model. All three stock renderers
	// (renderStockWithCapacityPrecheck / renderStockReply / buildStockEnvelope)
	// filter off the user text, so substituting one effective text keeps them
	// consistent. resolved is recorded back into SessionState by the engine.
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	effReq := req
	effReq.UserText = stockReferentText(req, items)
	resolved := singleStockModel(effReq.UserText, items)

	if reply, ok, fb := renderStockWithCapacityPrecheck(ctx, h, effReq, raw); fb != nil {
		return *fb
	} else if ok {
		result := HandledResult(reply)
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{})
		result.ResolvedStockGpuModel = resolved
		return result
	}
	reply := renderStockReply(raw, effReq.UserText)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	result.ResolvedStockGpuModel = resolved
	setEnvelopeIfPopulated(&result, buildStockEnvelope(raw, effReq.UserText))
	return result
}

// stockReferentText returns the user text the stock renderers should filter
// on. When the current turn already names an available model, that text is
// authoritative. When the turn elides the subject entirely — no GPU-like
// token at all — and a prior stock turn in this session resolved to a single
// model (req.FallbackGpuModel) that is STILL offered, the prior model name is
// used as the referent (RC017). The "still offered" check uses the same
// matcher as rendering, so a retired model never resurrects, and a turn that
// names an unknown/unavailable GPU ("H200") falls through to the normal
// not-found path rather than silently swapping to the prior model.
func stockReferentText(req HandlerRequest, items []any) string {
	search := slotSearchQuery(req.Plan.Slots)
	if search != "" {
		return search
	}
	if req.FallbackGpuModel == "" {
		return ""
	}
	if len(matchUserTextToInstanceTypeNames(req.FallbackGpuModel, items, false)) == 0 {
		return ""
	}
	return req.FallbackGpuModel
}

// singleStockModel returns the model name when the text resolves to exactly
// one available model, else "". Only an unambiguous single referent is
// recorded — a list-all or multi-model turn must not bind the session to one
// model (mirrors recordSelectedInstanceID's "exactly one instance" rule).
func singleStockModel(userText string, items []any) string {
	matched := matchUserTextToInstanceTypeNames(userText, items, false)
	if len(matched) == 1 {
		return matched[0]
	}
	return ""
}

func handlePlatformImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareImages"
	fieldOrder := []string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"}
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderImageListReply(raw, "ImageSet", fieldOrder, req.Plan.Slots)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	setEnvelopeIfPopulated(&result, buildImageListEnvelope(raw, "ImageSet", fieldOrder, req.Plan.Slots, req.UserText, action, "platform"))
	return result
}

func handleImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	switch req.Plan.Slots.ImageSource {
	case ImageSourceShared:
		return handleSharedImageList(ctx, h, req)
	case ImageSourceCustom:
		return handleCustomImageList(ctx, h, req)
	case ImageSourceCommunity:
		return handleCommunityImageList(ctx, h, req)
	default:
		return handlePlatformImageList(ctx, h, req)
	}
}

func handleImageTagCatalog(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareImageTags"
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderImageTagCatalogReply(raw)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

func handleModelRepositoryBrowse(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const modelAction = "DescribeModelRepositoryModels"
	const tagAction = "DescribeModelRepositoryTags"
	tagRaw, fb := executeRouteAction(ctx, h, req.Plan.Intent, tagAction, map[string]any{})
	if fb != nil {
		return *fb
	}
	args := modelRepositoryArgsFromSlots(req.Plan.Slots, tagRaw)
	modelRaw, fb := executeRouteAction(ctx, h, req.Plan.Intent, modelAction, args)
	if fb != nil {
		return *fb
	}
	reply := renderModelRepositoryReply(modelRaw, tagRaw, req.Plan.Slots)
	result := HandledResult(reply)
	result.ToolAction = modelAction
	result.ToolArgs = copyArgs(args)
	return result
}

func handleCustomImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareCustomImages"
	fieldOrder := []string{"CompShareImageId", "Name", "ImageName", "Status"}
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, map[string]any{})
	if fb != nil {
		return *fb
	}
	reply := renderImageListReply(raw, "ImageSet", fieldOrder, req.Plan.Slots)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	setEnvelopeIfPopulated(&result, buildImageListEnvelope(raw, "ImageSet", fieldOrder, req.Plan.Slots, req.UserText, action, "custom"))
	return result
}

func handleCommunityImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCommunityImages"
	args := map[string]any{}
	if query := slotSearchQuery(req.Plan.Slots); query != "" {
		args["FuzzySearch"] = query
	}
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, args)
	if fb != nil {
		return *fb
	}
	reply := renderCommunityImageReply(raw, req.Plan.Slots)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	setEnvelopeIfPopulated(&result, buildCommunityImageEnvelope(raw, req.Plan.Slots, req.UserText))
	return result
}

func handleSharedImageList(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareSharingImages"
	args := map[string]any{"Limit": 20}
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, args)
	if fb != nil {
		return *fb
	}
	reply := renderSharedImageListReply(raw, req.Plan.Slots)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

// ---- Renderers --------------------------------------------------------------
//
// L0 deterministic NL filter (PR A round 2, 2026-05-18):
//
// Route replies do NOT pass through an LLM (engine.go's groundedRenderer
// short-circuits when Envelope == nil, which is the case for all route
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
	communityImageGroupLimit = 10 // upper bound on community renderer output lines
	communityVersionPerGroup = 3  // versions to show per CompshareImageGroup
)

// asciiStopwords applies to ASCII tokens (post-tokenization, post-lowercase).
var asciiStopwords = map[string]struct{}{
	"list": {}, "image": {}, "images": {}, "of": {}, "the": {}, "a": {},
	"an": {}, "what": {}, "any": {}, "is": {}, "are": {}, "have": {}, "has": {},
	"do": {}, "does": {}, "show": {}, "me": {}, "my": {}, "for": {}, "to": {},
	"all": {}, "available": {},
}

var tokenSplitRegex = regexp.MustCompile(`[A-Za-z0-9_.]+|\p{Han}+`)

// pureNumericTokenRegex matches ASCII tokens consisting only of digits (no dot,
// no letters). These are too generic to use for image-name substring matching
// (e.g. "Debian 12" -> "12" silently matches "py312", "vLLM v0.12.0"). Version
// strings with dots like "22.04" are NOT pure-numeric (the dot makes them
// version-shaped) and remain useful as filter keywords.
var pureNumericTokenRegex = regexp.MustCompile(`^\d+$`)

// extractUserTokens tokenizes text and drops ASCII stopwords + 1-char noise.
// It is retained only for API-vocabulary utility tests; route handlers use
// router-provided read-only slots instead of deriving filters from the full
// user sentence.
func extractUserTokens(userText string) []string {
	if strings.TrimSpace(userText) == "" {
		return nil
	}
	raw := tokenSplitRegex.FindAllString(userText, -1)
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
	if len(matched) == 0 {
		matched = matchUserGPUVariantAliases(userText, apiNames)
	}
	return matched
}

func matchUserGPUVariantAliases(userText string, apiNames []string) []string {
	if userText == "" || len(apiNames) == 0 {
		return nil
	}
	tokens := gpuLikeTokenRegex.FindAllString(userText, -1)
	if len(tokens) == 0 {
		return nil
	}
	seenTokens := map[string]struct{}{}
	matched := []string{}
	seenNames := map[string]struct{}{}
	for _, token := range tokens {
		token = strings.ToUpper(strings.TrimSpace(token))
		if token == "" {
			continue
		}
		if _, ok := seenTokens[token]; ok {
			continue
		}
		seenTokens[token] = struct{}{}
		for _, name := range apiNames {
			if name == "" {
				continue
			}
			if _, ok := seenNames[name]; ok {
				continue
			}
			upperName := strings.ToUpper(name)
			if !strings.HasPrefix(upperName, token) || len(upperName) == len(token) {
				continue
			}
			next := rune(upperName[len(token)])
			if !isGPUVariantSuffixRune(next) {
				continue
			}
			matched = append(matched, name)
			seenNames[name] = struct{}{}
		}
	}
	return matched
}

func isGPUVariantSuffixRune(r rune) bool {
	return r == '_' || (r >= 'A' && r <= 'Z')
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
		if memoryMatched := matchMemoryHintedInstanceTypeNames(hints, items, matched); len(memoryMatched) > 0 {
			return memoryMatched
		}
		if len(matched) > 0 {
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

var gpuLikeTokenRegex = regexp.MustCompile(`(?i)\b([a-z]{1,3}\d{2,4}[a-z0-9_]*|\d{4}(?:_\d+g)?)\b`)

func renderGPUSpecsReply(raw map[string]any, slots Slots, userText string) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noGPUSpecsReply
	}
	query := slotSearchQuery(slots)
	matched := matchUserTextToInstanceTypeNames(query, items, true)

	var prefix string
	filterTo := map[string]struct{}{}
	if len(matched) > 0 {
		for _, m := range matched {
			filterTo[m] = struct{}{}
		}
	} else if query != "" {
		prefix = "未在当前可售机型里找到您提到的型号。以下是当前可售机型规格：\n"
	}

	detailed := slots.DetailLevel == DetailLevelFull
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

func buildGPUSpecsEnvelope(raw map[string]any, slots Slots, userText string) envelope.Envelope {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	matched := matchUserTextToInstanceTypeNames(slotSearchQuery(slots), items, true)
	filterTo := map[string]struct{}{}
	for _, m := range matched {
		filterTo[m] = struct{}{}
	}
	detailed := slots.DetailLevel == DetailLevelFull
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

func buildStockEnvelope(raw map[string]any, userText string) envelope.Envelope {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	matched := matchUserTextToInstanceTypeNames(userText, items, false)
	filterTo := map[string]struct{}{}
	for _, m := range matched {
		filterTo[m] = struct{}{}
	}

	env := envelope.Envelope{
		Kind:          envelope.KindStockAvailability,
		SourceActions: []string{"DescribeAvailableCompShareInstanceTypes"},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints:   envelope.Constraints{DoNotInventInstances: true},
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "user_question", Label: "User question", Value: userText, Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "disclaimer", Label: "Disclaimer", Value: soldOutDisclaimer, Source: envelope.FactSourceComputed},
	)

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
			continue
		}
		seenNames[name] = struct{}{}
		status := safeString(entry, "Status")
		if status == "" {
			status = "Normal"
		}
		subjectID := "stock:" + name
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID: subjectID, Name: name, Type: envelope.SubjectGPUModel,
		})
		env.Facts = append(env.Facts,
			envelope.Fact{SubjectID: subjectID, Key: "model_name", Label: "机型", Value: name, Source: envelope.FactSourceAPI},
			envelope.Fact{SubjectID: subjectID, Key: "status", Label: "状态", Value: status, Source: envelope.FactSourceAPI},
		)
	}
	if len(matched) == 0 && strings.TrimSpace(userText) != "" {
		env.Computed = append(env.Computed,
			envelope.Fact{Key: "no_match_hint", Label: "未找到匹配", Value: "未在当前可售机型里找到您提到的型号", Source: envelope.FactSourceComputed},
		)
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "total_count", Label: "Total count", Value: len(env.Subjects), Source: envelope.FactSourceComputed},
	)
	return env
}

func setEnvelopeIfPopulated(result *HandlerResult, env envelope.Envelope) {
	if len(env.Subjects) == 0 {
		return
	}
	result.Envelope = &env
	result.RendererInputEnvelopeHashes = hashEnvelopeForRenderer(env)
}

func buildImageListEnvelope(raw map[string]any, listKey string, fieldOrder []string, slots Slots, userText string, action string, category string) envelope.Envelope {
	items := mapSliceAt(raw, listKey)
	query := slotFilterQuery(slots)
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
		if query != "" && len(matchFields) > 0 {
			if !entryMatchesSlotQuery(entry, query, matchFields) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}

	env := envelope.Envelope{
		Kind:          envelope.KindImageList,
		SourceActions: []string{action},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints:   envelope.Constraints{DoNotInventInstances: true},
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "image_category", Label: "Image category", Value: category, Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "total_count", Label: "Total count", Value: len(filtered), Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "user_question", Label: "User question", Value: userText, Source: envelope.FactSourceComputed},
	)
	shown := 0
	for i, entry := range filtered {
		if shown >= imageListDisplayCap {
			break
		}
		id := safeString(entry, "CompShareImageId")
		if id == "" {
			id = fmt.Sprintf("image_%d", i)
		}
		subjectID := "image:" + id
		name := bestImageName(entry)
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID: subjectID, Name: name, Type: envelope.SubjectImage,
		})
		// Skip the raw-id / redundant-name display facts so the grounded renderer
		// does not dump CompShareImageId per row — the id lives in Subject.ID and the
		// name in Subject.Name. Keeps the rendered list clean (类型/状态/作者 only).
		for _, key := range fieldOrder {
			if _, skip := imageDisplaySkipFields[key]; skip {
				continue
			}
			v := safeString(entry, key)
			if v == "" {
				continue
			}
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID, Key: key, Label: imageFieldLabel(key), Value: v, Source: envelope.FactSourceAPI,
			})
		}
		shown++
	}
	if len(filtered) > shown {
		env.Computed = append(env.Computed, envelope.Fact{
			Key: "display_truncated", Label: "Display truncated",
			Value:  fmt.Sprintf("showing %d of %d images; ask with a keyword to narrow", shown, len(filtered)),
			Source: envelope.FactSourceComputed,
		})
	}
	return env
}

func buildCommunityImageEnvelope(raw map[string]any, slots Slots, userText string) envelope.Envelope {
	groups := mapSliceAt(raw, "CompshareImageGroup")
	if len(groups) == 0 {
		return buildImageListEnvelope(raw, "ImageSet",
			[]string{"Name", "Author", "CompShareImageId"}, slots, userText,
			"DescribeCommunityImages", "community")
	}
	filtered := make([]map[string]any, 0, len(groups))
	for _, item := range groups {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		filtered = append(filtered, entry)
	}
	// Surface genuinely-popular images first: live API order is recommend-weighted,
	// so sort subjects by CreatedCount (部署次数) desc — the grounded renderer lists
	// subjects in envelope order.
	sort.SliceStable(filtered, func(i, j int) bool {
		return communityDeployCount(filtered[i]) > communityDeployCount(filtered[j])
	})

	env := envelope.Envelope{
		Kind:          envelope.KindImageList,
		SourceActions: []string{"DescribeCommunityImages"},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints:   envelope.Constraints{DoNotInventInstances: true},
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "image_category", Label: "Image category", Value: "community", Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "total_count", Label: "Total count", Value: len(filtered), Source: envelope.FactSourceComputed},
		envelope.Fact{Key: "user_question", Label: "User question", Value: userText, Source: envelope.FactSourceComputed},
	)
	lineBudget := communityImageGroupLimit
	for _, entry := range filtered {
		if lineBudget <= 0 {
			break
		}
		name := communityGroupName(entry)
		if name == "" {
			continue
		}
		subjectID := "image_group:" + name
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID: subjectID, Name: name, Type: envelope.SubjectImageGroup,
		})
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: "group_name", Label: "名称", Value: name, Source: envelope.FactSourceAPI,
		})
		if author := safeString(entry, "Author"); author != "" {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID, Key: "author", Label: "作者", Value: author, Source: envelope.FactSourceAPI,
			})
		}
		versions := mapSliceAt(entry, "Data")
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID: subjectID, Key: "version_count", Label: "版本数", Value: len(versions), Source: envelope.FactSourceAPI,
		})
		if dc := communityDeployCount(entry); dc > 0 {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID, Key: "deploy_count", Label: "部署次数", Value: int(dc), Source: envelope.FactSourceAPI,
			})
		}
		lineBudget--
		shown := 0
		for _, v := range versions {
			if lineBudget <= 0 || shown >= communityVersionPerGroup {
				break
			}
			ver, ok := v.(map[string]any)
			if !ok {
				continue
			}
			parts := []string{}
			for _, key := range []string{"CompShareImageId", "Name", "VersionName", "Version"} {
				if val := safeString(ver, key); val != "" {
					parts = append(parts, imageFieldLabel(key)+"="+val)
				}
			}
			if len(parts) == 0 {
				continue
			}
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID,
				Key:       fmt.Sprintf("version_%d", shown+1),
				Label:     fmt.Sprintf("版本%d", shown+1),
				Value:     strings.Join(parts, ", "),
				Source:    envelope.FactSourceAPI,
			})
			lineBudget--
			shown++
		}
		if len(versions) > shown {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID,
				Key:       "versions_truncated",
				Label:     "版本截断",
				Value:     fmt.Sprintf("共 %d 个版本，仅展示 %d 个", len(versions), shown),
				Source:    envelope.FactSourceComputed,
			})
		}
	}
	return env
}

func communityGroupName(entry map[string]any) string {
	if name := safeString(entry, "ImageName"); name != "" {
		return name
	}
	return safeString(entry, "Name")
}

func bestImageName(entry map[string]any) string {
	for _, key := range []string{"Name", "CompShareImageName", "ImageName"} {
		if v := safeString(entry, key); v != "" {
			return v
		}
	}
	return ""
}

func imageFieldLabel(key string) string {
	switch key {
	case "CompShareImageId":
		return "镜像ID"
	case "CompShareImageName":
		return "镜像名称"
	case "ImageName":
		return "镜像名"
	case "ImageType":
		return "镜像类型"
	case "Name":
		return "名称"
	case "Status":
		return "状态"
	case "Author":
		return "作者"
	default:
		return key
	}
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

	var prefix string
	filterTo := map[string]struct{}{}
	if len(matched) > 0 {
		for _, m := range matched {
			filterTo[m] = struct{}{}
		}
	} else if strings.TrimSpace(userText) != "" {
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
	// Stock returns ALL regions by default. Targeted-region precision
	// (narrow to the user's anchored instance) is deferred to the M3
	// ContextAssembler follow-up — see project-next-action #34. Filtering
	// to "zones the user already has instances in" was previously done
	// here but silently hid cross-region inventory in multi-region
	// accounts, so it was removed (PR-δ0).
	entries := matchedNormalStockEntries(stockRaw, slotSearchQuery(req.Plan.Slots))
	if len(entries) == 0 {
		return "", false, nil
	}
	if req.Plan.Intent != IntentStockAvailability {
		result := FallbackBeforeTool(FallbackActionNotAllowed)
		return "", false, &result
	}
	supportZones := fetchStockSupportZones(ctx, h)
	if filter := stockZoneFilterFromSlot(req.Plan.Slots.Zone, supportZones); len(filter) > 0 {
		entries = filterStockEntriesByZone(entries, filter)
		if len(entries) == 0 {
			return renderStockReply(stockRaw, slotSearchQuery(req.Plan.Slots)) + "\n未在你指定的可用区里找到该机型的开售信息。", true, nil
		}
	}
	entriesByModel, modelOrder := groupStockEntriesByModel(entries)
	inventoryRaw, _ := h.executor.Execute(ctx, "DescribeCompShareGpuInventory", map[string]any{})
	inventoryLine := renderRawGPUInventoryLine(modelOrder, entriesByModel, inventoryRaw, supportZones)
	imageRaw, fb := executeRouteAction(ctx, h, req.Plan.Intent, "DescribeCompShareImages", map[string]any{
		"ImageType": "System",
		"Limit":     20,
	})
	if fb != nil {
		return "", false, fb
	}
	imageID := selectCapacityPrecheckImageID(imageRaw)
	if imageID == "" {
		return renderStockInventoryCapacityReply(failedStockCapacityChecks(entriesByModel, modelOrder), inventoryLine) + "\n容量预检未执行：未获取到可用于预检的系统镜像。", true, nil
	}

	checks := make([]stockCapacityCheck, 0, len(entries))

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
			args := capacityPrecheckArgs(entry, imageID, supportZones, stockRaw, imageRaw)
			capacityRaw, err := executeStockCapacityPrecheck(ctx, h, args)
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
		return renderStockInventoryCapacityReply(failedStockCapacityChecks(entriesByModel, modelOrder), inventoryLine) + "\n容量预检未执行：当前接口结果缺少可用区信息。", true, nil
	}
	return renderStockInventoryCapacityReply(checks, inventoryLine), true, nil
}

func failedStockCapacityChecks(entriesByModel map[string][]stockInstanceTypeEntry, modelOrder []string) []stockCapacityCheck {
	var checks []stockCapacityCheck
	for _, model := range modelOrder {
		entries := entriesByModel[model]
		if len(entries) == 0 {
			continue
		}
		zone := entries[0].Zone
		checks = append(checks, stockCapacityCheck{Name: model, Zone: zone, Failed: true})
	}
	return checks
}

func groupStockEntriesByModel(entries []stockInstanceTypeEntry) (map[string][]stockInstanceTypeEntry, []string) {
	entriesByModel := map[string][]stockInstanceTypeEntry{}
	modelOrder := []string{}
	for _, entry := range entries {
		if _, ok := entriesByModel[entry.Name]; !ok {
			modelOrder = append(modelOrder, entry.Name)
		}
		entriesByModel[entry.Name] = append(entriesByModel[entry.Name], entry)
	}
	return entriesByModel, modelOrder
}

func fetchStockSupportZones(ctx context.Context, h *DemoHandler) []zones.ZoneInfo {
	list, err := zones.FetchSupportZones(ctx, h.executor, 0, 0)
	if err != nil {
		return nil
	}
	return list
}

func stockZoneFilterFromSlot(zoneText string, supportZones []zones.ZoneInfo) map[string]struct{} {
	if len(supportZones) == 0 {
		return nil
	}
	zoneText = strings.TrimSpace(zoneText)
	if zoneText == "" {
		return nil
	}
	if exact, ok := zones.ExactZone(supportZones, zoneText); ok {
		return map[string]struct{}{strings.ToLower(exact): {}}
	}
	for _, z := range supportZones {
		if z.Zone == "" {
			continue
		}
		if strings.EqualFold(zoneText, z.Zone) ||
			(z.Describe != "" && strings.EqualFold(zoneText, z.Describe)) ||
			(z.Region != "" && strings.EqualFold(zoneText, z.Region)) {
			return map[string]struct{}{strings.ToLower(z.Zone): {}}
		}
	}
	return nil
}

func filterStockEntriesByZone(entries []stockInstanceTypeEntry, filter map[string]struct{}) []stockInstanceTypeEntry {
	if len(filter) == 0 {
		return entries
	}
	out := make([]stockInstanceTypeEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := filter[strings.ToLower(entry.Zone)]; ok {
			out = append(out, entry)
		}
	}
	return out
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
		out = append(out, stockInstanceTypeEntry{
			Name:   name,
			Status: status,
			Zone:   zone,
		})
	}
	return out
}

func capacityPrecheckArgs(entry stockInstanceTypeEntry, imageID string, supportZones []zones.ZoneInfo, catalog, images map[string]any) map[string]any {
	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:             entry.Zone,
		GPUType:          entry.Name,
		CompShareImageID: imageID,
		ChargeType:       deployment.ChargeTypePostpay,
		Disks:            deployment.ResolveBootDisk(images, catalog, imageID, entry.Name, entry.Zone),
	})
	placement := deployment.ZonePlacement{
		Zone:   entry.Zone,
		Region: stockRegionFromZone(entry.Zone),
	}
	if zone := stockZoneInfoForEntry(entry, supportZones); zone.Zone != "" {
		placement.Zone = zone.Zone
		if zone.Region != "" {
			placement.Region = zone.Region
		}
		placement.ZoneID = zone.ZoneID
		placement.AzGroup = zone.RegionID
		placement.IsPod = zone.IsPod
	}
	return deployment.ApplyCapacityPlacementArgs(args, placement)
}

func executeStockCapacityPrecheck(ctx context.Context, h *DemoHandler, args map[string]any) (map[string]any, error) {
	if exec, ok := h.executor.(internalHandlerExecutor); ok {
		return exec.ExecuteInternal(ctx, "CheckCompShareResourceCapacity", args)
	}
	return h.executor.Execute(ctx, "CheckCompShareResourceCapacity", args)
}

func stockZoneInfoForEntry(entry stockInstanceTypeEntry, supportZones []zones.ZoneInfo) zones.ZoneInfo {
	for _, z := range supportZones {
		if z.Zone != "" && strings.EqualFold(z.Zone, entry.Zone) {
			return z
		}
	}
	return zones.ZoneInfo{}
}

func stockRegionFromZone(zone string) string {
	zone = strings.TrimSpace(zone)
	if zone == "" || strings.Count(zone, "-") < 2 {
		return ""
	}
	idx := strings.LastIndex(zone, "-")
	if idx <= 0 {
		return ""
	}
	return zone[:idx]
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
		// Every model reaching here came from matchedNormalStockEntries, so the
		// catalog already reports Status=Normal (机型开售). checkedSpecs==0 means no
		// zone yielded a usable capacity-precheck result — the precheck either
		// failed to run (CLI with empty project_id → RetCode 230; HTTP missing
		// ProjectId → RetCode 292) or ran but returned no Specs. Don't bury the
		// known catalog truth under "无法确认是否有可创建库存" (which wrongly implies we
		// can't even tell it is on sale). Fall back to the catalog-level 开售
		// statement and be explicit that exact creatability was not verified this
		// turn. (#3b graceful degradation — a precheck failure must not override
		// the catalog answer.)
		return fmt.Sprintf("%s 机型当前开售；本次容量预检未能确认具体配置的可创建性，精确库存请以控制台创建页为准。", models)
	}
	reply := fmt.Sprintf("%s 当前暂无可创建库存，暂时不能新建实例。", models)
	return appendCapacityFailureNote(reply, failedZones)
}

func renderStockInventoryCapacityReply(checks []stockCapacityCheck, inventoryLine string) string {
	reply := renderStockCapacityReply(checks)
	names := uniqueStockCheckNames(checks)
	if len(names) == 0 {
		return reply
	}
	sort.Strings(names)
	models := strings.Join(names, "、")
	if inventoryLine == "" {
		inventoryLine = fmt.Sprintf("原始 GPU 库存：接口未返回 %s 的库存数量。", models)
	}
	if len(checks) > 0 && anyStockCapacityEnough(checks) {
		return fmt.Sprintf("%s 默认创建配置已通过容量预检，可以新建实例。\n%s\n机型状态：开售。", models, inventoryLine)
	}
	if allStockCapacityFailed(checks) {
		return fmt.Sprintf("%s 默认创建配置容量预检未完成，暂不能确认默认配置是否可创建。\n%s\n机型状态：开售。", models, inventoryLine)
	}
	return fmt.Sprintf("%s 默认创建配置暂未通过容量预检，暂时不能新建实例。\n%s\n机型状态：开售。", models, inventoryLine)
}

func uniqueStockCheckNames(checks []stockCapacityCheck) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, check := range checks {
		if check.Name == "" {
			continue
		}
		if _, ok := seen[check.Name]; ok {
			continue
		}
		seen[check.Name] = struct{}{}
		out = append(out, check.Name)
	}
	return out
}

func anyStockCapacityEnough(checks []stockCapacityCheck) bool {
	for _, check := range checks {
		if len(check.EnoughSpecs) > 0 {
			return true
		}
	}
	return false
}

func allStockCapacityFailed(checks []stockCapacityCheck) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if !check.Failed {
			return false
		}
	}
	return true
}

func renderRawGPUInventoryLine(modelOrder []string, entriesByModel map[string][]stockInstanceTypeEntry, raw map[string]any, supportZones []zones.ZoneInfo) string {
	if len(modelOrder) == 0 {
		return ""
	}
	pool := stockInventoryPool(raw, "Exclusive")
	if len(pool) == 0 {
		return "原始 GPU 库存：库存接口未返回可用数据。"
	}
	lines := []string{}
	for _, model := range modelOrder {
		entries := entriesByModel[model]
		known := []string{}
		for _, entry := range entries {
			zoneID := stockInventoryZoneID(entry, supportZones)
			if zoneID == 0 {
				continue
			}
			gpuCounts, ok := pool[zoneID]
			if !ok {
				continue
			}
			count, ok := gpuCounts[entry.Name]
			if !ok {
				continue
			}
			zone := stockZoneDisplay(entry, supportZones)
			if count > 0 {
				known = append(known, fmt.Sprintf("%s 库存约 %s 张 GPU", zone, trimFloat(count)))
			} else {
				known = append(known, fmt.Sprintf("%s 暂无原始 GPU 库存", zone))
			}
		}
		if len(known) == 0 {
			labels := stockEntryZoneLabels(entries, supportZones)
			if len(labels) > 0 {
				lines = append(lines, fmt.Sprintf("%s 接口未返回 %s 的库存数量", strings.Join(labels, "、"), model))
			} else {
				lines = append(lines, fmt.Sprintf("接口未返回 %s 的库存数量", model))
			}
			continue
		}
		sort.Strings(known)
		lines = append(lines, strings.Join(known, "；"))
	}
	return "原始 GPU 库存：" + strings.Join(lines, "；") + "。"
}

func stockZoneDisplay(entry stockInstanceTypeEntry, supportZones []zones.ZoneInfo) string {
	if entry.Zone != "" {
		if describe := zones.DescribeFor(supportZones, entry.Zone); describe != "" {
			return describe
		}
		return entry.Zone
	}
	return "未知可用区"
}

func stockInventoryZoneID(entry stockInstanceTypeEntry, supportZones []zones.ZoneInfo) uint32 {
	if entry.Zone != "" {
		for _, z := range supportZones {
			if z.ZoneID != 0 && strings.EqualFold(z.Zone, entry.Zone) {
				return z.ZoneID
			}
		}
	}
	return 0
}

func stockEntryZoneLabels(entries []stockInstanceTypeEntry, supportZones []zones.ZoneInfo) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, entry := range entries {
		label := stockZoneDisplay(entry, supportZones)
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func stockInventoryPool(raw map[string]any, poolName string) map[uint32]map[string]float64 {
	if raw == nil {
		return nil
	}
	switch inv := raw["GpuInventory"].(type) {
	case map[string]any:
		return convertStockInventoryPool(inv[poolName])
	case map[string]map[uint32]map[string]uint32:
		return convertStockInventoryPool(inv[poolName])
	case map[string]map[uint32]map[string]float64:
		return convertStockInventoryPool(inv[poolName])
	default:
		return nil
	}
}

func convertStockInventoryPool(raw any) map[uint32]map[string]float64 {
	out := map[uint32]map[string]float64{}
	switch pool := raw.(type) {
	case map[string]any:
		for rawZoneID, rawGPUCounts := range pool {
			id, ok := parseUint32Loose(rawZoneID)
			if !ok {
				continue
			}
			if counts := convertGPUCountMap(rawGPUCounts); len(counts) > 0 {
				out[id] = counts
			}
		}
	case map[uint32]map[string]uint32:
		for id, counts := range pool {
			out[id] = map[string]float64{}
			for gpu, count := range counts {
				out[id][gpu] = float64(count)
			}
		}
	case map[uint32]map[string]float64:
		for id, counts := range pool {
			out[id] = map[string]float64{}
			for gpu, count := range counts {
				out[id][gpu] = count
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func convertGPUCountMap(raw any) map[string]float64 {
	out := map[string]float64{}
	switch counts := raw.(type) {
	case map[string]any:
		for gpu, rawCount := range counts {
			if count, ok := numericValue(rawCount); ok {
				out[gpu] = count
			}
		}
	case map[string]uint32:
		for gpu, count := range counts {
			out[gpu] = float64(count)
		}
	case map[string]float64:
		for gpu, count := range counts {
			out[gpu] = count
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseUint32Loose(v any) (uint32, bool) {
	switch typed := v.(type) {
	case string:
		typed = strings.TrimSpace(typed)
		if typed == "" {
			return 0, false
		}
		n, err := strconv.ParseUint(typed, 10, 32)
		if err != nil || n == 0 {
			return 0, false
		}
		return uint32(n), true
	default:
		n, ok := numericValue(typed)
		if !ok || n <= 0 || n != float64(uint32(n)) {
			return 0, false
		}
		return uint32(n), true
	}
}

func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
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

// imageModelBrowseDisplayCap bounds how many image/model candidates the fast-tier
// browse replies surface by default. Keep image and model browsing aligned so
// all catalog-style answers stay compact and comparable.
const imageModelBrowseDisplayCap = 10

// imageListDisplayCap bounds how many image rows the fast-tier reply / envelope
// surfaces by default, so a "列出全部镜像" turn does not dump the whole catalog
// (39+) as one wall of text. The keyword filter narrows when the user names
// something; this cap only bites on the list-all path. Overflow is reported as a
// "共 N 个" note inviting a keyword filter.
const imageListDisplayCap = imageModelBrowseDisplayCap

// imageDisplaySkipFields are the raw-id / redundant-name keys the clean image
// display intentionally OMITS: the name is shown once up front (bestImageName) and
// the raw CompShareImageId is dropped from the default view (用户按名称引用即可; the
// envelope's Subject.ID still carries the id for any downstream consumer).
var imageDisplaySkipFields = map[string]struct{}{
	"CompShareImageId":   {},
	"Name":               {},
	"CompShareImageName": {},
	"ImageName":          {},
}

// formatImageDisplayLine renders one image as a clean, ID-free display line —
// 名称 first, then the human-relevant fields (类型/作者/状态) with 中文 labels
// (imageFieldLabel) — matching the GPU/stock renderers instead of the raw English
// `Key=Value` dump that led with CompShareImageId. Returns "" when there is no name.
func formatImageDisplayLine(entry map[string]any, fieldOrder []string) string {
	name := bestImageName(entry)
	if name == "" {
		return ""
	}
	parts := []string{"名称=" + name}
	seen := map[string]struct{}{}
	for _, key := range fieldOrder {
		if _, skip := imageDisplaySkipFields[key]; skip {
			continue
		}
		v := safeString(entry, key)
		if v == "" {
			continue
		}
		label := imageFieldLabel(key)
		if _, dup := seen[label]; dup {
			continue
		}
		seen[label] = struct{}{}
		parts = append(parts, label+"="+v)
	}
	return strings.Join(parts, ", ")
}

func renderImageListReply(raw map[string]any, listKey string, fieldOrder []string, slots Slots) string {
	items := mapSliceAt(raw, listKey)
	if len(items) == 0 {
		return noImageListReply
	}
	query := slotFilterQuery(slots)
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
		if query != "" && len(matchFields) > 0 {
			if !entryMatchesSlotQuery(entry, query, matchFields) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	// "query + 0 matches" -> explicit not-found, do not silently fall
	// through to the full list (that's what confused users in round 1 smoke).
	if query != "" && len(filtered) == 0 {
		return noImageListNoMatchReply
	}
	lines := make([]string, 0, imageListDisplayCap)
	for _, entry := range filtered {
		if len(lines) >= imageListDisplayCap {
			break
		}
		line := formatImageDisplayLine(entry, fieldOrder)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return noImageListReply
	}
	out := strings.Join(lines, "\n")
	if len(filtered) > len(lines) {
		out += fmt.Sprintf("\n（共 %d 个镜像，已显示前 %d 个；可补充关键词进一步筛选）", len(filtered), len(lines))
	}
	return out
}

func renderImageTagCatalogReply(raw map[string]any) string {
	tagIndex := stringSliceAt(raw, "TagIndex")
	tagsMap := stringSliceMapAt(raw, "TagsMap")
	lines := []string{}
	for _, category := range tagIndex {
		tags := tagsMap[category]
		if len(tags) == 0 {
			continue
		}
		lines = append(lines, category+": "+strings.Join(limitStrings(tags, 12), "、"))
	}
	if len(lines) == 0 {
		tags := stringSliceAt(raw, "Tags")
		if len(tags) == 0 {
			return "未获取到镜像标签。"
		}
		return "镜像标签: " + strings.Join(limitStrings(tags, 30), "、")
	}
	return "镜像标签分类:\n" + strings.Join(lines, "\n")
}

func modelRepositoryArgsFromSlots(slots Slots, tagRaw map[string]any) map[string]any {
	args := map[string]any{}
	query := slotSearchQuery(slots)
	matchedTags := matchModelRepositoryTags(query, uniqueStrings(stringSliceAt(tagRaw, "Tags")))
	if len(matchedTags) > 0 {
		args["tags"] = strings.Join(limitStrings(matchedTags, 3), ",")
		return args
	}
	if query != "" && slots.ListMode != ListModeAll {
		args["name"] = strings.ToLower(query)
	}
	return args
}

func matchModelRepositoryTags(userText string, tags []string) []string {
	if strings.TrimSpace(userText) == "" || len(tags) == 0 {
		return nil
	}
	lowerText := strings.ToLower(userText)
	matched := []string{}
	seen := map[string]struct{}{}
	for _, tag := range tags {
		clean := strings.TrimSpace(tag)
		if clean == "" {
			continue
		}
		if !strings.Contains(lowerText, strings.ToLower(clean)) {
			continue
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		matched = append(matched, clean)
	}
	return matched
}

func containsASCIIAlpha(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

func renderModelRepositoryReply(modelRaw, tagRaw map[string]any, slots Slots) string {
	tags := uniqueStrings(stringSliceAt(tagRaw, "Tags"))
	models := mapSliceAt(modelRaw, "Models")
	filtered := filterModelRepositoryModels(models, slots)
	sections := []string{}
	if len(tags) > 0 {
		sections = append(sections, "模型仓库标签: "+strings.Join(limitStrings(tags, 20), "、"))
	}
	if len(filtered) == 0 {
		if len(tags) > 0 {
			sections = append(sections, "未找到匹配的模型。", modelRepositoryGuidanceFooter(false))
			return strings.Join(sections, "\n")
		}
		return "未获取到模型仓库数据。"
	}
	allLines := []string{}
	for _, entry := range filtered {
		line := buildModelRepositoryLine(entry)
		if line == "" {
			continue
		}
		allLines = append(allLines, line)
	}
	if len(allLines) == 0 {
		if len(tags) > 0 {
			sections = append(sections, "未找到匹配的模型。", modelRepositoryGuidanceFooter(false))
			return strings.Join(sections, "\n")
		}
		return "未获取到模型仓库数据。"
	}
	lines := allLines
	if len(lines) > imageModelBrowseDisplayCap {
		lines = lines[:imageModelBrowseDisplayCap]
	}
	sections = append(sections, "模型仓库列表:\n"+strings.Join(lines, "\n"))
	if len(allLines) > len(lines) {
		sections = append(sections, fmt.Sprintf("（共 %d 个模型，已显示前 %d 个；可补充关键词进一步筛选）", len(allLines), len(lines)))
	}
	sections = append(sections, modelRepositoryGuidanceFooter(true))
	return strings.Join(sections, "\n")
}

// modelRepositoryGuidanceFooter bridges a model-repository browse reply to the
// two real follow-up actions the user can take. Repo models are pre-downloaded
// onto the instance under the per-entry Path (verified live 2026-06-11: paths
// sit under /model/HuggingFace, /model/ModelScope, /model/ollama, /model/llm by
// source), so a found model is usable after deploy without re-downloading; a
// model the repo does not carry is self-pulled inside the instance. The footer
// points at the per-line Path field rather than hardcoding a single mount so it
// stays correct if the layout changes.
func modelRepositoryGuidanceFooter(found bool) string {
	if !found {
		return "仓库里暂时没有匹配的模型。你可以部署实例后自行拉取：HuggingFace / ModelScope 下载，或 Ollama 容器用 `ollama pull <模型名>`——需要具体命令可以问我。"
	}
	return strings.Join([]string{
		"说明：以上模型已预置在实例对应的 Path 路径下（见每条的 Path，按来源分布在 /model/HuggingFace、/model/ModelScope、/model/ollama 等），部署实例后可直接加载，无需重新下载。",
		"· 想部署某个模型，直接告诉我模型名（如「部署 Llama-3.1-8B」），我来帮你选 GPU 配置。",
		"· 仓库里没有的模型，可在实例内自行拉取（HuggingFace / ModelScope 下载，或 Ollama `ollama pull <模型名>`）——需要命令可以问我。",
	}, "\n")
}

func filterModelRepositoryModels(models []any, slots Slots) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, item := range models {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(safeString(entry, "Deleted")) == "1" {
			continue
		}
		out = append(out, entry)
	}
	if len(out) == 0 || slots.ListMode == ListModeAll {
		return out
	}
	query := slotSearchQuery(slots)
	if query == "" {
		return out
	}
	filtered := make([]map[string]any, 0, len(out))
	for _, entry := range out {
		if entryMatchesSlotQuery(entry, query, []string{"Name", "Path", "Tag"}) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func buildModelRepositoryLine(entry map[string]any) string {
	parts := []string{}
	for _, key := range []string{"Name", "Size", "Tag", "Path"} {
		if v := strings.TrimSpace(safeString(entry, key)); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

func renderCommunityImageReply(raw map[string]any, slots Slots) string {
	groups := mapSliceAt(raw, "CompshareImageGroup")
	if len(groups) == 0 {
		// Fallback: some responses use a flat ImageSet shape.
		return renderImageListReply(raw, "ImageSet",
			[]string{"Name", "Author", "CompShareImageId"}, slots)
	}
	filtered := make([]map[string]any, 0, len(groups))
	for _, item := range groups {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		filtered = append(filtered, entry)
	}

	filtered = dedupeCommunityImageGroups(filtered)

	// Surface genuinely-popular images first: the live API default order is
	// recommend-weighted, so sort by CreatedCount (部署次数) desc to make the
	// popularity figures monotonic and put the most-deployed images on top.
	sort.SliceStable(filtered, func(i, j int) bool {
		return communityDeployCount(filtered[i]) > communityDeployCount(filtered[j])
	})

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
	return "社区镜像：\n" + strings.Join(lines, "\n")
}

func dedupeCommunityImageGroups(groups []map[string]any) []map[string]any {
	if len(groups) <= 1 {
		return groups
	}
	bestByName := map[string]map[string]any{}
	order := []string{}
	for _, group := range groups {
		name := strings.TrimSpace(bestImageName(group))
		if name == "" {
			name = strings.TrimSpace(safeString(group, "ImageName"))
		}
		key := strings.ToLower(name)
		if key == "" {
			key = fmt.Sprintf("__unnamed_%d", len(order))
		}
		if _, ok := bestByName[key]; !ok {
			order = append(order, key)
			bestByName[key] = group
			continue
		}
		if communityDeployCount(group) > communityDeployCount(bestByName[key]) {
			bestByName[key] = group
		}
	}
	out := make([]map[string]any, 0, len(bestByName))
	for _, key := range order {
		if group := bestByName[key]; group != nil {
			out = append(out, group)
		}
	}
	return out
}

func renderSharedImageListReply(raw map[string]any, slots Slots) string {
	items := mapSliceAt(raw, "ImageSet")
	if len(items) == 0 {
		return "未获取到共享给你的镜像。"
	}
	query := slotFilterQuery(slots)
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if query != "" && !entryMatchesSlotQuery(entry, query, []string{"Name", "Description", "CompShareImageId", "ImageType", "Status"}) {
			continue
		}
		filtered = append(filtered, entry)
	}
	if query != "" && len(filtered) == 0 {
		return "未找到匹配的共享镜像。"
	}
	lines := []string{}
	for _, entry := range filtered {
		line := buildSharedImageLine(entry)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) >= 20 {
			break
		}
	}
	if len(lines) == 0 {
		return "未获取到共享给你的镜像。"
	}
	prefix := "共享给你的镜像"
	if total := strings.TrimSpace(safeString(raw, "TotalCount")); total != "" && total != "0" {
		prefix += "（共 " + total + " 个）"
	}
	return prefix + ":\n" + strings.Join(lines, "\n")
}

func buildSharedImageLine(entry map[string]any) string {
	name := bestImageName(entry)
	if name == "" {
		return ""
	}
	// 名称 first, raw CompShareImageId dropped (用户按名称引用即可) — same clean style
	// as the platform image list.
	parts := []string{"名称=" + name}
	for _, key := range []string{"ImageType", "Status"} {
		if v := strings.TrimSpace(safeString(entry, key)); v != "" {
			parts = append(parts, imageFieldLabel(key)+"="+v)
		}
	}
	if v := strings.TrimSpace(safeString(entry, "Container")); v != "" {
		parts = append(parts, "容器="+v)
	}
	if owner := sharedImageOwnerDisplay(entry); owner != "" {
		parts = append(parts, "所有者="+owner)
	}
	return strings.Join(parts, ", ")
}

func sharedImageOwnerDisplay(entry map[string]any) string {
	owner, ok := entry["Owner"].(map[string]any)
	if !ok || owner == nil {
		return ""
	}
	if name := strings.TrimSpace(safeString(owner, "AccountName")); name != "" {
		return name
	}
	if id := strings.TrimSpace(safeString(owner, "AccountId")); id != "" && id != "0" {
		return id
	}
	return ""
}

func buildCommunityGroupHeader(entry map[string]any) string {
	parts := []string{}
	// Live DescribeCommunityImages carries the group name in ImageName (group-level
	// Name is empty); communityGroupName reads ImageName||Name.
	if name := communityGroupName(entry); name != "" {
		parts = append(parts, "名称="+name)
	}
	if v := safeString(entry, "Author"); v != "" {
		parts = append(parts, "作者="+v)
	}
	if n := communityDeployCount(entry); n > 0 {
		parts = append(parts, fmt.Sprintf("部署次数=%d", n))
	}
	versions := mapSliceAt(entry, "Data")
	if len(versions) > 0 {
		parts = append(parts, fmt.Sprintf("版本数=%d", len(versions)))
	}
	return strings.Join(parts, ", ")
}

// communityDeployCount reads CreatedCount (the catalog's 部署次数 popularity
// signal) from a community-image group, falling back to its first version.
func communityDeployCount(entry map[string]any) int64 {
	if n := numericFieldInt(entry, "CreatedCount"); n > 0 {
		return n
	}
	if versions := mapSliceAt(entry, "Data"); len(versions) > 0 {
		if v0, ok := versions[0].(map[string]any); ok {
			return numericFieldInt(v0, "CreatedCount")
		}
	}
	return 0
}

// numericFieldInt reads a JSON-decoded numeric field as int64 (live responses
// decode numbers to float64).
func numericFieldInt(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func buildCommunityVersionLine(ver map[string]any) string {
	// 版本名 first, raw CompShareImageId dropped from display (用户按名称引用即可).
	parts := []string{}
	if name := bestImageName(ver); name != "" {
		parts = append(parts, "版本="+name)
	}
	for _, key := range []string{"VersionName", "Version"} {
		if v := safeString(ver, key); v != "" {
			parts = append(parts, "版本号="+v)
			break
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

func stringSliceAt(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	return stringsFromAny(m[key])
}

func stringSliceMapAt(m map[string]any, key string) map[string][]string {
	out := map[string][]string{}
	if m == nil {
		return out
	}
	switch typed := m[key].(type) {
	case map[string][]string:
		for k, v := range typed {
			out[safeValue(k)] = limitStrings(v, len(v))
		}
	case map[string]any:
		for k, v := range typed {
			out[safeValue(k)] = stringsFromAny(v)
		}
	}
	return out
}

func stringsFromAny(v any) []string {
	switch typed := v.(type) {
	case []string:
		return limitStrings(typed, len(typed))
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			s := strings.TrimSpace(safeValue(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func limitStrings(values []string, max int) []string {
	if max <= 0 || len(values) == 0 {
		return nil
	}
	limit := max
	if len(values) < limit {
		limit = len(values)
	}
	out := make([]string, 0, limit)
	for _, value := range values {
		value = strings.TrimSpace(safeValue(value))
		if value == "" {
			continue
		}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		clean := strings.TrimSpace(safeValue(value))
		if clean == "" {
			continue
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clean)
	}
	return out
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
