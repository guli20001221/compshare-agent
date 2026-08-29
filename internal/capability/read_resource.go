package capability

import (
	"context"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/readprojection"
)

// Resource-info lists or filters account instances. Filter references and
// explicit ID/name references are mutually exclusive.

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
	Instances []entity.InstanceSnapshot
	Meta      readprojection.ResourceEnvelopeMeta
	// ZoneCatalog is immutable engine-owned reference data captured for this
	// request. It lets the pure renderer project the raw Zone code and the live
	// console display name from one catalog row.
	ZoneCatalog *deployment.ZoneCatalogSnapshot
	// VerifiedInstanceIDs are the exact ids the upstream DescribeCompShareInstance
	// response echoed this turn — the same-id-verified existence evidence the write
	// path may trust. Derived from the RESPONSE, never from the request.
	VerifiedInstanceIDs []string
}

func resourceReadSpec() ReadCapabilitySpec[ResourceInfoRequest, ResourceInfoResponse] {
	return ReadCapabilitySpec[ResourceInfoRequest, ResourceInfoResponse]{
		Label:            resourceCapabilityLabel,
		Description:      "查询当前账号已有实例的列表、状态和基础配置，也用于按 ID 或名称核实实例。可用区给实例代码和实时目录名；无法核验时只给代码。只反映账号内资源，不查 GPU 库存或平台应用入口；应用入口用 instance_access software。",
		Params:           objectParam(map[string]schemaNode{"targets": targetRefsParam()}),
		NeedsZoneCatalog: func(ResourceInfoRequest) bool { return true },
		Handle:           resourceHandle,
		Render:           resourceRender,
		Observe:          resourceObserve,
	}
}

func resourceHandle(ctx context.Context, req ResourceInfoRequest, rt ReadRuntime) (ResourceInfoResponse, ReadResult) {
	var ids []string
	var filters readprojection.ResourceFilterSet
	hasFilters := readprojection.ContainsFilterRef(req.Targets)
	if hasFilters {
		parsed, err := readprojection.ParseResourceFilters(req.Targets)
		if err != nil {
			// A model can copy a real instance reference from a shell prompt or platform URL while
			// mislabelling it as `filter` (production case 124). Do not maintain URL suffix or
			// instance-ID length rules here. Recovery is intentionally narrower than ordinary name
			// resolution: it applies only to one mislabelled ref, and the uniquely resolved account ID
			// must literally occur in the original value. Thus a malformed filter that merely equals an
			// instance display name, or a filter mixed with explicit targets, keeps the original validation
			// semantics. Valid state/GPU filters never enter this branch.
			if len(req.Targets) != 1 || req.Targets[0].Type != platform.TargetRefFilter {
				return ResourceInfoResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
			}
			original := req.Targets[0].Value
			targetRef := req.Targets[0]
			targetRef.Type = platform.TargetRefName
			_, recoveredIDs, reason := resolveReadTargetSnapshots(ctx, []platform.TargetRef{targetRef}, rt)
			if reason != nil || len(recoveredIDs) != 1 || !platform.ContainsLiteralSpan(original, recoveredIDs[0]) {
				return ResourceInfoResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
			}
			ids = recoveredIDs
			hasFilters = false
		} else {
			filters = parsed
		}
	} else {
		// Existence is established from the DescribeCompShareInstance response
		// (re-parsed below), never from the registry snapshot, so an id the local
		// registry has not seen is point-queried rather than refused before the call.
		_, resolvedIDs, reason := resolveReadTargetSnapshots(ctx, req.Targets, rt)
		if reason != nil {
			return ResourceInfoResponse{}, readTargetFallbackResult(*reason)
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
		// Parse failure carries no actionable upstream message.
		return ResourceInfoResponse{}, ReadFailureAfterTool(resourceInfoAction, resourceCapabilityLabel, err)
	}
	// A list/filter request just received a complete account listing. Keep the
	// session resolver current with that same authoritative response: subsequent
	// same-turn execution checks may safely rely on its account-single proof.
	// Point queries deliberately do not replace the registry with their partial
	// response.
	if len(ids) == 0 && rt.SyncRegistry != nil {
		rt.SyncRegistry(raw)
	}

	instances := describeData.Instances
	totalCount := describeData.TotalCount
	if len(ids) > 0 {
		// Same-id contract, shared with the write path's ExactTargetVerifier: keep
		// only instances the response ACTUALLY echoed for the requested ids. An
		// upstream that ignores the filter and returns a different instance must not
		// be rendered as the one the user asked about (it would claim a resource
		// exists, or show the wrong one's state).
		instances = filterInstancesByRequestedID(instances, ids)
	}
	// Existence evidence for the write path: exactly the ids present in this
	// response. Captured BEFORE display filters/truncation, which are presentation
	// concerns — an instance the account really has still exists whether or not it
	// matches a state filter or fits in the display window.
	verifiedIDs := instanceIDsOf(instances)
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
	// truncated. The typed Observe effect below carries exactly these displayed
	// rows to the engine for ordinal selection; hidden/truncated rows never enter it.
	if len(ids) == 0 {
		truncated, shown, isTruncated := readprojection.TruncateInstancesForDisplay(instances, 0)
		instances = truncated
		envMeta.Shown = shown
		envMeta.Truncated = isTruncated
	}
	if len(instances) == 0 {
		// Query succeeded but nothing is present/matched — a structured Empty read.
		// The Agent pairs this with CanAssertAbsence to state "you have none".
		return ResourceInfoResponse{}, ReadEmpty(readprojection.RenderResourceSummary(nil, envMeta))
	}
	return ResourceInfoResponse{
		Instances:           instances,
		Meta:                envMeta,
		ZoneCatalog:         rt.ZoneCatalog,
		VerifiedInstanceIDs: verifiedIDs,
	}, ReadResult{}
}

func resourceRender(resp ResourceInfoResponse) ReadResult {
	r := ReadHandled(readprojection.RenderResourceSummaryWithZoneCatalog(resp.Instances, resp.Meta, resp.ZoneCatalog))
	r.ToolAction = resourceInfoAction
	env := readprojection.BuildResourceEnvelopeWithMetaAndZoneCatalog(resp.Instances, resp.Meta, resp.ZoneCatalog)
	r.Envelope = &env
	return r
}

// resourceObserve declares the same-id-verified instance ids as the write path's
// existence evidence. resource_info is the ONLY read that emits it: its subjects
// come from the upstream response, not a pre-query snapshot.
func resourceObserve(resp ResourceInfoResponse) []ReadEffect {
	var effects []ReadEffect
	if len(resp.VerifiedInstanceIDs) > 0 {
		effects = append(effects, RememberVerifiedInstances{IDs: append([]string(nil), resp.VerifiedInstanceIDs...)})
	}
	if len(resp.Instances) > 1 {
		effects = append(effects, RememberDisplayedInstances{Instances: append([]entity.InstanceSnapshot(nil), resp.Instances...)})
	}
	return effects
}

// filterInstancesByRequestedID keeps only instances whose UHostId is one of the
// requested ids — the response-echo half of the same-id contract.
func filterInstancesByRequestedID(instances []entity.InstanceSnapshot, requested []string) []entity.InstanceSnapshot {
	if len(requested) == 0 {
		return instances
	}
	want := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		want[id] = struct{}{}
	}
	out := make([]entity.InstanceSnapshot, 0, len(instances))
	for _, inst := range instances {
		if _, ok := want[inst.UHostId]; ok {
			out = append(out, inst)
		}
	}
	return out
}

func instanceIDsOf(instances []entity.InstanceSnapshot) []string {
	ids := make([]string, 0, len(instances))
	for _, inst := range instances {
		if inst.UHostId != "" {
			ids = append(ids, inst.UHostId)
		}
	}
	return ids
}
