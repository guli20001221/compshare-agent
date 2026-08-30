package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/readprojection"
)

type ShareBandwidthEIP struct {
	IP               string
	Scope            string
	ShareBandwidthID string
	Bandwidth        uint32
	Status           string
	CanSwitch        bool
	TargetScope      string
}

type ShareBandwidthInstance struct {
	Instance entity.InstanceSnapshot
	EIPs     []ShareBandwidthEIP
}

type ShareBandwidthInfoResponse struct {
	Instances []ShareBandwidthInstance
	Meta      readprojection.ResourceEnvelopeMeta
}

func shareBandwidthInfoHandle(ctx context.Context, targets []platform.TargetRef, rt ReadRuntime) (ShareBandwidthInfoResponse, ReadResult) {
	var ids []string
	var filters readprojection.ResourceFilterSet
	hasFilters := readprojection.ContainsFilterRef(targets)
	if hasFilters {
		parsed, err := readprojection.ParseResourceFilters(targets)
		if err != nil {
			return ShareBandwidthInfoResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
		}
		filters = parsed
	} else {
		_, resolvedIDs, reason := resolveReadTargetSnapshots(ctx, targets, rt)
		if reason != nil {
			return ShareBandwidthInfoResponse{}, readTargetFallbackResult(*reason)
		}
		ids = resolvedIDs
	}

	args := map[string]any{"IncludeShareBandwidth": true}
	var raw map[string]any
	var err error
	if len(ids) == 0 {
		raw, err = describeAllAccountInstancesInternalWithArgs(ctx, rt.Executor, args)
	} else {
		args["UHostIds"] = append([]string(nil), ids...)
		raw, err = rt.Executor.ExecuteInternal(ctx, resourceInfoAction, args)
	}
	if err != nil {
		return ShareBandwidthInfoResponse{}, ReadFailureAfterTool(resourceInfoAction, resourceCapabilityLabel, err)
	}

	describeData, err := readprojection.InstancesFromDescribeResult(raw)
	if err != nil {
		return ShareBandwidthInfoResponse{}, ReadFailureAfterTool(resourceInfoAction, resourceCapabilityLabel, err)
	}
	instances := describeData.Instances
	if len(ids) > 0 {
		instances = filterInstancesByRequestedID(instances, ids)
	}
	if hasFilters {
		instances = readprojection.ApplyResourceFilters(instances, filters)
	}
	if len(ids) == 0 && rt.SyncRegistry != nil {
		rt.SyncRegistry(raw)
	}
	if len(instances) == 0 {
		switch {
		case len(ids) > 0:
			return ShareBandwidthInfoResponse{}, ReadEmpty("查询成功，但没有返回指定实例。")
		case hasFilters:
			return ShareBandwidthInfoResponse{}, ReadEmpty("没有符合筛选条件的实例。")
		default:
			return ShareBandwidthInfoResponse{}, ReadEmpty("当前账号没有查询到实例。")
		}
	}

	meta := readprojection.ResourceEnvelopeMeta{TotalCount: describeData.TotalCount}
	if hasFilters && !filters.IsZero() {
		meta.FilterApplied = filters.String()
		meta.MatchedCount = len(instances)
	}
	if len(ids) == 0 {
		instances, meta.Shown, meta.Truncated = readprojection.TruncateInstancesForDisplay(instances, 0)
	}

	byID := make(map[string]ShareBandwidthInstance, len(instances))
	order := make([]string, 0, len(instances))
	for _, inst := range instances {
		if inst.UHostId == "" {
			continue
		}
		byID[inst.UHostId] = ShareBandwidthInstance{Instance: inst}
		order = append(order, inst.UHostId)
	}
	for _, item := range mapSliceAt(raw, "UHostSet") {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(row, "UHostId")
		entry, wanted := byID[id]
		if !wanted {
			continue
		}
		for _, ipAny := range mapSliceAt(row, "IPSet") {
			ip, ok := ipAny.(map[string]any)
			if !ok {
				continue
			}
			share, ok := ip["ShareBandwidth"].(map[string]any)
			if !ok || len(share) == 0 {
				continue
			}
			bandwidth, _ := numericField(share, "Bandwidth")
			canSwitch, _ := boolField(share, "CanSwitch")
			entry.EIPs = append(entry.EIPs, ShareBandwidthEIP{
				IP:               stringField(ip, "IP"),
				Scope:            stringField(share, "Scope"),
				ShareBandwidthID: stringField(share, "ShareBandwidthId"),
				Bandwidth:        uint32(bandwidth),
				Status:           stringField(share, "Status"),
				CanSwitch:        canSwitch,
				TargetScope:      stringField(share, "TargetScope"),
			})
		}
		byID[id] = entry
	}

	result := ShareBandwidthInfoResponse{Instances: make([]ShareBandwidthInstance, 0, len(order)), Meta: meta}
	for _, id := range order {
		result.Instances = append(result.Instances, byID[id])
	}
	return result, ReadResult{}
}

func shareBandwidthInfoRender(resp ShareBandwidthInfoResponse) ReadResult {
	lines := []string{"实例公网共享带宽（只读实时查询）："}
	instances := make([]entity.InstanceSnapshot, 0, len(resp.Instances))
	for _, item := range resp.Instances {
		instances = append(instances, item.Instance)
	}
	env := readprojection.BuildResourceEnvelopeWithMeta(instances, resp.Meta)
	env.Facts = nil
	env.Constraints.DoNotInventMetrics = true
	for _, item := range resp.Instances {
		inst := item.Instance
		name := strings.TrimSpace(inst.Name)
		if name == "" {
			name = inst.UHostId
		}
		if len(item.EIPs) == 0 {
			lines = append(lines, fmt.Sprintf("- %s（%s）：上游未返回公网 EIP 的共享带宽归属。", name, inst.UHostId))
			env.Facts = append(env.Facts, envelope.Fact{SubjectID: inst.UHostId, Key: "share_bandwidth_reported", Label: "是否返回共享带宽归属", Value: false, Source: envelope.FactSourceAPI})
			continue
		}
		for index, eip := range item.EIPs {
			prefix := fmt.Sprintf("eip_%d_", index+1)
			label := shareBandwidthScopeLabel(eip.Scope)
			if eip.Bandwidth > 0 {
				label += fmt.Sprintf(" %d %s", eip.Bandwidth, shareBandwidthUnit(eip.Scope))
			}
			if eip.ShareBandwidthID != "" && strings.EqualFold(eip.Scope, "Company") {
				label += "（" + eip.ShareBandwidthID + "）"
			}
			if eip.Status != "" {
				label += "，状态 " + eip.Status
			}
			if switchLabel := shareBandwidthSwitchLabel(eip); switchLabel != "" {
				label += "，" + switchLabel
			}
			ipLabel := eip.IP
			if ipLabel == "" {
				ipLabel = "公网 EIP"
			}
			lines = append(lines, fmt.Sprintf("- %s（%s），%s：%s。", name, inst.UHostId, ipLabel, label))
			addShareBandwidthFact(&env, inst.UHostId, prefix+"scope", "当前共享带宽归属", eip.Scope, "")
			if eip.IP != "" {
				addShareBandwidthFact(&env, inst.UHostId, prefix+"ip", "公网 IP", eip.IP, "")
			}
			if eip.Bandwidth > 0 {
				addShareBandwidthFact(&env, inst.UHostId, prefix+"bandwidth", "所属共享出口的产品带宽（非单实例带宽保证）", eip.Bandwidth, shareBandwidthUnit(eip.Scope))
			}
			if strings.EqualFold(eip.Scope, "Company") && eip.ShareBandwidthID != "" {
				addShareBandwidthFact(&env, inst.UHostId, prefix+"share_bandwidth_id", "公司独享共享带宽 ID", eip.ShareBandwidthID, "")
			}
			if eip.Status != "" {
				addShareBandwidthFact(&env, inst.UHostId, prefix+"status", "共享带宽状态", eip.Status, "")
			}
			addShareBandwidthFact(&env, inst.UHostId, prefix+"can_switch", "当前是否已有可切换的目标共享带宽池", eip.CanSwitch, "")
			if eip.TargetScope != "" {
				addShareBandwidthFact(&env, inst.UHostId, prefix+"target_scope", "当前可切换的既有目标归属", eip.TargetScope, "")
			}
			addShareBandwidthComputed(&env, inst.UHostId, prefix+"switch_interpretation", "切换字段含义", "仅表示当前已有可切换的目标池，不证明支持购买、扩容或存在控制台入口")
			if strings.EqualFold(eip.Scope, "Public") {
				addShareBandwidthComputed(&env, inst.UHostId, prefix+"public_scope_interpretation", "公共共享带宽含义", "平台公共共享出口的产品口径，不是单实例保底带宽或端到端测速结果")
			}
		}
	}
	lines = append(lines, "Public 表示平台公共共享出口；Company 表示另购的公司独享共享带宽。查询结果不等于端到端传输测速。")
	r := ReadHandled(strings.Join(lines, "\n"))
	r.ToolAction = resourceInfoAction
	r.Envelope = &env
	return r
}

func addShareBandwidthFact(env *envelope.Envelope, subjectID, key, label string, value any, unit string) {
	env.Facts = append(env.Facts, envelope.Fact{
		SubjectID: subjectID,
		Key:       key,
		Label:     label,
		Value:     value,
		Unit:      unit,
		Source:    envelope.FactSourceAPI,
	})
}

func addShareBandwidthComputed(env *envelope.Envelope, subjectID, key, label, value string) {
	env.Computed = append(env.Computed, envelope.Fact{
		SubjectID: subjectID,
		Key:       key,
		Label:     label,
		Value:     value,
		Source:    envelope.FactSourceComputed,
	})
}

func shareBandwidthSwitchLabel(eip ShareBandwidthEIP) string {
	if !eip.CanSwitch || eip.TargetScope == "" {
		return ""
	}
	if strings.EqualFold(eip.TargetScope, "Company") {
		return "账号当前已有可用的公司独享共享带宽池，可将该 EIP 切换过去"
	}
	return "可切换到当前已有的" + shareBandwidthScopeLabel(eip.TargetScope)
}

func shareBandwidthScopeLabel(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "public":
		return "平台公共共享带宽"
	case "company":
		return "公司独享共享带宽"
	case "withoutgpu":
		return "无卡实例共享带宽"
	default:
		if strings.TrimSpace(scope) == "" {
			return "共享带宽归属未知"
		}
		return "共享带宽归属 " + scope
	}
}

func shareBandwidthUnit(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "Public") {
		return "Gbps"
	}
	return "Mbps"
}
