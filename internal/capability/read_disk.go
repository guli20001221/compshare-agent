package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/readprojection"
)

const (
	diskInfoAction   = "DescribeCompshareDisk"
	maxRenderedDisks = 20
)

// DiskInfoRequest lists account disks, optionally scoped to one instance or
// exact disk resource IDs. Targets use the same resolver contract as other
// instance-scoped reads; disk IDs are matched against the API response's
// ResourceId so the capability works for both UDisk and CVolume rows.
type DiskInfoRequest struct {
	Targets []platform.TargetRef `json:"targets,omitempty"`
	DiskIDs []string             `json:"disk_ids,omitempty"`
}

type diskInfo struct {
	Name          string
	ResourceID    string
	Configuration string
	DiskType      string
	Zone          string
	MountInstance string
	MountPoint    string
	Status        string
	ChargeType    string
	Source        string
	ExpiredTime   int64
}

type DiskInfoResponse struct {
	Disks      []diskInfo
	TotalCount int
}

func diskInfoHandle(ctx context.Context, req DiskInfoRequest, rt ReadRuntime) (DiskInfoResponse, ReadResult) {
	_, hostIDs, reason := resolveReadTargetSnapshots(ctx, req.Targets, rt)
	if reason != nil {
		return DiskInfoResponse{}, readTargetFallbackResult(*reason)
	}
	if len(hostIDs) > 1 {
		return DiskInfoResponse{}, ReadConflict("磁盘查询一次只能按一台实例筛选，请指定其中一台实例。")
	}

	args := map[string]any{}
	if len(hostIDs) == 1 {
		args["HostId"] = hostIDs[0]
	}
	raw, err := rt.Executor.Execute(ctx, diskInfoAction, args)
	if err != nil {
		return DiskInfoResponse{}, ReadFailureAfterTool(diskInfoAction, resourceCapabilityLabel, err)
	}

	disks := diskInfoRows(raw)
	requested := normalizedDiskIDs(req.DiskIDs)
	if len(requested) > 0 {
		disks = filterDiskInfoByID(disks, requested)
	}
	if len(disks) == 0 {
		r := ReadEmpty("未查询到符合条件的磁盘。")
		r.ToolAction = diskInfoAction
		return DiskInfoResponse{}, r
	}
	return DiskInfoResponse{Disks: disks, TotalCount: len(disks)}, ReadResult{}
}

func diskInfoRows(raw map[string]any) []diskInfo {
	rows := mapSliceAt(raw, "DiskSet")
	out := make([]diskInfo, 0, len(rows))
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		resourceID := strings.TrimSpace(stringField(row, "ResourceId"))
		if resourceID == "" {
			continue
		}
		out = append(out, diskInfo{
			Name:          stringField(row, "Name"),
			ResourceID:    resourceID,
			Configuration: stringField(row, "Configuration"),
			DiskType:      stringField(row, "DiskType"),
			Zone:          stringField(row, "Zone"),
			MountInstance: stringField(row, "MountInstance"),
			MountPoint:    stringField(row, "MountPoint"),
			Status:        stringField(row, "Status"),
			ChargeType:    stringField(row, "ChargeType"),
			Source:        stringField(row, "Source"),
			ExpiredTime:   int64NumericField(row, "ExpiredTime"),
		})
	}
	return out
}

func normalizedDiskIDs(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func filterDiskInfoByID(disks []diskInfo, requested map[string]struct{}) []diskInfo {
	out := make([]diskInfo, 0, len(disks))
	for _, disk := range disks {
		if _, ok := requested[strings.ToLower(strings.TrimSpace(disk.ResourceID))]; ok {
			out = append(out, disk)
		}
	}
	return out
}

func diskInfoRender(resp DiskInfoResponse) ReadResult {
	shown := len(resp.Disks)
	if shown > maxRenderedDisks {
		shown = maxRenderedDisks
	}
	disks := resp.Disks[:shown]
	lines := []string{"磁盘："}
	for _, disk := range disks {
		name := cleanDiskText(disk.Name)
		if name == "" {
			name = cleanDiskText(disk.ResourceID)
		}
		parts := make([]string, 0, 9)
		appendDiskPart := func(label, value string) {
			if value = cleanDiskText(value); value != "" {
				parts = append(parts, label+" "+value)
			}
		}
		appendDiskPart("容量", disk.Configuration)
		appendDiskPart("类型", disk.DiskType)
		appendDiskPart("来源", disk.Source)
		appendDiskPart("状态", disk.Status)
		appendDiskPart("挂载实例", disk.MountInstance)
		appendDiskPart("挂载点", disk.MountPoint)
		appendDiskPart("可用区", disk.Zone)
		appendDiskPart("计费", disk.ChargeType)
		if disk.ExpiredTime > 0 {
			parts = append(parts, "到期 "+readprojection.ResourceTimeLabel(disk.ExpiredTime))
		}
		resourceID := cleanDiskText(disk.ResourceID)
		title := name
		if title != resourceID {
			title += "（" + resourceID + "）"
		}
		lines = append(lines, fmt.Sprintf("- %s：%s", title, strings.Join(parts, "；")))
	}
	if resp.TotalCount > shown {
		lines = append(lines, fmt.Sprintf("（已显示 %d/%d 块，提供实例或磁盘 ID 可精确查询）", shown, resp.TotalCount))
	}

	r := ReadHandled(strings.Join(lines, "\n"))
	r.ToolAction = diskInfoAction
	env := buildDiskInfoEnvelope(disks)
	r.Envelope = &env
	return r
}

func buildDiskInfoEnvelope(disks []diskInfo) envelope.Envelope {
	env := envelope.Envelope{
		Kind:          envelope.KindDiskInfo,
		SourceActions: []string{diskInfoAction},
		Subjects:      make([]envelope.Subject, 0, len(disks)),
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints: envelope.Constraints{
			DoNotInventInstances:   true,
			DoNotAnswerAccountBill: true,
		},
	}
	for _, disk := range disks {
		id := cleanDiskText(disk.ResourceID)
		env.Subjects = append(env.Subjects, envelope.Subject{ID: id, Name: cleanDiskText(disk.Name), Type: envelope.SubjectDisk})
		add := func(key, label string, value any) {
			valueText := cleanDiskText(value)
			if valueText == "" {
				return
			}
			env.Facts = append(env.Facts, envelope.Fact{SubjectID: id, Key: key, Label: label, Value: valueText, Source: envelope.FactSourceAPI})
		}
		add("resource_id", "磁盘资源ID", disk.ResourceID)
		add("name", "名称", disk.Name)
		add("configuration", "容量", disk.Configuration)
		add("disk_type", "类型", disk.DiskType)
		add("source", "来源", disk.Source)
		add("status", "状态", disk.Status)
		add("mount_instance", "挂载实例", disk.MountInstance)
		add("mount_point", "挂载点", disk.MountPoint)
		add("zone", "可用区", disk.Zone)
		add("charge_type", "计费方式", disk.ChargeType)
		if disk.ExpiredTime > 0 {
			add("expired_time", "到期时间", readprojection.ResourceTimeLabel(disk.ExpiredTime))
		}
	}
	return env
}

func cleanDiskText(value any) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(safeValue(value)))
}

func int64NumericField(row map[string]any, key string) int64 {
	value, ok := numericField(row, key)
	if !ok {
		return 0
	}
	return int64(value)
}
