package capability

import (
	"context"
	"strconv"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/readprojection"
)

// Resource-info lists or filters account instances. Filter references and
// explicit ID/name references are mutually exclusive.

const (
	resourceCapabilityLabel    = string(intent.IntentResourceInfo)
	resourceInfoAction         = "DescribeCompShareInstance"
	resourceTypeInstances      = "instances"
	resourceTypeDisks          = "disks"
	resourceTypeShareBandwidth = "shared_bandwidth"
)

// ResourceInfoRequest is the capability's own request contract.
type ResourceInfoRequest struct {
	ResourceType string               `json:"resource_type,omitempty"`
	Targets      []platform.TargetRef `json:"targets,omitempty"`
	DiskIDs      []string             `json:"disk_ids,omitempty"`
}

// MissingFields: none — an empty target set lists the account's instances.
func (ResourceInfoRequest) MissingFields() []platform.MissingField { return nil }

// ResourceInfoResponse carries the display-ready instance list, its envelope
// metadata and the selection candidates (populated only on a "list all" turn).
type ResourceInfoResponse struct {
	Instances          []entity.InstanceSnapshot
	DiskInfo           *DiskInfoResponse
	ShareBandwidthInfo *ShareBandwidthInfoResponse
	Meta               readprojection.ResourceEnvelopeMeta
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
		Label:       resourceCapabilityLabel,
		Description: "查实例、云盘。查询现有实例的共享带宽归属或切换目标时选择 shared_bandwidth；购买和提速规则另查知识库。",
		Params: objectParam(map[string]schemaNode{
			"resource_type": enumParam(resourceTypeInstances, resourceTypeDisks, resourceTypeShareBandwidth).described("instances 查实例；disks 查云盘/CVolume；shared_bandwidth 查实例 EIP 的共享带宽归属、口径和已有切换目标，不代表测速或购买入口。"),
			"targets":       targetRefsParam(platform.TargetRefFilter).described("实例 ID 或精确名称；省略查全部。filter 仅用于 instances/shared_bandwidth，value 用 all、state=running、state=stopped 或 gpu_type=实时型号；不能与 ID/名称混用。disks 只可指定一台实例。"),
			"disk_ids":      arrayParam(stringParam()).described("磁盘 ID。"),
		}),
		NeedsZoneCatalog: func(req ResourceInfoRequest) bool {
			return req.ResourceType == "" || req.ResourceType == resourceTypeInstances
		},
		Handle:  resourceHandle,
		Render:  resourceRender,
		Observe: resourceObserve,
	}
}

func resourceHandle(ctx context.Context, req ResourceInfoRequest, rt ReadRuntime) (ResourceInfoResponse, ReadResult) {
	if req.ResourceType == resourceTypeDisks {
		resp, terminal := diskInfoHandle(ctx, DiskInfoRequest{Targets: req.Targets, DiskIDs: req.DiskIDs}, rt)
		return ResourceInfoResponse{DiskInfo: &resp}, terminal
	}
	if req.ResourceType == resourceTypeShareBandwidth {
		if len(req.DiskIDs) > 0 {
			return ResourceInfoResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
		}
		resp, terminal := shareBandwidthInfoHandle(ctx, req.Targets, rt)
		return ResourceInfoResponse{ShareBandwidthInfo: &resp}, terminal
	}
	if len(req.DiskIDs) > 0 {
		return ResourceInfoResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
	}
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

	var raw map[string]any
	var err error
	if len(ids) == 0 {
		raw, err = describeAllAccountInstances(ctx, rt.Executor)
	} else {
		raw, err = rt.Executor.Execute(ctx, resourceInfoAction, readprojection.DescribeResourceArgs(ids))
	}
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
		// No matches do not change the account total reported before filtering.
		env := readprojection.BuildResourceEnvelopeWithMetaAndZoneCatalog(nil, envMeta, rt.ZoneCatalog)
		if len(ids) == 0 {
			env.Facts = append(env.Facts, envelope.Fact{
				Key: "account_instance_count", Label: "当前账号实例数", Value: strconv.Itoa(totalCount), Source: envelope.FactSourceAPI,
			})
		} else {
			for _, id := range ids {
				env.Subjects = append(env.Subjects, envelope.Subject{ID: id, Type: envelope.SubjectInstance})
				env.Facts = append(env.Facts, envelope.Fact{
					SubjectID: id, Key: "exists", Label: "当前账号中是否存在", Value: "false", Source: envelope.FactSourceAPI,
				})
			}
		}
		result := ReadEmpty(readprojection.RenderResourceSummary(nil, envMeta))
		result.ToolAction = resourceInfoAction
		result.Envelope = &env
		return ResourceInfoResponse{}, result
	}
	return ResourceInfoResponse{
		Instances:           instances,
		Meta:                envMeta,
		ZoneCatalog:         rt.ZoneCatalog,
		VerifiedInstanceIDs: verifiedIDs,
	}, ReadResult{}
}

func resourceRender(resp ResourceInfoResponse) ReadResult {
	if resp.DiskInfo != nil {
		return diskInfoRender(*resp.DiskInfo)
	}
	if resp.ShareBandwidthInfo != nil {
		return shareBandwidthInfoRender(*resp.ShareBandwidthInfo)
	}
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
	if resp.DiskInfo != nil || resp.ShareBandwidthInfo != nil {
		return nil
	}
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
