package readprojection

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
)

type ResourceEnvelopeMeta struct {
	FilterApplied string
	MatchedCount  int
	TotalCount    int
	// Shown is the number of instances actually included in the envelope's
	// Subjects after display-side truncation. 0 means "not truncated /
	// not applicable". When Shown > 0 and Shown < TotalCount the
	// envelope advertises a truncated view.
	Shown     int
	Truncated bool
}

func BuildResourceEnvelope(instances []entity.InstanceSnapshot) envelope.Envelope {
	return BuildResourceEnvelopeWithMetaAndZoneCatalog(instances, ResourceEnvelopeMeta{}, nil)
}

func BuildResourceEnvelopeWithMeta(instances []entity.InstanceSnapshot, meta ResourceEnvelopeMeta) envelope.Envelope {
	return BuildResourceEnvelopeWithMetaAndZoneCatalog(instances, meta, nil)
}

// BuildResourceEnvelopeWithMetaAndZoneCatalog projects the instance's raw zone
// code and, when the live catalog carries it, the console display name as two
// independently labelled facts. The code always comes from the instance
// Describe response; the name only comes from the exact catalog row. No local
// alias or code-to-name map is involved, and an unavailable or missing catalog
// row simply omits the name rather than guessing.
func BuildResourceEnvelopeWithMetaAndZoneCatalog(instances []entity.InstanceSnapshot, meta ResourceEnvelopeMeta, zoneCatalog *deployment.ZoneCatalogSnapshot) envelope.Envelope {
	copied := append([]entity.InstanceSnapshot(nil), instances...)
	env := envelope.Envelope{
		Kind:          envelope.KindResourceInfo,
		SourceActions: []string{"DescribeCompShareInstance"},
		Subjects:      make([]envelope.Subject, 0, len(copied)),
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints: envelope.Constraints{
			DoNotInventInstances:   true,
			DoNotInventZoneLabels:  true,
			DoNotAnswerAccountBill: true,
		},
	}
	usedZoneCatalog := false
	for _, inst := range copied {
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID:   safeValue(inst.UHostId),
			Name: safeValue(inst.Name),
			Type: envelope.SubjectInstance,
		})
		addInstanceFact := func(key, label string, value any) {
			env.Facts = append(env.Facts, envelope.Fact{
				SubjectID: safeValue(inst.UHostId),
				Key:       key,
				Label:     label,
				Value:     safeValue(value),
				Source:    envelope.FactSourceAPI,
			})
		}
		addInstanceFact("uhost_id", resourceLabelInstanceID, inst.UHostId)
		addStringFact(addInstanceFact, "name", resourceLabelName, inst.Name)
		addStringFact(addInstanceFact, "state", resourceLabelState, inst.State)
		addStringFact(addInstanceFact, "os_type", "OsType", inst.OsType)
		addInstanceFact("gpu_count", resourceLabelGPU, inst.GPU)
		if inst.GPU == 0 {
			addInstanceFact("gpu_mode", "GPU运行模式", "无卡")
		} else {
			addStringFact(addInstanceFact, "gpu_type", resourceLabelGPUType, inst.GpuType)
		}
		addStringFact(addInstanceFact, "image_name", resourceLabelImageName, inst.ImageName)
		addStringFact(addInstanceFact, "image_type", resourceLabelImageType, inst.ImageType)
		addStringFact(addInstanceFact, "instance_type", resourceLabelInstanceType, inst.InstanceType)
		addPositiveTimeFact(addInstanceFact, "start_time", resourceLabelStartTime, inst.StartTime)
		addPositiveTimeFact(addInstanceFact, "scheduler_stop_time", resourceLabelSchedulerStopTime, inst.SchedulerStopTime)
		addPositiveTimeFact(addInstanceFact, "stop_time", resourceLabelStopTime, inst.StopTime)
		addPositiveTimeFact(addInstanceFact, "release_time", resourceLabelReleaseTime, inst.ReleaseTime)
		addPositiveIntFact(addInstanceFact, "cpu", resourceLabelCPU, inst.CPU)
		addPositiveIntFact(addInstanceFact, "memory", resourceLabelMemory, inst.Memory)
		addStringFact(addInstanceFact, "zone", "可用区代码", inst.Zone)
		if displayName, ok := resourceZoneDisplayName(zoneCatalog, inst.Zone); ok {
			addStringFact(addInstanceFact, "zone_display_name", "可用区名称", displayName)
			usedZoneCatalog = true
		}
		addStringFact(addInstanceFact, "region", "Region", inst.Region)
		addStringFact(addInstanceFact, "charge_type", "ChargeType", inst.ChargeType)
		// Emit both values so positive and negative answers are equally grounded;
		// ChargeType does not encode whether an existing instance is spot.
		addInstanceFact("is_spot", "是否抢占式", spotFactValue(inst.IsSpot))
		addPositiveTimeFact(addInstanceFact, "expire_time", resourceLabelExpireTime, inst.ExpireTime)
		addStringFact(addInstanceFact, "auto_renew", "AutoRenew", inst.AutoRenew)
		addStringFact(addInstanceFact, "cfs_id", resourceLabelCFSID, inst.CfsID)
		if progress := inst.MigrationProgress; progress.Present {
			addStringFact(addInstanceFact, "migration_id", "迁移任务ID", progress.MigrationID)
			addStringFact(addInstanceFact, "migration_state", "系统盘迁移状态", progress.State)
			addStringFact(addInstanceFact, "migration_reason", "系统盘迁移说明", progress.Reason)
			addStringFact(addInstanceFact, "migration_current", "已迁移量", progress.Current)
			addStringFact(addInstanceFact, "migration_total", "迁移总量", progress.Total)
			addStringFact(addInstanceFact, "migration_speed", "迁移速度", progress.Speed)
			addInstanceFact("migration_eta_seconds", "预计剩余秒数", progress.ETASeconds)
			addInstanceFact("migration_percent", "迁移进度百分比", progress.Percent)
		}
	}
	if usedZoneCatalog {
		env.SourceActions = append(env.SourceActions, "DescribeCompShareSupportZone")
	}
	addComputedResourceMeta(&env, meta)
	return env
}

func addComputedResourceMeta(env *envelope.Envelope, meta ResourceEnvelopeMeta) {
	addComputed := func(key, label, value string) {
		if value == "" {
			return
		}
		env.Computed = append(env.Computed, envelope.Fact{
			Key:    key,
			Label:  label,
			Value:  value,
			Source: envelope.FactSourceComputed,
		})
	}
	addComputed("filter_applied", "Filter applied", meta.FilterApplied)
	if meta.TotalCount > 0 {
		addComputed("total_count", "Total count", strconv.Itoa(meta.TotalCount))
	}
	if meta.FilterApplied != "" {
		addComputed("matched_count", "Matched count", strconv.Itoa(meta.MatchedCount))
	}
	if meta.Truncated {
		if meta.Shown > 0 {
			addComputed("shown_count", "Shown count", strconv.Itoa(meta.Shown))
		}
		addComputed("truncated", "Truncated", "true")
		// This is a fixed display cap, not pagination. Supply the exact constraint
		// text using the filtered or total denominator.
		denominator := meta.TotalCount
		if meta.FilterApplied != "" && meta.MatchedCount > 0 {
			denominator = meta.MatchedCount
		}
		if denominator > 0 && meta.Shown > 0 && denominator > meta.Shown {
			notice := fmt.Sprintf("（已显示 %d/%d 台，完整列表请到控制台查看）", meta.Shown, denominator)
			addComputed("truncation_notice", "Truncation notice", notice)
		}
	}
}

func BuildMonitorEnvelope(subjects []entity.InstanceSnapshot, metrics []Metric, payload map[string]any) envelope.Envelope {
	copied := append([]entity.InstanceSnapshot(nil), subjects...)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].UHostId < copied[j].UHostId
	})

	env := envelope.Envelope{
		Kind:          envelope.KindMonitorQuery,
		SourceActions: []string{"GetCompShareInstanceMonitor"},
		Subjects:      make([]envelope.Subject, 0, len(copied)),
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints: envelope.Constraints{
			DoNotInventInstances:   true,
			DoNotInventMetrics:     true,
			DoNotAnswerAccountBill: true,
		},
	}
	for _, inst := range copied {
		env.Subjects = append(env.Subjects, envelope.Subject{
			ID:   safeValue(inst.UHostId),
			Name: safeValue(inst.Name),
			Type: envelope.SubjectInstance,
		})
	}

	flat := monitorScalarFacts(metrics, payload)
	subjectID := ""
	if len(copied) == 1 {
		subjectID = copied[0].UHostId
	}
	for _, item := range flat {
		itemSubjectID := item.SubjectID
		if itemSubjectID == "" {
			itemSubjectID = subjectID
		}
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID:   itemSubjectID,
			Key:         item.Key,
			Label:       item.Label,
			Value:       item.Value,
			Unit:        item.Unit,
			Source:      envelope.FactSourceAPI,
			Period:      "latest",
			Aggregation: "latest",
		})
	}
	addMissingRequestedMonitorFacts(&env, metrics, flat, subjectID)
	return env
}

// BuildHistoricalMonitorEnvelope is the historical variant of BuildMonitorEnvelope:
// identical facts, but every metric fact is marked as an aggregate over the queried
// window (Period="range" + WindowStart/WindowEnd) so the observation carries the
// exact historical time window as structured evidence, instead of the engine
// correcting the model's prose after the fact.
func BuildHistoricalMonitorEnvelope(subjects []entity.InstanceSnapshot, metrics []Metric, payload map[string]any, windowStart, windowEnd int64) envelope.Envelope {
	env := BuildMonitorEnvelope(subjects, metrics, payload)
	for i := range env.Facts {
		env.Facts[i].Period = "range"
		env.Facts[i].WindowStart = windowStart
		env.Facts[i].WindowEnd = windowEnd
	}
	return env
}

type monitorScalarFact struct {
	SubjectID string
	Key       string
	Label     string
	Value     string
	Unit      string
}

func monitorScalarFacts(metrics []Metric, payload map[string]any) []monitorScalarFact {
	if facts, ok := monitorSemanticFacts(metrics, payload); ok {
		return facts
	}
	redacted := safeValueMap(payload)
	flat := map[string]string{}
	flattenScalars("", redacted, flat)
	keys := make([]string, 0, len(flat))
	for key := range flat {
		if len(metrics) == 0 || matchesRequestedMetric(key, metrics) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]monitorScalarFact, 0, len(keys))
	for _, key := range keys {
		out = append(out, monitorScalarFact{Key: key, Label: key, Value: flat[key]})
	}
	return out
}

type monitorMetricDefinition struct {
	Metric Metric
	Key    string
	Label  string
	Unit   string
}

var monitorMetricDefinitions = map[string]monitorMetricDefinition{
	"uhost_cpu_used":                {Metric: MetricCPU, Key: "cpu_usage", Label: "CPU 使用率", Unit: "%"},
	"cloudwatch_memory_usage":       {Metric: MetricMemory, Key: "memory_usage", Label: "内存使用率", Unit: "%"},
	"cloudwatch_gpu_util":           {Metric: MetricGPU, Key: "gpu_usage", Label: "GPU 使用率", Unit: "%"},
	"cloudwatch_gpu_memory_usage":   {Metric: MetricVRAM, Key: "vram_usage", Label: "显存使用率", Unit: "%"},
	"cloudwatch_sys_disk_used_per":  {Metric: "", Key: "system_disk_usage", Label: "系统盘使用率", Unit: "%"},
	"cloudwatch_data_disk_used_per": {Metric: "", Key: "data_disk_usage", Label: "数据盘使用率", Unit: "%"},
}

var podMonitorMetricDefinitions = map[string]monitorMetricDefinition{
	"Cpu":         {Metric: MetricCPU, Key: "cpu_usage", Label: "CPU 使用率", Unit: "%"},
	"Memory":      {Metric: MetricMemory, Key: "memory_usage", Label: "内存使用率", Unit: "%"},
	"SysDiskUsed": {Metric: "", Key: "system_disk_usage", Label: "系统盘使用率", Unit: "%"},
	"GpuUtil":     {Metric: MetricGPU, Key: "gpu_usage", Label: "GPU 使用率", Unit: "%"},
	"GpuMemory":   {Metric: MetricVRAM, Key: "vram_usage", Label: "显存使用率", Unit: "%"},
}

func monitorSemanticFacts(metrics []Metric, payload map[string]any) ([]monitorScalarFact, bool) {
	data, _ := payload["Data"].(map[string]any)
	if data == nil {
		return nil, false
	}
	list, _ := data["List"].([]any)
	podList, _ := data["PodList"].([]any)
	if len(list) == 0 && len(podList) == 0 {
		return nil, false
	}
	recognized := false
	var facts []monitorScalarFact
	for _, item := range list {
		instance, _ := item.(map[string]any)
		if instance == nil {
			continue
		}
		subjectID, _ := instance["UHostId"].(string)
		metricItems, _ := instance["Metrics"].([]any)
		for _, metricItem := range metricItems {
			metric, _ := metricItem.(map[string]any)
			if metric == nil {
				continue
			}
			metricKey, _ := metric["MetricKey"].(string)
			def, ok := monitorMetricDefinitions[metricKey]
			if !ok || !monitorMetricRequested(def.Metric, metrics) {
				continue
			}
			recognized = true
			results := monitorMetricResults(metric)
			for i, result := range results {
				value, ok := latestMonitorValue(result)
				if !ok {
					continue
				}
				key, label := def.Key, def.Label
				if suffix := monitorResultLabelSuffix(metricKey, i, len(results)); suffix != "" {
					key += "." + suffix
					label += " (" + suffix + ")"
				}
				facts = append(facts, monitorScalarFact{
					SubjectID: subjectID,
					Key:       key,
					Label:     label,
					Value:     value,
					Unit:      def.Unit,
				})
			}
		}
	}
	for _, item := range podList {
		instance, _ := item.(map[string]any)
		if instance == nil {
			continue
		}
		subjectID, _ := instance["UHostId"].(string)
		metricItems, _ := instance["Metrics"].(map[string]any)
		if metricItems == nil {
			continue
		}

		for _, field := range []string{"Cpu", "Memory", "SysDiskUsed"} {
			values, exists := metricItems[field]
			if !exists {
				continue
			}
			recognized = true
			def := podMonitorMetricDefinitions[field]
			if !monitorMetricRequested(def.Metric, metrics) {
				continue
			}
			value, ok := latestPodMonitorValue(values)
			if !ok {
				continue
			}
			facts = append(facts, monitorScalarFact{
				SubjectID: subjectID,
				Key:       def.Key,
				Label:     def.Label,
				Value:     value,
				Unit:      def.Unit,
			})
		}

		gpuItems, _ := metricItems["Gpu"].([]any)
		if _, exists := metricItems["Gpu"]; exists {
			recognized = true
		}
		for i, item := range gpuItems {
			gpu, _ := item.(map[string]any)
			if gpu == nil {
				continue
			}
			addPodGPUFact := func(field, defKey string) {
				values, exists := gpu[field]
				if !exists {
					return
				}
				recognized = true
				def := podMonitorMetricDefinitions[defKey]
				if !monitorMetricRequested(def.Metric, metrics) {
					return
				}
				value, ok := latestPodMonitorValue(values)
				if !ok {
					return
				}
				key, label := def.Key, def.Label
				if suffix := podMonitorResultLabelSuffix(i, len(gpuItems)); suffix != "" {
					key += "." + suffix
					label += " (" + suffix + ")"
				}
				facts = append(facts, monitorScalarFact{
					SubjectID: subjectID,
					Key:       key,
					Label:     label,
					Value:     value,
					Unit:      def.Unit,
				})
			}
			addPodGPUFact("Util", "GpuUtil")
			addPodGPUFact("Memory", "GpuMemory")
		}
	}
	if !recognized {
		return nil, false
	}
	sort.SliceStable(facts, func(i, j int) bool {
		if facts[i].SubjectID != facts[j].SubjectID {
			return facts[i].SubjectID < facts[j].SubjectID
		}
		if facts[i].Key != facts[j].Key {
			return facts[i].Key < facts[j].Key
		}
		return facts[i].Value < facts[j].Value
	})
	return facts, true
}

// MonitorScalar is a per-metric, per-instance scalar value extracted from
// a GetCompShareInstanceMonitor result using renderer vocabulary (cpu_usage,
// gpu_usage, vram_usage etc.) rather than raw API keys.
//
// Multi-GPU disambiguation: a host with N GPUs produces N MonitorScalar
// entries with Key in the form "gpu_usage.GPU 1", "gpu_usage.GPU 2", ....
type MonitorScalar struct {
	SubjectID string
	Key       string
	Value     string
	Unit      string
}

// ExtractMonitorScalars walks a GetCompShareInstanceMonitor raw result and
// returns the per-(host, metric) latest scalar values, using the same
// display vocabulary that read capabilities emit.
// Returns nil if the payload is unrecognized or contains no
// known-metric data.
//
// metrics may be empty, in which case all known metric keys are accepted.
// Pass through whatever metric set the caller needs for the current turn.
func ExtractMonitorScalars(payload map[string]any, metrics []Metric) []MonitorScalar {
	semantic, ok := monitorSemanticFacts(metrics, payload)
	if !ok {
		return nil
	}
	out := make([]MonitorScalar, 0, len(semantic))
	for _, f := range semantic {
		out = append(out, MonitorScalar{
			SubjectID: f.SubjectID,
			Key:       f.Key,
			Value:     f.Value,
			Unit:      f.Unit,
		})
	}
	return out
}

// ExtractMonitorScalarsForSubject extracts only the requested instance and also
// reports whether the response shape can truthfully support an empty result for
// that instance. A successful API call is not enough: a missing Data block,
// schema drift, or a non-empty list containing only other instance IDs must not
// be presented as "the platform returned no metrics for this instance".
func ExtractMonitorScalarsForSubject(payload map[string]any, metrics []Metric, subjectID string) ([]MonitorScalar, bool) {
	if subjectID == "" {
		return nil, false
	}
	all := ExtractMonitorScalars(payload, metrics)
	out := make([]MonitorScalar, 0, len(all))
	for _, scalar := range all {
		if scalar.SubjectID == subjectID {
			out = append(out, scalar)
		}
	}
	if len(out) > 0 {
		return out, true
	}
	return nil, monitorSubjectEmptyResponseRecognized(payload, subjectID)
}

func monitorSubjectEmptyResponseRecognized(payload map[string]any, subjectID string) bool {
	data, ok := payload["Data"].(map[string]any)
	if !ok {
		return false
	}
	branchSeen := false
	nonEmptyBranchSeen := false

	if raw, present := data["List"]; present {
		items, valid := raw.([]any)
		if !valid {
			return false
		}
		branchSeen = true
		nonEmptyBranchSeen = nonEmptyBranchSeen || len(items) > 0
		for _, rawItem := range items {
			item, _ := rawItem.(map[string]any)
			itemID, _ := item["UHostId"].(string)
			if item == nil || strings.TrimSpace(itemID) != subjectID {
				continue
			}
			rawMetrics, present := item["Metrics"]
			metricItems, valid := rawMetrics.([]any)
			if !present || !valid {
				return false
			}
			if len(metricItems) == 0 {
				return true
			}
			for _, rawMetric := range metricItems {
				metric, _ := rawMetric.(map[string]any)
				if metric == nil {
					continue
				}
				metricKey, _ := metric["MetricKey"].(string)
				if _, recognized := monitorMetricDefinitions[metricKey]; recognized && monitorResultsShapeRecognized(metric["Results"]) {
					return true
				}
			}
			return false
		}
	}

	if raw, present := data["PodList"]; present {
		items, valid := raw.([]any)
		if !valid {
			return false
		}
		branchSeen = true
		nonEmptyBranchSeen = nonEmptyBranchSeen || len(items) > 0
		for _, rawItem := range items {
			item, _ := rawItem.(map[string]any)
			itemID, _ := item["UHostId"].(string)
			if item == nil || strings.TrimSpace(itemID) != subjectID {
				continue
			}
			rawMetrics, present := item["Metrics"]
			metricItems, valid := rawMetrics.(map[string]any)
			if !present || !valid {
				return false
			}
			if len(metricItems) == 0 {
				return true
			}
			for key, values := range metricItems {
				if key == "Gpu" && podGPUShapeRecognized(values) {
					return true
				}
				if _, recognized := podMonitorMetricDefinitions[key]; recognized && monitorValueSeriesShapeRecognized(values) {
					return true
				}
			}
			return false
		}
	}

	// An explicitly present, typed, empty result branch is the upstream's
	// successful-empty representation for a query scoped to this subject. Once
	// any record is present, however, absence of the requested ID is ambiguous.
	return branchSeen && !nonEmptyBranchSeen
}

func monitorResultsShapeRecognized(raw any) bool {
	results, ok := raw.([]any)
	if !ok {
		return false
	}
	if len(results) == 0 {
		return true
	}
	for _, rawResult := range results {
		result, _ := rawResult.(map[string]any)
		if result != nil && monitorValueSeriesShapeRecognized(result["Values"]) {
			return true
		}
	}
	return false
}

func monitorValueSeriesShapeRecognized(raw any) bool {
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	if len(values) == 0 {
		return true
	}
	point, _ := values[len(values)-1].(map[string]any)
	if point == nil {
		return false
	}
	_, valid := monitorNumberString(point["Value"])
	return valid
}

func podGPUShapeRecognized(raw any) bool {
	gpus, ok := raw.([]any)
	if !ok {
		return false
	}
	if len(gpus) == 0 {
		return true
	}
	for _, rawGPU := range gpus {
		gpu, _ := rawGPU.(map[string]any)
		if gpu == nil {
			continue
		}
		for _, key := range []string{"Util", "Memory"} {
			if values, present := gpu[key]; present && monitorValueSeriesShapeRecognized(values) {
				return true
			}
		}
	}
	return false
}

func monitorMetricRequested(metric Metric, requested []Metric) bool {
	if len(requested) == 0 {
		return true
	}
	if metric == "" {
		return false
	}
	for _, value := range requested {
		if value == metric {
			return true
		}
	}
	return false
}

func addMissingRequestedMonitorFacts(env *envelope.Envelope, metrics []Metric, facts []monitorScalarFact, subjectID string) {
	if env == nil || len(metrics) == 0 {
		return
	}
	present := presentMonitorMetrics(facts)
	for _, metric := range uniqueMonitorMetrics(metrics) {
		if present[metric] {
			continue
		}
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID:   subjectID,
			Key:         "missing_" + string(metric) + "_usage",
			Label:       monitorMetricReplyLabel(metric),
			Value:       "未返回数据",
			Source:      envelope.FactSourceComputed,
			Period:      "latest",
			Aggregation: "latest",
		})
	}
}

func monitorMetricResults(metric map[string]any) []map[string]any {
	items, _ := metric["Results"].([]any)
	results := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result, _ := item.(map[string]any)
		if result != nil {
			results = append(results, result)
		}
	}
	return results
}

func latestMonitorValue(result map[string]any) (string, bool) {
	items, _ := result["Values"].([]any)
	if len(items) == 0 {
		return "", false
	}
	valuePoint, _ := items[len(items)-1].(map[string]any)
	if valuePoint == nil {
		return "", false
	}
	return monitorNumberString(valuePoint["Value"])
}

func latestPodMonitorValue(values any) (string, bool) {
	items, _ := values.([]any)
	if len(items) == 0 {
		return "", false
	}
	valuePoint, _ := items[len(items)-1].(map[string]any)
	if valuePoint == nil {
		return "", false
	}
	return monitorNumberString(valuePoint["Value"])
}

func monitorNumberString(value any) (string, bool) {
	switch typed := value.(type) {
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32), true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case string:
		if typed == "" {
			return "", false
		}
		if _, err := strconv.ParseFloat(typed, 64); err != nil {
			return "", false
		}
		return typed, true
	default:
		return "", false
	}
}

func monitorResultLabelSuffix(metricKey string, index, total int) string {
	if total <= 1 {
		return ""
	}
	switch metricKey {
	case "cloudwatch_gpu_util", "cloudwatch_gpu_memory_usage":
		return fmt.Sprintf("GPU %d", index+1)
	case "cloudwatch_data_disk_used_per":
		return fmt.Sprintf("Disk %d", index+1)
	default:
		return fmt.Sprintf("%d", index+1)
	}
}

func podMonitorResultLabelSuffix(index, total int) string {
	if total <= 1 {
		return ""
	}
	return fmt.Sprintf("GPU %d", index+1)
}

func addStringFact(add func(string, string, any), key, label, value string) {
	if value != "" {
		add(key, label, value)
	}
}

func addPositiveIntFact(add func(string, string, any), key, label string, value int) {
	if value > 0 {
		add(key, label, value)
	}
}

// addPositiveTimeFact emits the same localized text used by the renderer so the
// model never has to convert an epoch or create a contradictory date.
func addPositiveTimeFact(add func(string, string, any), key, label string, value int64) {
	if value > 0 {
		add(key, label, resourceTimeLabel(value))
	}
}
