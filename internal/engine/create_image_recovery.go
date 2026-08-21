package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/imagecatalogfetch"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// maxCreateRecoveryProbes bounds how many candidate images the create-zone
// recovery probes for availability, so a recovery never hammers the capacity
// API. Ordered by keyword relevance, so the most likely match is probed first.
const maxCreateRecoveryProbes = 8

// isImageUnavailableError reports whether a failed workflow's cause is the
// upstream "image not available in this zone" rejection — RetCode=230 on
// CheckCompShareResourceCapacity ("Params [CompShareImageId] not available").
// Platform images are zone-blind at query time (DescribeCompShareImages has no
// Zone param), so a name-matched image can simply be absent from the resolved
// create-zone; the capacity precheck is the only signal we get.
//
// The code is read off the typed error rather than matched in the message text.
// The previous form, strings.Contains(msg, "230") && strings.Contains(msg,
// "CompShareImageId"), was unanchored on both halves: any "230" anywhere in the
// sentence satisfied it — a memory size, a byte count, a substring of an id — so
// an unrelated failure whose text happened to mention the image param could
// trigger an image swap and re-run.
//
// The param name is still checked, and that is not laziness: upstream shares 230
// between CodeParamsError and CodeParamsConflictError (uhost-compshare-api
// internal/errors/code.go), with a generic "Params [%s] not available" body, so
// the code alone does not identify WHICH param was rejected. Matching the typed
// Message (the upstream body, not our wrapped sentence) is the narrowest signal
// available.
func isImageUnavailableError(err error) bool {
	apiErr, ok := tools.UpstreamAPIErrorFrom(err)
	if !ok {
		return false
	}
	return apiErr.Code == upstreamParamsRejectedCode && strings.Contains(apiErr.Message, "CompShareImageId")
}

// upstreamParamsRejectedCode is upstream's params-rejected RetCode. It is shared
// by several param errors, so it is necessary but not sufficient to identify an
// image rejection — see isImageUnavailableError.
const upstreamParamsRejectedCode = 230

// createImageUnavailable reports whether a CreateInstanceWorkflow result failed
// specifically because the chosen platform image is not available in the
// resolved zone (vs. sold-out stock, insufficient balance, etc.).
//
// Note it reads Result.Err, not Result.Message: a failure we raise ourselves from
// a successful upstream response (the capacity gate's "库存不足") carries no Err
// and is correctly not eligible for image recovery — it needs a different remedy.
func createImageUnavailable(result *workflow.Result) bool {
	return result != nil && !result.Success && isImageUnavailableError(result.Err)
}

// resolveAvailableCreateImage finds a platform image that (a) relates to the
// user's requested image keyword and (b) is actually creatable (single-card
// stock) in the given zone for gpuType — probing capacity per candidate, first
// available wins. Returns ok=false when no relevant image is creatable, so the
// caller falls back to an honest failure reply. failedID is the image that
// already 230'd and is skipped. It runs only after the create's own resolution
// hit the zone-availability wall (RetCode=230).
//
// deployment.ResolveImage is the only image relevance interpreter; recovery
// does not maintain a private matcher.
func (e *Engine) resolveAvailableCreateImage(ctx context.Context, args map[string]any, zone, failedID string, zoneCat *deployment.ZoneCatalogSnapshot) (id, name string, ok bool) {
	keyword, _ := args["ImageName"].(string)
	gpuType, _ := args["GpuType"].(string)
	if strings.TrimSpace(keyword) == "" || strings.TrimSpace(gpuType) == "" {
		return "", "", false
	}
	// Query a BROAD image pool (not the create's name-filtered 20) so recovery has
	// alternatives to probe. It is NOT prefiltered — this pool is unfiltered, so
	// the resolver's client-side name relevance still applies and recovery never
	// swaps in a wholly unrelated image.
	res, err := imagecatalogfetch.FetchAll(ctx, func(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
		result := e.querySafeRead(ctx, action, args)
		if result == nil {
			return nil, fmt.Errorf("%s query failed", action)
		}
		return result, nil
	}, "DescribeCompShareImages", "ImageSet", nil)
	if err != nil {
		return "", "", false
	}
	snap := deployment.NewImageCatalogSnapshot(true, deployment.ParsePlatformImageEntries(res, "platform"))
	resolution := deployment.ResolveImage(snap, deployment.ImageRequest{
		Name:         keyword,
		RequestedGPU: gpuType,
		Source:       "platform",
	})
	for i, c := range recoveryCandidates(resolution, failedID) {
		if i >= maxCreateRecoveryProbes {
			break
		}
		// zoneStockState 230s back to zoneUnknown for an image absent from the
		// zone, so zoneInStock means BOTH "image valid here" AND "has stock".
		if e.zoneStockState(ctx, zone, gpuType, c.ID, zoneCat) == zoneInStock {
			return c.ID, c.Name, true
		}
	}
	return "", "", false
}

// recoveryCandidates flattens a resolution into the ranked images recovery should
// probe — the resolved selection first (an exact name match), then the resolver's
// ranked near-matches — dropping the id that already 230'd. It carries NO ranking
// of its own: the order is exactly what deployment.ResolveImage produced.
func recoveryCandidates(res deployment.ImageResolution, failedID string) []deployment.ImageSelection {
	var out []deployment.ImageSelection
	seen := map[string]struct{}{}
	add := func(sel deployment.ImageSelection) {
		if sel.ID == "" || strings.EqualFold(sel.ID, failedID) {
			return
		}
		if _, dup := seen[strings.ToLower(sel.ID)]; dup {
			return
		}
		seen[strings.ToLower(sel.ID)] = struct{}{}
		out = append(out, sel)
	}
	if res.Status == deployment.ResolutionResolved {
		add(res.Selection)
	}
	for _, c := range res.Candidates {
		add(c)
	}
	return out
}
