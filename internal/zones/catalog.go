// Package zones resolves CompShare availability zones from the live upstream
// catalog, including the human "可用区显示名称" (Describe, e.g. "华北一C") that
// only DescribeCompShareSupportZone exposes, so a zone named in Chinese ("华北一C")
// resolves to its zone id (cn-bj2-03) instead of being dropped to the default.
//
// It is an executor-backed, process-cached (TTL) zone list plus pure exact-lookup
// helpers (ExactZone / DescribeFor / Label). Fuzzy or partial zone interpretation
// is NOT here — that belongs to the action resolver's CodecZone, which canonicalizes
// a zone against this same live list and refuses (asks) on an ambiguous mention.
package zones

import (
	"context"
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
