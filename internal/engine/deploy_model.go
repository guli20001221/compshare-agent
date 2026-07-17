package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
)

// deploy_model.go now holds the zone-resolution and create-time stock helpers that
// remain after the runtime convergence removed the dedicated "deploy_model" saga.
// Its former role — a B8.3 agent-tier dispatch handler (tryDeployModel) that drove a
// separate orchestrator saga (RunAgentSaga) — is gone: there is a single Workflow
// execution entry (workflow.Engine.Run), and the instance-create path resolves the
// zone/GPU here before the workflow runs. The zone-resolution helpers below are
// shared by the create flow (engine.applyCreateZoneResolution); together with a few
// small JSON/text utilities they are the live remainder of this file.

// deployZoneAliases is a small deterministic floor for common legacy mentions.
// Full zone ids and display names, including newly added zones, are resolved from
// the live support-zone catalog in resolveRequestedZone.
var deployZoneAliases = []struct {
	keys []string
	zone string
}{
	{[]string{"cn-sh2-02", "cn-sh2", "上海", "sh2"}, "cn-sh2-02"},
	{[]string{"cn-wlcb-01", "cn-wlcb", "乌兰察布", "wlcb"}, "cn-wlcb-01"},
}

// extractDeployZone returns the create-zone the user explicitly named in the
// request, or "" if none. Deterministic (Rule 5: code answers a structured signal
// — zone ids/aliases are exact tokens, no LLM needed). A non-empty result is
// honored strictly downstream (error rather than silent move if unsatisfiable).
func extractDeployZone(userMsg string) string {
	lower := strings.ToLower(userMsg)
	for _, a := range deployZoneAliases {
		for _, k := range a.keys {
			if strings.Contains(lower, strings.ToLower(k)) {
				return a.zone
			}
		}
	}
	return ""
}

// resolveRequestedZone resolves the availability zone the user named, matching the
// live support-zone catalog so a Chinese display name ("华北一C") maps to its
// zone id (cn-bj2-03) — the upstream catalog carries that mapping but the agent
// previously had no way to read it, so "华北一C" was silently dropped to the
// platform default. Returns:
//   - zone != "" : a confident match → honored strictly downstream.
//   - clarify != "": the mention was partial/ambiguous/unsupported ("华北一区" →
//     "是华北一C吗？") — the caller stops and asks instead of guessing a default.
//   - both "" : no zone referenced → existing default-zone behavior.
//
// Shared with the internal CreateInstanceWorkflow path
// (engine.applyCreateZoneResolution) so a user-named zone resolves identically
// regardless of which create entry point the turn took.
//
// It is strictly additive over the deterministic alias floor (extractDeployZone):
// when the live catalog is unavailable (CLI/no tenant identity) or the model
// declines, it degrades to that floor, never worse than before.
func (e *Engine) resolveRequestedZone(ctx context.Context, userMsg string) (zone, clarify string) {
	aliasZone := extractDeployZone(userMsg)
	list, err := e.supportZoneList(ctx)
	if err != nil || len(list) == 0 {
		return aliasZone, "" // degrade to the deterministic alias floor
	}
	// Unambiguous literal (zone id or full display name) → no LLM needed.
	if z, ok := zones.ExactZone(list, userMsg); ok {
		return z, ""
	}
	// No zone-ish mention at all → keep the alias floor (e.g. a bare city alias).
	if !zones.Mentions(userMsg) {
		return aliasZone, ""
	}
	// A zone mention with no exact literal → LLM judgment (partial/ambiguous).
	switch d := e.matchZoneLLM(ctx, userMsg, list); d.Kind {
	case "exact":
		return d.Zone, ""
	case "clarify":
		return "", d.Clarify
	default:
		return aliasZone, ""
	}
}

func (e *Engine) supportZoneList(ctx context.Context) ([]zones.ZoneInfo, error) {
	if e.externalExecutor == nil {
		return nil, nil
	}
	cat := e.zoneCatalog
	if cat == nil {
		cat = zones.Default()
	}
	u, _ := tools.UserFrom(ctx)
	return cat.Get(ctx, e.externalExecutor, u.TopOrganizationID, u.OrganizationID)
}

// supportZoneListStrict is supportZoneList without the serve-stale fallback: an
// expired catalog that fails to refresh returns an error instead of stale zones.
// The write-path snapshot builder uses it so a create never lands on a zone list
// that may have changed upstream.
func (e *Engine) supportZoneListStrict(ctx context.Context) ([]zones.ZoneInfo, error) {
	if e.externalExecutor == nil {
		return nil, nil
	}
	cat := e.zoneCatalog
	if cat == nil {
		cat = zones.Default()
	}
	u, _ := tools.UserFrom(ctx)
	return cat.GetStrict(ctx, e.externalExecutor, u.TopOrganizationID, u.OrganizationID)
}

// matchZoneLLM asks the TierAgent model to match a fuzzy zone mention against the
// live zone list, returning a structured decision (exact / clarify / none).
// Mirrors extractDeploySearch: small focused prompt, JSON out, hallucinated
// zones rejected by zones.ParseDecision against the live list.
func (e *Engine) matchZoneLLM(ctx context.Context, userMsg string, list []zones.ZoneInfo) zones.Decision {
	client := e.agentLLMClient
	if client == nil {
		client = e.llmClient
	}
	if client == nil {
		return zones.Decision{Kind: "none"}
	}
	resp, err := client.Chat(ctx, llm.ChatRequest{Messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: zones.MatchSystemPrompt(list)},
		{Role: openai.ChatMessageRoleUser, Content: "用户消息：" + strings.TrimSpace(userMsg)},
	}})
	if err != nil || resp == nil {
		return zones.Decision{Kind: "none"}
	}
	e.emitTokenUsage(resp.Usage)
	return zones.ParseDecision(extractJSONObject(resp.Content), list,
		func(s string, v any) error { return json.Unmarshal([]byte(s), v) })
}

// zoneDescribeMap returns a zone-id → display-name map ("cn-bj2-03"→"华北一C")
// from the live catalog, used to label the create confirm form's zone options
// with the names the console shows. Empty when the catalog is unavailable — the
// form then falls back to bare zone ids (current behavior).
func (e *Engine) zoneDescribeMap(ctx context.Context) map[string]string {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(list))
	for _, z := range list {
		if z.Describe != "" {
			m[z.Zone] = z.Describe
		}
	}
	return m
}

func (e *Engine) zoneIDMap(ctx context.Context) map[string]uint32 {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]uint32, len(list))
	for _, z := range list {
		if z.Zone != "" && z.ZoneID != 0 {
			m[z.Zone] = z.ZoneID
		}
	}
	return m
}

func (e *Engine) zoneRegionIDMap(ctx context.Context) map[string]uint32 {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]uint32, len(list))
	for _, z := range list {
		if z.Zone != "" && z.RegionID != 0 {
			m[z.Zone] = z.RegionID
		}
	}
	return m
}

func (e *Engine) zoneIDFor(ctx context.Context, zone string) uint32 {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return 0
	}
	for z, id := range e.zoneIDMap(ctx) {
		if strings.EqualFold(z, zone) {
			return id
		}
	}
	return 0
}

func (e *Engine) zoneRegionIDFor(ctx context.Context, zone string) uint32 {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return 0
	}
	for z, id := range e.zoneRegionIDMap(ctx) {
		if strings.EqualFold(z, zone) {
			return id
		}
	}
	return 0
}

func (e *Engine) deploymentZonePlacement(ctx context.Context, zone string) deployment.ZonePlacement {
	placement := deployment.ZonePlacement{
		Zone:    strings.TrimSpace(zone),
		Region:  workflow.RegionFromZone(zone),
		ZoneID:  e.zoneIDFor(ctx, zone),
		AzGroup: e.zoneRegionIDFor(ctx, zone),
	}
	if isPod, ok := e.zoneIsPod(ctx, zone); ok {
		placement.IsPod = isPod
	}
	return placement
}

// zoneCatalogSnapshot builds the turn's read-only zone catalog from the live
// support-zone list, one structured placement per zone plus its console display
// name — the SINGLE reference the resolver validates against and the workflow
// looks placements up in, replacing the four parallel zone-keyed maps.
//
// The network call, its failure mode and caching live HERE (the same
// process-cached supportZoneList the old chain used, so this adds a cache read,
// not an API call), so the snapshot the workflow consumes is pure data. An error
// or an empty list yields an UNAVAILABLE snapshot — never a fallback: a consumer
// must refuse rather than guess a zone from a stale table, exactly as for the
// machine-type catalog. Each record carries its own Region (falling back to the
// structural derivation only when upstream omits it), so ZoneID/Region/AzGroup/
// IsPod and the display name all come from one row and cannot disagree.
func (e *Engine) zoneCatalogSnapshot(ctx context.Context) *deployment.ZoneCatalogSnapshot {
	// "could not obtain the catalog" is distinct from "obtained an empty catalog":
	// only the former is unavailable. No executor (CLI, no tenant) or a query error
	// is a failure — refuse; a successful query is available even when it lists no
	// zones.
	if e.externalExecutor == nil {
		return deployment.NewZoneCatalogSnapshot(false, nil)
	}
	list, err := e.supportZoneListStrict(ctx)
	if err != nil {
		return deployment.NewZoneCatalogSnapshot(false, nil)
	}
	entries := make([]deployment.ZoneCatalogEntry, 0, len(list))
	for _, z := range list {
		// Every field comes straight from the catalog row — including Region. A
		// missing Region is left empty (a consumer that needs it fails explicitly),
		// never guessed from the zone string: guessing is exactly the interpretation
		// this convergence removes.
		entries = append(entries, deployment.ZoneCatalogEntry{
			Placement: deployment.ZonePlacement{
				Zone:    z.Zone,
				Region:  z.Region,
				ZoneID:  z.ZoneID,
				AzGroup: z.RegionID,
				IsPod:   z.IsPod,
			},
			DisplayName: z.Describe,
		})
	}
	return deployment.NewZoneCatalogSnapshot(true, entries)
}

// zoneCatalogSnapshotForAction returns the zone catalog for the workflows that
// resolve a zone, or nil for the rest — so a non-zone workflow attaches no
// reference data and pays no catalog read.
func (e *Engine) zoneCatalogSnapshotForAction(ctx context.Context, action string) *deployment.ZoneCatalogSnapshot {
	switch action {
	case "CreateInstanceWorkflow", "CreateCFSWorkflow", "EnableNetOptimizerWorkflow":
		return e.zoneCatalogSnapshot(ctx)
	default:
		return nil
	}
}

func (e *Engine) zoneIsPodMap(ctx context.Context) map[string]bool {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]bool, len(list))
	for _, z := range list {
		if z.Zone != "" {
			m[z.Zone] = z.IsPod
		}
	}
	return m
}

func (e *Engine) zoneIsPod(ctx context.Context, zone string) (bool, bool) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return false, false
	}
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return false, false
	}
	for _, z := range list {
		if strings.EqualFold(z.Zone, zone) {
			return z.IsPod, true
		}
	}
	return false, false
}

// applyCreateZoneResolution resolves the structured Zone already accepted by
// Action Resolver, mutating args in place. It overrides args["Zone"]
// with the resolved zone id (e.g. "华北一C" → cn-bj2-03) and injects
// args["ZoneDescribes"] (zone-id → 显示名) so the confirm form labels each zone
// with the console's Chinese name. It returns a non-empty clarify question when
// the zone mention is partial/ambiguous ("华北一区" → "是华北一C吗？") so the
// caller stops before creating; otherwise "". Reuses the deploy saga's
// resolveRequestedZone so both create entry points behave identically. Degrades
// to no-op (LLM Zone untouched, no ZoneDescribes) when the live catalog is
// unavailable — e.g. on the CLI path with no tenant identity.
func (e *Engine) applyCreateZoneResolution(ctx context.Context, args map[string]any) (clarify string) {
	requestedZone, _ := args["Zone"].(string)
	userZone, clarify := e.resolveRequestedZone(ctx, requestedZone)
	if clarify != "" {
		return clarify
	}
	if userZone != "" {
		args["Zone"] = userZone
		args["GuidedZoneLocked"] = true
	}
	targetZone, _ := args["Zone"].(string)
	if isPod, ok := e.zoneIsPod(ctx, targetZone); ok {
		args["ZoneIsPod"] = isPod
	}
	if descMap := e.zoneDescribeMap(ctx); len(descMap) > 0 {
		args["ZoneDescribes"] = descMap
	}
	if idMap := e.zoneIDMap(ctx); len(idMap) > 0 {
		args["ZoneIds"] = idMap
	}
	if regionIDMap := e.zoneRegionIDMap(ctx); len(regionIDMap) > 0 {
		args["ZoneRegionIds"] = regionIDMap
	}
	if podMap := e.zoneIsPodMap(ctx); len(podMap) > 0 {
		args["ZoneIsPods"] = podMap
	}
	return ""
}

type zoneStock int

const (
	zoneUnknown zoneStock = iota // could not determine (no image id / API error / no matching spec)
	zoneInStock                  // single-card config confirmed available
	zoneSoldOut                  // single-card config present but ResourceEnough=false
)

// zoneStockState checks whether gpuType's single-card config has real stock in a
// zone, the same gate the saga's stepCheckCapacity uses (Specs[].{Gpu==1,
// ResourceEnough}). It needs the resolved CompShareImageId (capacity is image-
// scoped); without one it returns zoneUnknown so the caller falls back to the
// preferred zone rather than skipping it. Read-only (works in read-only mode too).
func (e *Engine) zoneStockState(ctx context.Context, zone, gpuType, imageID string, zoneCat *deployment.ZoneCatalogSnapshot) zoneStock {
	if imageID == "" || gpuType == "" {
		return zoneUnknown
	}
	// The placement comes from the SAME turn snapshot the create used — image
	// recovery must not re-resolve the zone through a second path. A zone the
	// snapshot does not carry yields zoneUnknown, so the caller defers to the
	// preferred zone rather than guessing.
	placement, ok := zoneCat.Placement(zone)
	if !ok {
		return zoneUnknown
	}
	capArgs := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:             zone,
		GPUType:          gpuType,
		CompShareImageID: imageID,
	})
	deployment.ApplyCapacityPlacementArgs(capArgs, placement)
	res := e.querySafeRead(ctx, "CheckCompShareResourceCapacity", capArgs)
	if res == nil {
		return zoneUnknown
	}
	specs, _ := res["Specs"].([]any)
	sawSingleCard := false
	for _, s := range specs {
		m, _ := s.(map[string]any)
		if m == nil {
			continue
		}
		if g, _ := m["Gpu"].(float64); g != 1 {
			continue
		}
		sawSingleCard = true
		if enough, _ := m["ResourceEnough"].(bool); enough {
			return zoneInStock
		}
	}
	if sawSingleCard {
		return zoneSoldOut
	}
	return zoneUnknown
}

// querySafeRead runs a read-only tool through the safe executor
// (OriginWorkflowInternal = no per-call confirm / registry churn) and returns the
// raw result map, or nil on error (matching degrades gracefully — the matcher still
// has the other source + the user message + the static-table GPU fallback).
func (e *Engine) querySafeRead(ctx context.Context, action string, args map[string]any) map[string]any {
	res, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: tools.OriginWorkflowInternal,
	})
	if err != nil || res == nil {
		return nil
	}
	return res.RawResult
}

// ── small pure helpers ──

// extractJSONObject returns the first {...} block in s, stripping markdown code
// fences and surrounding prose the model may add around the JSON decision.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// ── post-create usage guidance (B8.5: tell the user HOW to use the instance) ──

// imageUsage is the chosen image's usage guidance, fetched read-only AFTER a
// successful create. ports = app→port (the access endpoints); firewall = extra
// open TCP ports; autoStart = services come up on their own; readme = the
// community author's rich-text guide (platform Readme is always empty — verified
// 2026-05-31, so only community populates it).
type imageUsage struct {
	ports     []softwarePort
	firewall  []int
	autoStart bool
	readme    string
}

// softwarePort is one app↔port mapping from an image's SoftwarePorts.
type softwarePort struct {
	name string
	port int
}

var (
	mdImageRe      = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`) // markdown image: ![alt](url)
	htmlTagRe      = regexp.MustCompile(`(?s)<[^>]+>`)          // any HTML tag incl. <iframe ...>
	multiNewlineRe = regexp.MustCompile(`\n{3,}`)
	multiSpaceRe   = regexp.MustCompile(` {2,}`)
)
