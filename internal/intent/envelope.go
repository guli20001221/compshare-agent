package intent

import (
	"fmt"
	"sort"
	"strconv"

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
	return BuildResourceEnvelopeWithMeta(instances, ResourceEnvelopeMeta{})
}

func BuildResourceEnvelopeWithMeta(instances []entity.InstanceSnapshot, meta ResourceEnvelopeMeta) envelope.Envelope {
	copied := append([]entity.InstanceSnapshot(nil), instances...)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].UHostId < copied[j].UHostId
	})

	env := envelope.Envelope{
		Kind:          envelope.KindResourceInfo,
		SourceActions: []string{"DescribeCompShareInstance"},
		Subjects:      make([]envelope.Subject, 0, len(copied)),
		Facts:         []envelope.Fact{},
		Computed:      []envelope.Fact{},
		Constraints: envelope.Constraints{
			DoNotInventInstances:   true,
			DoNotAnswerAccountBill: true,
		},
	}
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
		addPositiveIntFact(addInstanceFact, "gpu_count", resourceLabelGPU, inst.GPU)
		addStringFact(addInstanceFact, "gpu_type", resourceLabelGPUType, inst.GpuType)
		addStringFact(addInstanceFact, "image_type", resourceLabelImageType, inst.ImageType)
		addPositiveInt64Fact(addInstanceFact, "start_time", resourceLabelStartTime, inst.StartTime)
		addPositiveIntFact(addInstanceFact, "cpu", resourceLabelCPU, inst.CPU)
		addPositiveIntFact(addInstanceFact, "memory", resourceLabelMemory, inst.Memory)
		addStringFact(addInstanceFact, "zone", "Zone", inst.Zone)
		addStringFact(addInstanceFact, "region", "Region", inst.Region)
		addStringFact(addInstanceFact, "charge_type", "ChargeType", inst.ChargeType)
		addPositiveInt64Fact(addInstanceFact, "expire_time", resourceLabelExpireTime, inst.ExpireTime)
		addStringFact(addInstanceFact, "auto_renew", "AutoRenew", inst.AutoRenew)
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

func monitorSemanticFacts(metrics []Metric, payload map[string]any) ([]monitorScalarFact, bool) {
	data, _ := payload["Data"].(map[string]any)
	if data == nil {
		return nil, false
	}
	list, _ := data["List"].([]any)
	if len(list) == 0 {
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
// a GetCompShareInstanceMonitor result. Exported for use by the engine
// M2 ToolFact writer (internal/engine/session_state.go) so monitor_sample
// facts share the renderer's metric vocabulary (cpu_usage, gpu_usage,
// vram_usage etc.) instead of raw API keys (uhost_cpu_used etc.).
//
// Multi-GPU disambiguation: a host with N GPUs produces N MonitorScalar
// entries with Key in the form "gpu_usage.GPU 1", "gpu_usage.GPU 2", ...
// The fact writer collapses them into a single fact's Payload using the
// dotted-suffix convention.
type MonitorScalar struct {
	SubjectID string
	Key       string
	Value     string
	Unit      string
}

// ExtractMonitorScalars walks a GetCompShareInstanceMonitor raw result and
// returns the per-(host, metric) latest scalar values, using the same
// renderer vocabulary that capability handlers and the grounded renderer
// emit. Returns nil if the payload is unrecognized or contains no
// known-metric data; callers should treat nil as "no fact to write" (a
// successful empty-data probe is not a fact-producing event).
//
// metrics may be empty, in which case all known metric keys are accepted.
// Pass through whatever metric set the caller has for the current turn
// (engine.go can leave it nil; the renderer-side already filters by what
// the user asked for, but ToolFact wants all observed metrics).
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

func addPositiveInt64Fact(add func(string, string, any), key, label string, value int64) {
	if value > 0 {
		add(key, label, value)
	}
}
