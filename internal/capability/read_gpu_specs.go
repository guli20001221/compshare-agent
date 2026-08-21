package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// GPU-specs reads the live machine-type catalog and renders either an overview
// or full specifications with an evidence envelope.

const (
	gpuSpecsCapabilityLabel = string(intent.IntentGPUSpecsQuery)
	gpuSpecsAction          = "DescribeAvailableCompShareInstanceTypes"

	noGPUSpecsReply = "未获取到 GPU 机型规格数据。"
)

// GPUSpecsRequest is the capability's own request contract.
type GPUSpecsRequest struct {
	GPUType     string               `json:"gpu_type,omitempty"`
	DetailLevel platform.DetailLevel `json:"detail_level,omitempty"`
}

// MissingFields: none — an unfiltered overview is valid.
func (GPUSpecsRequest) MissingFields() []platform.MissingField { return nil }

// GPUSpecsResponse carries the payload and typed rendering controls.
type GPUSpecsResponse struct {
	Raw     map[string]any
	GPUType string
	Detail  platform.DetailLevel
}

func gpuSpecsReadSpec() ReadCapabilitySpec[GPUSpecsRequest, GPUSpecsResponse] {
	return ReadCapabilitySpec[GPUSpecsRequest, GPUSpecsResponse]{
		Label:       gpuSpecsCapabilityLabel,
		Description: "查询平台 GPU 机型的结构化规格，包括显存、算力、最大卡数和可选 CPU/内存组合。用于规格比较，不代表当前实时库存。",
		Params:      objectParam(map[string]schemaNode{"gpu_type": stringParam(), "detail_level": enumParam(platform.DetailLevelValues()...)}),
		Handle:      gpuSpecsHandle,
		Render:      gpuSpecsRender,
	}
}

func gpuSpecsHandle(ctx context.Context, req GPUSpecsRequest, rt ReadRuntime) (GPUSpecsResponse, ReadResult) {
	raw, err := rt.Executor.Execute(ctx, gpuSpecsAction, map[string]any{})
	if err != nil {
		return GPUSpecsResponse{}, ReadFailureAfterTool(gpuSpecsAction, gpuSpecsCapabilityLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	if len(mapSliceAt(raw, "AvailableInstanceTypes")) == 0 {
		// Query succeeded but the catalog is empty — a structured Empty read, not
		// a Handled answer that happens to say "no data".
		return GPUSpecsResponse{}, ReadEmpty(noGPUSpecsReply)
	}
	return GPUSpecsResponse{Raw: raw, GPUType: req.GPUType, Detail: req.DetailLevel}, ReadResult{}
}

func gpuSpecsRender(resp GPUSpecsResponse) ReadResult {
	r := ReadHandled(renderGPUSpecsReply(resp.Raw, resp.GPUType, resp.Detail))
	r.ToolAction = gpuSpecsAction
	env := buildGPUSpecsEnvelope(resp.Raw, resp.GPUType, resp.Detail)
	r.Envelope = &env
	return r
}

func renderGPUSpecsReply(raw map[string]any, gpuType string, detail platform.DetailLevel) string {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	if len(items) == 0 {
		return noGPUSpecsReply
	}
	query := strings.TrimSpace(gpuType)
	matched := matchUserTextToInstanceTypeNames(query, items, true)

	var prefix string
	filterTo := map[string]struct{}{}
	if len(matched) > 0 {
		for _, m := range matched {
			filterTo[m] = struct{}{}
		}
	} else if query != "" {
		prefix = "未在当前可售机型里找到您提到的型号。以下是当前可售机型规格：\n"
	}

	detailed := detail == platform.DetailLevelFull
	lines := buildGPUSpecLines(items, filterTo, detailed)
	if len(lines) == 0 {
		if prefix != "" {
			return strings.TrimRight(prefix, "\n")
		}
		return noGPUSpecsReply
	}
	return prefix + strings.Join(lines, "\n")
}

func buildGPUSpecLines(items []any, filterTo map[string]struct{}, detailed bool) []string {
	lines := make([]string, 0, len(items))
	seenNames := map[string]struct{}{}
	seenDetailed := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		if len(filterTo) > 0 {
			if _, ok := filterTo[name]; !ok {
				continue
			}
		}
		if detailed {
			key := name + "\x00" + safeString(entry, "Zone") + "\x00" + expandMachineSizes(entry)
			if _, ok := seenDetailed[key]; ok {
				continue
			}
			seenDetailed[key] = struct{}{}
		} else {
			// Dedupe by Name for overview replies so a plain spec question stays
			// concise even if the API returns the same model in multiple zones.
			if _, ok := seenNames[name]; ok {
				continue
			}
			seenNames[name] = struct{}{}
		}
		parts := []string{"机型=" + name}
		if detailed {
			if zone := safeString(entry, "Zone"); zone != "" {
				parts = append(parts, "可用区="+zone)
			}
		}
		// Performance + GraphicsMemory are nested {Rate, Value} maps in the API
		// response; we display the Value (the scalar the user actually wants).
		if perf := nestedValue(entry, "Performance"); perf != "" {
			parts = append(parts, "性能="+perf)
		}
		if gmem := nestedValue(entry, "GraphicsMemory"); gmem != "" {
			parts = append(parts, "显存="+gmem+"GB")
		}
		if status := safeString(entry, "Status"); status != "" {
			parts = append(parts, "状态="+status)
		}
		if detailed {
			if sizes := expandMachineSizes(entry); sizes != "" {
				parts = append(parts, "完整配置="+sizes)
			}
		} else if maxGPU := maxGPUFromMachineSizes(entry); maxGPU != "" {
			parts = append(parts, "最大卡数="+maxGPU)
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	return lines
}

func buildGPUSpecsEnvelope(raw map[string]any, gpuType string, detail platform.DetailLevel) envelope.Envelope {
	items := mapSliceAt(raw, "AvailableInstanceTypes")
	matched := matchUserTextToInstanceTypeNames(strings.TrimSpace(gpuType), items, true)
	filterTo := map[string]struct{}{}
	for _, m := range matched {
		filterTo[m] = struct{}{}
	}
	detailed := detail == platform.DetailLevelFull
	entries := selectGPUSpecEntries(items, filterTo, detailed)

	env := envelope.Envelope{
		Kind:          envelope.KindGPUSpecsQuery,
		SourceActions: []string{"DescribeAvailableCompShareInstanceTypes"},
		Subjects:      []envelope.Subject{},
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints: envelope.Constraints{
			DoNotInventInstances:   true,
			DoNotAnswerAccountBill: true,
		},
	}
	answerMode := "overview"
	if detailed {
		answerMode = "full_specs"
	}
	env.Computed = append(env.Computed,
		envelope.Fact{Key: "answer_mode", Label: "Answer mode", Value: answerMode, Source: envelope.FactSourceComputed},
	)

	seenSubjects := map[string]struct{}{}
	for _, entry := range entries {
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		subjectID := "gpu_model:" + name
		if _, ok := seenSubjects[subjectID]; !ok {
			seenSubjects[subjectID] = struct{}{}
			env.Subjects = append(env.Subjects, envelope.Subject{
				ID:   subjectID,
				Name: name,
				Type: envelope.SubjectGPUModel,
			})
		}
		addFact := func(key, label string, value any, unit string) {
			valueString := safeValue(value)
			if strings.TrimSpace(valueString) == "" {
				return
			}
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: subjectID,
				Key:       key,
				Label:     label,
				Value:     valueString,
				Unit:      unit,
				Source:    envelope.FactSourceAPI,
			})
		}
		addFact("model_name", "机型", name, "")
		if detailed {
			addFact("zone", "可用区", safeString(entry, "Zone"), "")
		}
		addFact("performance", "性能", nestedValue(entry, "Performance"), "")
		addFact("graphics_memory", "显存", nestedValue(entry, "GraphicsMemory"), "GB")
		addFact("status", "状态", safeString(entry, "Status"), "")
		if detailed {
			addFact("machine_size_configs", "完整配置", expandMachineSizes(entry), "")
		} else {
			addFact("max_gpu_count", "最大卡数", maxGPUFromMachineSizes(entry), "卡")
		}
	}
	return env
}

func selectGPUSpecEntries(items []any, filterTo map[string]struct{}, detailed bool) []map[string]any {
	entries := make([]map[string]any, 0, len(items))
	seenNames := map[string]struct{}{}
	seenDetailed := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := safeString(entry, "Name")
		if name == "" {
			continue
		}
		if len(filterTo) > 0 {
			if _, ok := filterTo[name]; !ok {
				continue
			}
		}
		if detailed {
			key := name + "\x00" + safeString(entry, "Zone") + "\x00" + expandMachineSizes(entry)
			if _, ok := seenDetailed[key]; ok {
				continue
			}
			seenDetailed[key] = struct{}{}
		} else {
			if _, ok := seenNames[name]; ok {
				continue
			}
			seenNames[name] = struct{}{}
		}
		entries = append(entries, entry)
	}
	return entries
}

func expandMachineSizes(entry map[string]any) string {
	sizes := mapSliceAt(entry, "MachineSizes")
	if len(sizes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sizes))
	seen := map[string]struct{}{}
	for _, s := range sizes {
		size, ok := s.(map[string]any)
		if !ok {
			continue
		}
		gpu := safeNumeric(size, "Gpu")
		collection := mapSliceAt(size, "Collection")
		if len(collection) == 0 {
			appendUniqueMachineSize(&parts, seen, formatMachineSizeSegment(gpu, "", ""))
			continue
		}
		for _, c := range collection {
			combo, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cpu := safeNumeric(combo, "Cpu")
			mems := mapSliceAt(combo, "Memory")
			if len(mems) == 0 {
				appendUniqueMachineSize(&parts, seen, formatMachineSizeSegment(gpu, cpu, ""))
				continue
			}
			for _, mem := range mems {
				appendUniqueMachineSize(&parts, seen, formatMachineSizeSegment(gpu, cpu, fmt.Sprint(mem)))
			}
		}
	}
	return strings.Join(parts, ", ")
}

func maxGPUFromMachineSizes(entry map[string]any) string {
	sizes := mapSliceAt(entry, "MachineSizes")
	if len(sizes) == 0 {
		return ""
	}
	maxLabel := ""
	var maxValue float64
	hasNumeric := false
	for _, s := range sizes {
		size, ok := s.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := size["Gpu"]
		if !ok {
			continue
		}
		label := fmt.Sprint(raw)
		value, numeric := numericValue(raw)
		if numeric {
			if !hasNumeric || value > maxValue {
				maxValue = value
				maxLabel = label
				hasNumeric = true
			}
			continue
		}
		if maxLabel == "" {
			maxLabel = label
		}
	}
	return maxLabel
}

func appendUniqueMachineSize(parts *[]string, seen map[string]struct{}, segment string) {
	segment = strings.Trim(segment, "/")
	if segment == "" {
		return
	}
	if _, ok := seen[segment]; ok {
		return
	}
	seen[segment] = struct{}{}
	*parts = append(*parts, segment)
}

func formatMachineSizeSegment(gpu, cpu, memory string) string {
	parts := []string{}
	if gpu != "" {
		parts = append(parts, gpu+"卡")
	}
	if cpu != "" {
		parts = append(parts, cpu+"C")
	}
	if memory != "" {
		parts = append(parts, memory+"G")
	}
	return strings.Join(parts, "/")
}
