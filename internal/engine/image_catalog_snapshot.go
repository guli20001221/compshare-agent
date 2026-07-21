package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/imagecatalogfetch"
)

// imageCatalogSnapshotForSpec is the spec-gated builder the action-proposal resolver
// uses to decide "does this operation need the image catalog?" and, when it does,
// build exactly one snapshot per turn — which the resolver verifies an explicit
// CompShareImageId against AND threads into executeWorkflow via ReferenceData, so
// the resolver and the workflow share one image catalog (the single authority that
// ends the three-interpreter image resolution). Non-image ops get nil, exactly as
// zoneCatalogSnapshotForSpec returns nil for non-zone ops.
func (e *Engine) imageCatalogSnapshotForSpec(ctx context.Context, spec actionresolver.OperationSpec, source string) *deployment.ImageCatalogSnapshot {
	if !actionresolver.SpecNeedsImageCatalog(spec) {
		return nil
	}
	return e.imageCatalogSnapshot(ctx, source)
}

// imageCatalogSnapshot builds the turn's read-only image catalog from the live
// image listing for the requested source (default platform). Mirroring the zone
// snapshot: the network call and its failure mode live HERE so the resolver stays a
// pure function of its inputs. Only a FAILURE to obtain the catalog (no executor or
// a query error) yields an UNAVAILABLE snapshot — never a fallback; a successful
// query with zero images is an available (empty) catalog, not unavailable. It is
// scoped to ONE source because an id or name is resolved within the source the user
// declared; a create/reinstall for a different source re-fetches its own catalog.
func (e *Engine) imageCatalogSnapshot(ctx context.Context, source string) *deployment.ImageCatalogSnapshot {
	if e.externalExecutor == nil {
		return deployment.NewImageCatalogSnapshot(false, nil)
	}
	action, community, canonical := imageQueryForSource(source)
	listKey := "ImageSet"
	if community {
		listKey = "CompshareImageGroup"
	}
	res, err := imagecatalogfetch.FetchAll(ctx, func(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
		result := e.querySafeRead(ctx, action, args)
		if result == nil {
			return nil, fmt.Errorf("%s query failed", action)
		}
		return result, nil
	}, action, listKey, imageCatalogQueryArgs(community))
	if err != nil {
		return deployment.NewImageCatalogSnapshot(false, nil)
	}
	var entries []deployment.ImageCatalogEntry
	if community {
		entries = deployment.ParseCommunityImageEntries(res)
	} else {
		entries = deployment.ParsePlatformImageEntries(res, canonical)
	}
	return deployment.NewImageCatalogSnapshot(true, entries)
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

func imageCatalogQueryArgs(community bool) map[string]any {
	if community {
		return map[string]any{"Limit": 100, "ExcludeReadme": true}
	}
	return map[string]any{"Limit": 100}
}

// proposalSlotString returns a string-valued slot from a proposal, or "" — used to
// read the declared ImageSource so the image snapshot is fetched from the right
// listing before the resolver runs.
func proposalSlotString(proposal actionresolver.ActionProposal, name string) string {
	for _, s := range proposal.Slots {
		if s.Name == name {
			if v, ok := s.Value.(string); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}
