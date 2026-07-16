package capability

import (
	"context"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/readprojection"
)

// Resource-info read capability (migrated from the legacy intent route). It
// lists / filters the account's instances via DescribeCompShareInstance and
// renders them deterministically. Filter refs and explicit id/name refs are
// mutually exclusive (a filter set describes "all matching", an id/name set
// pins exact targets) — the same contract the legacy handler enforced.

const (
	resourceCapabilityLabel = string(intent.IntentResourceInfo)
	resourceInfoAction      = "DescribeCompShareInstance"
)

// ResourceInfoRequest is the capability's own request contract.
type ResourceInfoRequest struct {
	Targets []platform.TargetRef `json:"targets,omitempty"`
}

// MissingFields: none — an empty target set lists the account's instances.
func (ResourceInfoRequest) MissingFields() []platform.MissingField { return nil }

// ResourceInfoResponse carries the display-ready instance list, its envelope
// metadata and the selection candidates (populated only on a "list all" turn).
type ResourceInfoResponse struct {
	Instances  []entity.InstanceSnapshot
	Meta       readprojection.ResourceEnvelopeMeta
	Candidates []entity.InstanceSnapshot
}

func resourceReadSpec() ReadCapabilitySpec[ResourceInfoRequest, ResourceInfoResponse] {
	return ReadCapabilitySpec[ResourceInfoRequest, ResourceInfoResponse]{
		Label:       resourceCapabilityLabel,
		Description: "查询当前账号的实例列表、实例状态和实例配置。",
		Schema:      objectSchema(map[string]any{"targets": targetRefsSchema()}, nil),
		Handle:      resourceHandle,
		Render:      resourceRender,
	}
}

func resourceHandle(ctx context.Context, req ResourceInfoRequest, rt ReadRuntime) (ResourceInfoResponse, ReadResult) {
	var ids []string
	var filters readprojection.ResourceFilterSet
	hasFilters := readprojection.ContainsFilterRef(req.Targets)
	if hasFilters {
		parsed, err := readprojection.ParseResourceFilters(req.Targets)
		if err != nil {
			return ResourceInfoResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
		}
		filters = parsed
	} else {
		_, resolvedIDs, reason := resolveReadTargetSnapshots(req.Targets, rt.Resolver)
		if reason != nil {
			return ResourceInfoResponse{}, ReadFallbackBeforeTool(*reason)
		}
		ids = resolvedIDs
	}

	args := readprojection.DescribeResourceArgs(ids)
	raw, err := rt.Executor.Execute(ctx, resourceInfoAction, args)
	if err != nil {
		return ResourceInfoResponse{}, ReadFailureAfterTool(resourceInfoAction, resourceCapabilityLabel, err)
	}
	describeData, err := readprojection.InstancesFromDescribeResult(raw)
	if err != nil {
		// Parse failure carries no actionable upstream message — generic read
		// failure, matching the legacy failureAfterToolWithTrace path.
		return ResourceInfoResponse{}, ReadFailureAfterTool(resourceInfoAction, resourceCapabilityLabel, err)
	}

	instances := describeData.Instances
	totalCount := describeData.TotalCount
	if hasFilters {
		instances = readprojection.ApplyResourceFilters(instances, filters)
	}
	envMeta := readprojection.ResourceEnvelopeMeta{TotalCount: totalCount}
	if hasFilters && !filters.IsZero() {
		envMeta.FilterApplied = filters.String()
		envMeta.MatchedCount = len(instances)
	}
	// Display truncation only when the caller did not pin a specific id set
	// ("list my instances" / "list before a write op"); explicit ids are never
	// truncated. The kept list becomes the selection candidates.
	var candidates []entity.InstanceSnapshot
	if len(ids) == 0 {
		truncated, shown, isTruncated := readprojection.TruncateInstancesForDisplay(instances, 0)
		instances = truncated
		candidates = append([]entity.InstanceSnapshot(nil), instances...)
		envMeta.Shown = shown
		envMeta.Truncated = isTruncated
	}
	return ResourceInfoResponse{Instances: instances, Meta: envMeta, Candidates: candidates}, ReadResult{}
}

func resourceRender(resp ResourceInfoResponse) ReadResult {
	r := ReadHandled(readprojection.RenderResourceSummary(resp.Instances, resp.Meta))
	r.ToolAction = resourceInfoAction
	env := readprojection.BuildResourceEnvelopeWithMeta(resp.Instances, resp.Meta)
	r.Envelope = &env
	if len(resp.Candidates) > 0 {
		r.ResourceSelectionCandidates = resp.Candidates
	}
	return r
}
