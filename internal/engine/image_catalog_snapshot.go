package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/imagecatalogfetch"
	"github.com/compshare-agent/internal/tools"
)

// imageCatalogSnapshotForSpec is the spec-gated builder the action-proposal resolver
// uses to decide "does this operation need the image catalog?" and, when it does,
// build exactly one snapshot per turn — which the resolver verifies an explicit
// CompShareImageId against AND threads into executeWorkflow via ReferenceData, so
// the resolver and the workflow share one image catalog (the single authority that
// ends the three-interpreter image resolution). Non-image ops get nil, exactly as
// zoneCatalogSnapshotForSpec returns nil for non-zone ops.
func (e *Engine) imageCatalogSnapshotForSpec(ctx context.Context, spec actionresolver.OperationSpec, source, proposedImageID string) *deployment.ImageCatalogSnapshot {
	snapshot, _ := e.resolveImageCatalogSnapshotForSpec(ctx, spec, source, proposedImageID, true)
	return snapshot
}

// resolveImageCatalogSnapshotForSpec verifies one proposed image id with an
// upstream point query and returns the source that actually contained it.
//
// A caller-provided source is strict only when the user explicitly supplied it
// (or the operation fixes it). For an Agent-inferred or omitted source, candidates
// are tried in deterministic order and the first exact id match supplies the
// canonical source. This keeps a community id from silently falling through the
// historical empty-source => platform default while still refusing to guess from
// a name.
func (e *Engine) resolveImageCatalogSnapshotForSpec(
	ctx context.Context,
	spec actionresolver.OperationSpec,
	preferredSource, proposedImageID string,
	strictSource bool,
) (*deployment.ImageCatalogSnapshot, string) {
	if !actionresolver.SpecNeedsImageCatalog(spec) {
		return nil, ""
	}
	id := strings.TrimSpace(proposedImageID)
	if id == "" {
		return nil, ""
	}

	sources := imageCatalogSourcesForSpec(spec, preferredSource, strictSource)
	var unavailable bool
	for _, source := range sources {
		snapshot := e.imageCatalogSnapshotByID(ctx, source, id)
		if !snapshot.Available() {
			unavailable = true
			continue
		}
		if _, ok := snapshot.ByID(id); ok {
			return snapshot, normalizeImageSource(source)
		}
	}
	if unavailable {
		return deployment.NewImageCatalogSnapshot(false, nil), ""
	}
	// Every candidate source answered successfully and none contained the id:
	// available-but-empty is the resolver's ordinary invalid-value verdict.
	return deployment.NewImageCatalogSnapshot(true, nil), ""
}

// imageCatalogSnapshotByID verifies one exact id within the requested source.
// Platform and community APIs safely support CompShareImageId point reads. Their
// successful wire shapes differ:
//
//   - platform returns the matching row in ImageSet but currently leaves
//     TotalCount=0 even when that row exists;
//   - community returns one CompshareImageGroup with the matching version in Data.
//
// Custom point reads are intentionally not used here because that upstream
// branch changes tenant scope when given a known id. Sharing exact reads remain
// tenant-scoped upstream and can use the same precise path as public catalogs.
//
// Resolution therefore reads the rows, never TotalCount. A typed "this image does
// not exist in this source" response is an available-but-empty catalog; transport
// and unrelated upstream failures remain unavailable so the resolver does not
// blame the user's id for our dependency failure.
func (e *Engine) imageCatalogSnapshotByID(ctx context.Context, source, imageID string) *deployment.ImageCatalogSnapshot {
	if e.externalExecutor == nil {
		return deployment.NewImageCatalogSnapshot(false, nil)
	}
	action, community, canonical := imageQueryForSource(source)
	if canonical == "custom" {
		return e.tenantScopedImageCatalogSnapshot(ctx, action, canonical)
	}
	args := imageCatalogExactQueryArgs(community, imageID)
	result, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: tools.OriginWorkflowInternal,
	})
	if err != nil {
		if isImageCatalogMissError(action, imageID, err) {
			return deployment.NewImageCatalogSnapshot(true, nil)
		}
		return deployment.NewImageCatalogSnapshot(false, nil)
	}
	var res map[string]any
	if result != nil {
		res = result.RawResult
	}
	var entries []deployment.ImageCatalogEntry
	if community {
		entries = deployment.ParseCommunityImageEntries(res)
	} else {
		entries = deployment.ParsePlatformImageEntries(res, canonical)
	}
	return deployment.NewImageCatalogSnapshot(true, entries)
}

// tenantScopedImageCatalogSnapshot preserves the visibility contract of the
// custom-image list API. FetchAll supplies bounded Limit/Offset pagination; no
// CompShareImageId is sent upstream, so the result cannot escape the caller's
// tenant-scoped list and ByID remains a local exact match.
func (e *Engine) tenantScopedImageCatalogSnapshot(ctx context.Context, action, source string) *deployment.ImageCatalogSnapshot {
	res, err := imagecatalogfetch.FetchAll(
		ctx,
		func(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
			result, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
				Action: action,
				Args:   args,
				Origin: tools.OriginWorkflowInternal,
			})
			if err != nil {
				return nil, err
			}
			if result == nil {
				return nil, nil
			}
			return result.RawResult, nil
		},
		action,
		"ImageSet",
		nil,
	)
	if err != nil {
		return deployment.NewImageCatalogSnapshot(false, nil)
	}
	return deployment.NewImageCatalogSnapshot(
		true,
		deployment.ParsePlatformImageEntries(res, source),
	)
}

// isImageCatalogMissError classifies a safe exact-id listing action's "not in
// this source" outcomes. It is deliberately narrower than the global RetCode
// hints: 230 is shared by many parameter failures and only counts here when the
// typed upstream message identifies CompShareImageId. RetCode 8039 is the
// community endpoint's resource-not-exists code. Other actions, other 230s,
// transport failures, and service errors remain dependency failures.
func isImageCatalogMissError(action, imageID string, err error) bool {
	if strings.TrimSpace(imageID) == "" {
		return false
	}
	switch action {
	case "DescribeCompShareImages", "DescribeCommunityImages":
	default:
		return false
	}
	if isImageUnavailableError(err) {
		return true
	}
	apiErr, ok := tools.UpstreamAPIErrorFrom(err)
	if !ok {
		return false
	}
	return action == "DescribeCommunityImages" && apiErr.Code == 8039
}

func imageCatalogSourcesForSpec(spec actionresolver.OperationSpec, preferred string, strict bool) []string {
	preferred = normalizeImageSource(preferred)
	if fixed := strings.TrimSpace(spec.ImageCatalogSource); fixed != "" {
		return []string{normalizeImageSource(fixed)}
	}
	if strict {
		return []string{preferred}
	}
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	appendSource := func(source string) {
		source = normalizeImageSource(source)
		if source == "" || seen[source] {
			return
		}
		seen[source] = true
		out = append(out, source)
	}
	if strings.TrimSpace(preferred) != "" {
		appendSource(preferred)
	}
	if field, ok := spec.Fields["ImageSource"]; ok {
		for _, source := range field.Enum {
			appendSource(source)
		}
	}
	if len(out) == 0 {
		appendSource("platform")
	}
	return out
}

// imageQueryForSource maps an ImageSource to its upstream listing action, whether it
// uses the grouped community shape, and the canonical source tag stored on entries.
func imageQueryForSource(source string) (action string, community bool, canonical string) {
	switch normalizeImageSource(source) {
	case "community":
		return "DescribeCommunityImages", true, "community"
	case "custom":
		return "DescribeCompShareCustomImages", false, "custom"
	case "sharing":
		return "DescribeCompShareSharingImages", false, "sharing"
	default:
		return "DescribeCompShareImages", false, "platform"
	}
}

func normalizeImageSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "community":
		return "community"
	case "custom":
		return "custom"
	case "shared", "sharing":
		return "sharing"
	default:
		return "platform"
	}
}

func imageCatalogExactQueryArgs(community bool, imageID string) map[string]any {
	args := map[string]any{
		"CompShareImageId": strings.TrimSpace(imageID),
		"Limit":            100,
	}
	if community {
		args["ExcludeReadme"] = true
	}
	return args
}

// proposalSlotString returns a string-valued slot from a proposal, or "" — used to
// read the declared ImageSource so the image snapshot is fetched from the right
// listing before the resolver runs.
func proposalSlotString(proposal actionresolver.ActionProposal, name string) string {
	candidate, ok := proposalSlotCandidate(proposal, name)
	if !ok {
		return ""
	}
	if value, ok := candidate.Value.(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// proposalSlotCandidate reads the first model-proposed value for prerequisite
// catalog work. Generated Request* tools produce at most one value per field;
// duplicate legacy slots still reach Resolver, which owns conflict handling.
func proposalSlotCandidate(proposal actionresolver.ActionProposal, name string) (actionresolver.SlotCandidate, bool) {
	for _, candidate := range proposal.Slots {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return actionresolver.SlotCandidate{}, false
}
