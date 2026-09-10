package workflow

import (
	"fmt"
	"strings"
)

// After the write, the instance is read back and compared with what was
// sealed. A mismatch, an initialization failure or a readback that does not
// cover every returned id is reported as itself — never narrated as a
// successful delivery, because the user's next move after a create they
// believe failed is to create it again.

func stepDescribeInstance() Step {
	return Step{
		Name:     "查看状态",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareInstance",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			createResult := wfCtx.Result("创建实例")
			ids, ok := createResult["UHostIds"].([]any)
			if !ok || len(ids) == 0 {
				return nil, fmt.Errorf("创建实例未返回 UHostIds")
			}
			return map[string]any{
				"UHostIds": ids,
			}, nil
		},
	}
}

// createInstanceResultData keeps two claims separate: Intended comes from the
// sealed contract the user confirmed; Observed comes from the optional readback
// of the exact ids returned by create. ActualReadbackAvailable reports that every
// returned id had a matching readback row. It does not redefine create success or
// claim that every actual field was present.
func createInstanceResultData(wfCtx *Context) map[string]any {
	createResult := wfCtx.Result("创建实例")
	if createResult == nil {
		return nil
	}
	ids, ok := createResult["UHostIds"]
	if !ok {
		return nil
	}
	data := map[string]any{"UHostIds": ids}
	if snapshot, err := sealedCreateConfirmation(wfCtx); err == nil {
		if expected := expectedCreateDataDisks(snapshot.Execution.Args.Disks); len(expected) > 0 {
			state := "pending"
			if createDataDisksObserved(wfCtx.Result("查看状态"), ids, expected) {
				state = "verified"
			}
			data["DataDiskDelivery"] = map[string]any{
				"State":     state,
				"Requested": expected,
			}
		}
	}
	observed := createObservedInstances(wfCtx, ids)
	data["ActualReadbackAvailable"] = createReadbackCoversEveryReturnedInstance(ids, observed)
	if len(observed) == 0 {
		return data
	}
	data["Observed"] = observed
	intended, hasIntent := createIntendedSpec(wfCtx)
	if !hasIntent {
		return data
	}
	data["Intended"] = intended
	if mismatches := createSpecMismatches(intended, observed); len(mismatches) > 0 {
		data["SpecMismatch"] = mismatches
	}
	return data
}

// createObservedInstances projects only rows for ids returned by this create.
func createObservedInstances(wfCtx *Context, ids any) []map[string]any {
	describe := wfCtx.Result("查看状态")
	if describe == nil {
		return nil
	}
	rows, _ := describe["UHostSet"].([]any)
	if len(rows) == 0 {
		return nil
	}
	wanted := createReturnedInstanceIDs(ids)
	if len(wanted) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(wanted))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row == nil {
			continue
		}
		// Projected here rather than through entity.InstanceFromMap: internal/entity's
		// own tests import this package, so depending on it the other way round is an
		// import cycle. These are the eight fields a created instance is judged by.
		id := strings.TrimSpace(paramStr(row, "UHostId", ""))
		if _, want := wanted[strings.ToLower(id)]; !want {
			continue
		}
		out = append(out, map[string]any{
			"UHostId": id,
			"Name":    paramStr(row, "Name", ""),
			"State":   paramStr(row, "State", ""),
			"CPU":     int(paramNum(row, "CPU", 0)),
			"Memory":  int(paramNum(row, "Memory", 0)),
			"GPU":     int(paramNum(row, "GPU", 0)),
			"GpuType": paramStr(row, "GpuType", ""),
			"Zone":    paramStr(row, "Zone", ""),
		})
	}
	return out
}

func createReturnedInstanceIDs(ids any) map[string]struct{} {
	wanted := map[string]struct{}{}
	switch list := ids.(type) {
	case []string:
		for _, id := range list {
			if id = strings.TrimSpace(id); id != "" {
				wanted[strings.ToLower(id)] = struct{}{}
			}
		}
	case []any:
		for _, id := range list {
			if s, _ := id.(string); strings.TrimSpace(s) != "" {
				wanted[strings.ToLower(strings.TrimSpace(s))] = struct{}{}
			}
		}
	}
	return wanted
}

func createReadbackCoversEveryReturnedInstance(ids any, observed []map[string]any) bool {
	missing := createReturnedInstanceIDs(ids)
	if len(missing) == 0 {
		return false
	}
	for _, row := range observed {
		delete(missing, strings.ToLower(strings.TrimSpace(paramStr(row, "UHostId", ""))))
	}
	return len(missing) == 0
}

// createIntendedSpec reads the confirmed sealed contract, never mutable Params.
func createIntendedSpec(wfCtx *Context) (map[string]any, bool) {
	snapshot, err := sealedCreateConfirmation(wfCtx)
	if err != nil {
		return nil, false
	}
	args := snapshot.Execution.Args
	return map[string]any{
		"CPU":     int(args.CPU),
		"Memory":  int(args.Memory),
		"GPU":     int(args.GPU),
		"GpuType": args.GpuType,
		"Zone":    args.Zone,
	}, true
}

// createSpecMismatches compares fields that are present on both sides. An unset
// confirmed value delegates that field to the platform; an unset observed value
// is unknown rather than evidence of a mismatch.
func createSpecMismatches(intended map[string]any, observed []map[string]any) []map[string]any {
	var out []map[string]any
	for _, inst := range observed {
		for _, field := range []string{"CPU", "Memory", "GPU", "GpuType", "Zone"} {
			want, got := intended[field], inst[field]
			if createSpecValueUnset(want) || createSpecValueUnset(got) {
				continue
			}
			if fmt.Sprint(want) == fmt.Sprint(got) {
				continue
			}
			out = append(out, map[string]any{
				"UHostId": inst["UHostId"], "Field": field, "Intended": want, "Observed": got,
			})
		}
	}
	return out
}

func createSpecValueUnset(v any) bool {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value) == ""
	case int:
		return value == 0
	case float64:
		return value == 0
	case nil:
		return true
	}
	return false
}

func expectedCreateDataDisks(disks []any) []map[string]any {
	var out []map[string]any
	for _, raw := range disks {
		disk, _ := raw.(map[string]any)
		if disk == nil || paramBool(disk, "IsBoot", false) {
			continue
		}
		size, ok := createDiskSizeGB(disk["Size"])
		if !ok || size <= 0 {
			continue
		}
		out = append(out, map[string]any{
			"SizeGB": size,
			"Type":   strings.TrimSpace(paramStr(disk, "Type", "")),
		})
	}
	return out
}

func createDataDisksObserved(describe map[string]any, ids any, expected []map[string]any) bool {
	if len(expected) == 0 || describe == nil {
		return false
	}
	wanted := createReturnedInstanceIDs(ids)
	if len(wanted) == 0 {
		return false
	}
	matched := map[string]struct{}{}
	rows, _ := describe["UHostSet"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row == nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(paramStr(row, "UHostId", "")))
		if _, ok := wanted[id]; !ok {
			continue
		}
		if createDataDiskSetObserved(row, expected) {
			matched[id] = struct{}{}
		}
	}
	return len(matched) == len(wanted)
}

func createDataDiskSetObserved(row map[string]any, expected []map[string]any) bool {
	remaining := append([]map[string]any(nil), expected...)
	rawDisks, _ := row["DiskSet"].([]any)
	for _, rawDisk := range rawDisks {
		disk, _ := rawDisk.(map[string]any)
		if disk == nil {
			continue
		}
		if isBootDisk(disk) {
			continue
		}
		actual := diskNumber(disk, "Size", "DiskSize", "Capacity")
		actualType := strings.TrimSpace(paramStr(disk, "DiskType", ""))
		if actualType == "" {
			actualType = strings.TrimSpace(paramStr(disk, "Type", ""))
		}
		for i, want := range remaining {
			size, _ := priceNumber(want["SizeGB"])
			wantType := strings.TrimSpace(paramStr(want, "Type", ""))
			if actual == size && wantType != "" && strings.EqualFold(actualType, wantType) {
				remaining = append(remaining[:i], remaining[i+1:]...)
				break
			}
		}
	}
	return len(remaining) == 0
}
