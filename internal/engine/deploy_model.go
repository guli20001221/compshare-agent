package engine

import (
	"context"
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/zones"
)

// deploy_model.go holds the zone-resolution and create-time stock helpers used by
// the single workflow execution path. The instance-create path resolves the
// zone/GPU here before the workflow runs. This file owns the write-path zone
// catalog snapshot builder (the single reference the resolver and the workflow
// share) and the create-time stock helpers. A user-named zone is resolved once, by
// the action resolver's CodecZone against the live catalog.

// supportZoneListStrict is the write-path support-zone fetch, without a serve-stale fallback: an
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

// zoneCatalogSnapshot builds the turn's read-only zone catalog from the live
// support-zone list, one structured placement per zone plus its console display
// name — the SINGLE reference the resolver validates against and the workflow
// looks placements up in, replacing the four parallel zone-keyed maps.
//
// The network call, its failure mode and caching live HERE (the same
// process-cached supportZoneList the old chain used, so this adds a cache read,
// not an API call), so the snapshot the workflow consumes is pure data. Only a
// FAILURE to obtain the catalog (no executor or a query error) yields an
// UNAVAILABLE snapshot — never a fallback: a consumer must refuse rather than
// guess a zone from a stale table, exactly as for the machine-type catalog. A
// query that SUCCEEDS with zero zones is an available (empty) catalog, not
// unavailable. Each record carries its own Region (falling back to the
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

// zoneCatalogSnapshotForSpec is the spec-gated builder the action-proposal
// resolver uses to decide "does this operation need the zone catalog?" and, when
// it does, build exactly one snapshot per turn — which the resolver then threads
// into executeWorkflow, the sole consumer (executeWorkflow never builds its own).
func (e *Engine) zoneCatalogSnapshotForSpec(ctx context.Context, spec actionresolver.OperationSpec) *deployment.ZoneCatalogSnapshot {
	if !actionresolver.SpecNeedsZoneCatalog(spec) {
		return nil
	}
	return e.zoneCatalogSnapshot(ctx)
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
	raw, _ := e.querySafeReadResult(ctx, action, args)
	return raw
}

// querySafeReadResult is querySafeRead that preserves the failure/empty
// distinction the plain form collapses. ok=false means the CALL failed (transport
// error / upstream error) — a dependency failure; ok=true means it completed,
// whether the raw result is populated or empty. Existence verification needs this:
// a failed DescribeCompShareInstance must not read as "the instance is absent".
func (e *Engine) querySafeReadResult(ctx context.Context, action string, args map[string]any) (map[string]any, bool) {
	res, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: tools.OriginWorkflowInternal,
	})
	if err != nil {
		return nil, false
	}
	if res == nil {
		return nil, true
	}
	return res.RawResult, true
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
