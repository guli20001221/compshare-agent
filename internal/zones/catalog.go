// Package zones resolves CompShare availability zones from the live upstream
// catalog, including the human "可用区显示名称" (Describe, e.g. "华北一C") that
// only DescribeCompShareSupportZone exposes. It backs two consumers — the
// deploy_model saga and the create-instance workflow — so a user who names a
// zone in Chinese ("华北一C") is matched to its zone id (cn-bj2-03) instead of
// being silently dropped to the platform default.
//
// Two layers, deliberately split so the fuzzy judgment is testable without a
// live LLM and the data fetch is testable without a live API:
//   - Catalog: executor-backed, process-cached (TTL) zone list + exact lookups.
//   - Match prompt/parse: pure string helpers the caller feeds to its own LLM
//     client. Fuzzy/partial mentions ("华北一区" → "是华北一C吗？") are the LLM's
//     job; the zone list it chooses from is always the live authoritative one.
package zones

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ZoneInfo is one supported availability zone as reported by
// DescribeCompShareSupportZone.
type ZoneInfo struct {
	Zone     string // zone id, e.g. "cn-bj2-03"
	Region   string // region id, e.g. "cn-bj2"
	RegionID uint32 // numeric region/az_group id used by selected upstream APIs.
	ZoneID   uint32 // numeric zone id, e.g. 5001
	Describe string // 可用区显示名称, e.g. "华北一C"
	IsPod    bool   // true when the zone creates CPod/container instances.
}

// Executor is the subset of the CompShare API executor this package needs.
// Satisfied by *tools.ExternalExecutor.
type Executor interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

// FetchSupportZones calls DescribeCompShareSupportZone and parses the ZoneInfo
// list. topOrg/org are the gateway tenant identity — the action reads
// organization_id from the request body and 230s ("Params [organization_id]
// not available") without it. Returns an empty slice (not an error) when the
// response carries no ZoneInfo so callers degrade to default-zone behavior.
func FetchSupportZones(ctx context.Context, exec Executor, topOrg, org uint32) ([]ZoneInfo, error) {
	args := map[string]any{}
	if topOrg != 0 {
		args["top_organization_id"] = topOrg
	}
	if org != 0 {
		args["organization_id"] = org
	}
	res, err := exec.Execute(ctx, "DescribeCompShareSupportZone", args)
	if err != nil {
		return nil, err
	}
	raw, _ := res["ZoneInfo"].([]any)
	out := make([]ZoneInfo, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		zi := ZoneInfo{
			Zone:     str(m["Zone"]),
			Region:   str(m["Region"]),
			RegionID: u32(m["RegionId"]),
			Describe: str(m["Describe"]),
			ZoneID:   u32(m["ZoneId"]),
			IsPod:    boolVal(m["IsPod"]),
		}
		if zi.Zone == "" {
			continue
		}
		out = append(out, zi)
	}
	return out, nil
}

// Catalog is a process-wide TTL cache of the support-zone list. The Describe↔
// zone mapping is platform-global (identical for every tenant), so a single
// cache shared across requests is correct; any tenant's identity authenticates
// the refresh. Safe for concurrent use.
type Catalog struct {
	ttl   time.Duration
	now   func() time.Time
	fetch func(ctx context.Context, exec Executor, topOrg, org uint32) ([]ZoneInfo, error)

	mu        sync.Mutex
	zones     []ZoneInfo
	fetchedAt time.Time
}

// NewCatalog returns a Catalog with the given TTL. A zero ttl disables caching
// (every Get refetches) — useful in tests.
func NewCatalog(ttl time.Duration) *Catalog {
	return &Catalog{ttl: ttl, now: time.Now, fetch: FetchSupportZones}
}

// defaultCatalog is the process-wide cache. The support-zone list is identical
// for every tenant, so a single shared instance is correct and lets both the
// deploy_model saga and the create-instance workflow resolve zones without
// threading a *Catalog through two dependency trees.
var defaultCatalog = NewCatalog(10 * time.Minute)

// Default returns the process-wide zone catalog.
func Default() *Catalog { return defaultCatalog }

// Get returns the cached zone list, refreshing via the executor when the cache
// is empty or stale. On refresh failure it returns the last good cache (stale)
// when available, so a transient API blip doesn't disable zone resolution. This
// lenient read is for READ-ONLY display, where a slightly stale zone list is
// acceptable — a write path must use GetStrict.
func (c *Catalog) Get(ctx context.Context, exec Executor, topOrg, org uint32) ([]ZoneInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.zones) > 0 && c.ttl > 0 && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.zones, nil
	}
	z, err := c.fetch(ctx, exec, topOrg, org)
	if err != nil {
		if len(c.zones) > 0 {
			return c.zones, nil // serve stale on transient failure
		}
		return nil, err
	}
	if len(z) > 0 {
		c.zones = z
		c.fetchedAt = c.now()
	}
	return z, nil
}

// GetStrict is Get without the serve-stale fallback: a cache still inside its TTL
// is reused, but an EXPIRED cache whose refresh fails returns the error rather
// than stale data. Write paths (create / CFS / net-optimizer) use it so a
// mutating action never runs on a zone list that may have changed upstream since
// it was last fetched — the S1 rule that a write must refuse rather than fall
// back to old zone data. Read-only display keeps the lenient Get.
func (c *Catalog) GetStrict(ctx context.Context, exec Executor, topOrg, org uint32) ([]ZoneInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.zones) > 0 && c.ttl > 0 && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.zones, nil
	}
	z, err := c.fetch(ctx, exec, topOrg, org)
	if err != nil {
		return nil, err // strict: never serve stale on a write path
	}
	if len(z) > 0 {
		c.zones = z
		c.fetchedAt = c.now()
	}
	return z, nil
}

// DescribeFor returns the display name (e.g. "华北一C") for a zone id, or "" when
// the zone is unknown. Case-insensitive on the zone id.
func DescribeFor(list []ZoneInfo, zone string) string {
	zone = strings.TrimSpace(zone)
	for _, z := range list {
		if strings.EqualFold(z.Zone, zone) {
			return z.Describe
		}
	}
	return ""
}

// Label renders a zone for display: "华北一C (cn-bj2-03)" when the describe name
// is known, else the bare zone id. Used for form option labels.
func Label(list []ZoneInfo, zone string) string {
	if d := DescribeFor(list, zone); d != "" {
		return d + " (" + zone + ")"
	}
	return zone
}

// ExactZone resolves a user message to a zone id WITHOUT an LLM when the text
// contains an unambiguous literal: a zone id (cn-bj2-03) or a full display name
// (华北一C). Returns ("", false) when no exact literal is present — the caller
// then decides whether to invoke the LLM matcher. Display-name matching is
// space-insensitive so "华北一 C" still hits "华北一C".
func ExactZone(list []ZoneInfo, userMsg string) (string, bool) {
	lower := strings.ToLower(userMsg)
	squashed := squashSpace(userMsg)
	for _, z := range list {
		if z.Zone != "" && strings.Contains(lower, strings.ToLower(z.Zone)) {
			return z.Zone, true
		}
		if z.Describe != "" && strings.Contains(squashed, squashSpace(z.Describe)) {
			return z.Zone, true
		}
	}
	return "", false
}

// zoneMentionTokens gate whether a message is even talking about a zone, so the
// common case (no zone named) never pays for an LLM call. This is a cheap
// pre-filter, NOT the matching logic — the actual fuzzy match is the LLM's job.
var zoneMentionTokens = []string{
	"可用区", "地域", "机房", "区域", "华北", "华东", "华南", "华中", "西南", "西北", "东北", "cn-",
}

// Mentions reports whether the message plausibly references a zone/region.
func Mentions(userMsg string) bool {
	lower := strings.ToLower(userMsg)
	for _, t := range zoneMentionTokens {
		if strings.Contains(lower, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// Decision is the parsed result of the LLM zone match.
type Decision struct {
	Kind    string // "exact" | "clarify" | "none"
	Zone    string // set when Kind == "exact"
	Clarify string // set when Kind == "clarify": a question/message shown verbatim
}

// MatchSystemPrompt builds the system prompt for the zone matcher over the live
// zone list. The model returns ONE of three decisions so the caller can act
// deterministically: exact (a confident single zone), clarify (a partial /
// ambiguous / unsupported mention → ask the user), none (no zone referenced).
func MatchSystemPrompt(list []ZoneInfo) string {
	var b strings.Builder
	b.WriteString("你是优云智算的可用区匹配器。下面是当前支持的可用区列表（仅这些可选）：\n")
	for _, z := range list {
		b.WriteString(fmt.Sprintf("- %s（zone_id=%s）\n", z.Describe, z.Zone))
	}
	b.WriteString(`根据用户消息判断其想要的可用区，只输出一个 JSON 对象：
{"decision":"exact|clarify|none","zone":"<zone_id>","clarify":"<一句中文追问或说明>"}
规则：
- 用户明确且唯一指向列表中某个可用区（中文名/zone_id/无歧义简称）→ decision="exact"，zone 填该 zone_id，clarify 留空。
- 用户提到可用区但不完整或有歧义（如只说"华北一""华北一区"，未指明具体是哪个；或表述可对应多个）→ decision="clarify"，zone 留空，clarify 写一句追问，主动给出最可能的候选（如"您是指 华北一C 吗？"）。
- 用户想要的可用区不在上面列表中（不支持）→ decision="clarify"，zone 留空，clarify 说明当前仅支持哪些可用区并请其改选。
- 用户根本没提到可用区/地域 → decision="none"，zone 和 clarify 都留空。
只输出 JSON，不要任何额外文字。`)
	return b.String()
}

// ParseDecision parses the matcher JSON (already stripped to a JSON object by
// the caller) and validates the chosen zone against the live list: an "exact"
// decision whose zone is not in the list is downgraded to "none" so a model
// hallucination can never reach the saga as a real zone.
func ParseDecision(raw string, list []ZoneInfo, parse func(string, any) error) Decision {
	var d struct {
		Decision string `json:"decision"`
		Zone     string `json:"zone"`
		Clarify  string `json:"clarify"`
	}
	if err := parse(raw, &d); err != nil {
		return Decision{Kind: "none"}
	}
	kind := strings.ToLower(strings.TrimSpace(d.Decision))
	switch kind {
	case "exact":
		zone := strings.TrimSpace(d.Zone)
		for _, z := range list {
			if strings.EqualFold(z.Zone, zone) {
				return Decision{Kind: "exact", Zone: z.Zone}
			}
		}
		return Decision{Kind: "none"} // hallucinated zone → treat as no zone
	case "clarify":
		if c := strings.TrimSpace(d.Clarify); c != "" {
			return Decision{Kind: "clarify", Clarify: c}
		}
		return Decision{Kind: "none"}
	default:
		return Decision{Kind: "none"}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func u32(v any) uint32 {
	switch n := v.(type) {
	case float64:
		return uint32(n)
	case int:
		return uint32(n)
	case uint32:
		return n
	}
	return 0
}

func boolVal(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(strings.TrimSpace(b), "true")
	default:
		return false
	}
}

func squashSpace(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}
