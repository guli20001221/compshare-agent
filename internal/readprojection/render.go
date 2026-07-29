package readprojection

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
)

const (
	resourceLabelInstanceID = "\u5b9e\u4f8bID"
	resourceLabelName       = "\u540d\u79f0"
	resourceLabelState      = "\u72b6\u6001"
	resourceLabelGPUType    = "GPU\u578b\u53f7"
	resourceLabelGPU        = "GPU\u6570\u91cf"
	resourceLabelCPU        = "CPU"
	resourceLabelMemory     = "\u5185\u5b58"
	resourceLabelImageType  = "\u955c\u50cf\u7c7b\u578b"
	resourceLabelStartTime  = "\u542f\u52a8\u65f6\u95f4"
	resourceLabelExpireTime = "\u5230\u671f\u65f6\u95f4"

	noInstancesReply              = "\u672a\u627e\u5230\u5b9e\u4f8b\u3002"
	noMonitorValuesReply          = "\u672a\u8fd4\u56de\u76d1\u63a7\u6570\u636e\u3002"
	noRequestedMonitorValuesReply = "\u672a\u8fd4\u56de\u8bf7\u6c42\u7684\u76d1\u63a7\u6307\u6807\u3002"
)

func RenderResourceSummary(instances []entity.InstanceSnapshot, meta ResourceEnvelopeMeta) string {
	if len(instances) == 0 {
		return noInstancesReply
	}
	// Caller is expected to have already applied SortInstancesForDisplay /
	// TruncateInstancesForDisplay when relevant. Render in the order
	// received so the user sees the operationally-relevant instances first.
	lines := make([]string, 0, len(instances))
	for _, inst := range instances {
		name := cleanResourceText(inst.Name)
		if name == "" {
			name = "未命名"
		}
		id := cleanResourceText(inst.UHostId)
		title := name
		if id != "" {
			title += "（" + id + "）"
		}
		parts := []string{resourceStateLabel(inst.State)}
		if inst.GPU == 0 {
			parts = append(parts, "无 GPU")
		} else {
			gpuType := cleanResourceText(inst.GpuType)
			if gpuType == "" {
				gpuType = "GPU"
			}
			parts = append(parts, fmt.Sprintf("%s × %d", gpuType, inst.GPU))
		}
		parts = append(parts, fmt.Sprintf("%d vCPU / %s", inst.CPU, resourceMemoryLabel(inst.Memory)))
		if inst.ImageType != "" {
			parts = append(parts, "镜像 "+cleanResourceText(inst.ImageType))
		}
		if inst.ChargeType != "" {
			parts = append(parts, resourceChargeTypeLabel(inst.ChargeType))
		}
		if inst.StartTime != 0 {
			parts = append(parts, "启动于 "+resourceTimeLabel(inst.StartTime))
		}
		if inst.ExpireTime != 0 {
			parts = append(parts, "到期于 "+resourceTimeLabel(inst.ExpireTime))
		}
		lines = append(lines, "- "+title+"："+strings.Join(parts, "；"))
	}
	body := strings.Join(lines, "\n")
	displayTotal := meta.TotalCount
	if meta.FilterApplied != "" && meta.MatchedCount > 0 {
		displayTotal = meta.MatchedCount
	}
	if meta.Truncated && meta.Shown > 0 && displayTotal > meta.Shown {
		body += fmt.Sprintf("\n（已显示 %d/%d 台，完整列表请到控制台查看）", meta.Shown, displayTotal)
	}
	return body
}

func cleanResourceText(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(safeValue(value)))
}

func resourceMemoryLabel(memory int) string {
	if memory <= 0 {
		return "内存未知"
	}
	if memory < 1024 {
		return fmt.Sprintf("%d GB", memory)
	}
	gb := float64(memory) / 1024
	if float64(int(gb)) == gb {
		return fmt.Sprintf("%d GB", int(gb))
	}
	return fmt.Sprintf("%.1f GB", gb)
}

func resourceTimeLabel(timestamp int64) string {
	return time.Unix(timestamp, 0).In(monitorHistoryLoc).Format("2006-01-02 15:04")
}

func resourceStateLabel(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return "运行中"
	case "stopped":
		return "已关机"
	case "initializing", "installing":
		return "初始化中"
	case "imagemaking":
		return "镜像制作中"
	case "starting":
		return "启动中"
	case "stopping":
		return "关机中"
	case "rebooting":
		return "重启中"
	case "failed":
		return "异常"
	default:
		if label := cleanResourceText(state); label != "" {
			return label
		}
		return "状态未知"
	}
}

func resourceChargeTypeLabel(chargeType string) string {
	switch strings.ToLower(strings.TrimSpace(chargeType)) {
	case "postpay", "dynamic":
		return "按量"
	case "spot":
		return "抢占式"
	case "day":
		return "按天"
	case "month":
		return "按月"
	case "year":
		return "按年"
	default:
		return cleanResourceText(chargeType)
	}
}

func RenderMonitorSummary(metrics []Metric, payload map[string]any) string {
	allFacts := monitorScalarFacts(nil, payload)
	if len(allFacts) == 0 {
		return noMonitorValuesReply
	}

	facts := allFacts
	if len(metrics) > 0 {
		facts = monitorScalarFacts(metrics, payload)
	}
	if len(facts) == 0 {
		return noRequestedMonitorValuesReply
	}

	parts := make([]string, 0, len(facts))
	for _, fact := range facts {
		value := fact.Value
		if fact.Unit != "" {
			value += fact.Unit
		}
		label := fact.Label
		if label == "" {
			label = fact.Key
		}
		parts = append(parts, label+"="+value)
	}
	if len(metrics) > 0 {
		present := presentMonitorMetrics(facts)
		for _, metric := range uniqueMonitorMetrics(metrics) {
			if !present[metric] {
				parts = append(parts, monitorMetricReplyLabel(metric)+"未返回数据")
			}
		}
	}
	return strings.Join(parts, "; ")
}

type monitorHistoricalPoint struct {
	Timestamp int64
	Value     float64
}

type monitorHistoricalSeries struct {
	Key    string
	Label  string
	Unit   string
	Metric Metric
	Points []monitorHistoricalPoint
}

func RenderHistoricalMonitorSummary(metrics []Metric, payload map[string]any, windowStart, windowEnd int64) string {
	prefix := historicalMonitorWindowPrefix(windowStart, windowEnd)
	series := historicalMonitorSeries(metrics, payload)
	if len(series) == 0 {
		if len(historicalMonitorSeries(nil, payload)) > 0 {
			return prefix + noRequestedMonitorValuesReply
		}
		return prefix + noMonitorValuesReply
	}
	parts := make([]string, 0, len(series))
	for _, item := range series {
		if len(item.Points) == 0 {
			continue
		}
		latest := item.Points[len(item.Points)-1]
		sum := 0.0
		peak := item.Points[0]
		for _, p := range item.Points {
			sum += p.Value
			if p.Value > peak.Value {
				peak = p
			}
		}
		avg := sum / float64(len(item.Points))
		label := item.Label
		if label == "" {
			label = item.Key
		}
		peakAt := time.Unix(peak.Timestamp, 0).In(monitorHistoryLoc).Format("2006-01-02 15:04")
		parts = append(parts, fmt.Sprintf("%s最新=%s%s, 平均=%s%s, 峰值=%s%s（%s）",
			label,
			formatMonitorFloat(latest.Value), item.Unit,
			formatMonitorFloat(avg), item.Unit,
			formatMonitorFloat(peak.Value), item.Unit,
			peakAt,
		))
	}
	if len(parts) == 0 {
		return prefix + noMonitorValuesReply
	}
	return prefix + strings.Join(parts, "; ")
}

// historicalMonitorWindowPrefix renders the queried Beijing window as a
// self-labeled prefix so the deterministic historical reply states the exact time
// window it aggregates — the structured replacement for the engine's former
// post-hoc date-regex correction. Empty for an unset window (0) so current-monitor
// and window-less callers are unchanged. monitorHistoryLoc is Asia/Shanghai
// (UTC+8), matching the engine's beijingZone, so the two never disagree.
func historicalMonitorWindowPrefix(start, end int64) string {
	if start <= 0 || end <= 0 {
		return ""
	}
	startAt := time.Unix(start, 0).In(monitorHistoryLoc)
	endAt := time.Unix(end, 0).In(monitorHistoryLoc)
	return fmt.Sprintf("北京时间 %s ~ %s（历史时间窗）：", startAt.Format("2006-01-02 15:04"), endAt.Format("15:04"))
}

func historicalMonitorSeries(metrics []Metric, payload map[string]any) []monitorHistoricalSeries {
	data, _ := payload["Data"].(map[string]any)
	if data == nil {
		return nil
	}
	var out []monitorHistoricalSeries
	for _, item := range mapSliceAt(data, "List") {
		instance, _ := item.(map[string]any)
		if instance == nil {
			continue
		}
		for _, metricItem := range mapSliceAt(instance, "Metrics") {
			metric, _ := metricItem.(map[string]any)
			if metric == nil {
				continue
			}
			metricKey, _ := metric["MetricKey"].(string)
			def, ok := monitorMetricDefinitions[metricKey]
			if !ok || !monitorMetricRequested(def.Metric, metrics) {
				continue
			}
			results := monitorMetricResults(metric)
			for i, result := range results {
				points := historicalPointsFromValues(result["Values"])
				if len(points) == 0 {
					continue
				}
				key, label := def.Key, def.Label
				if suffix := monitorResultLabelSuffix(metricKey, i, len(results)); suffix != "" {
					key += "." + suffix
					label += " (" + suffix + ")"
				}
				out = append(out, monitorHistoricalSeries{Key: key, Label: label, Unit: def.Unit, Metric: def.Metric, Points: points})
			}
		}
	}
	for _, item := range mapSliceAt(data, "PodList") {
		instance, _ := item.(map[string]any)
		if instance == nil {
			continue
		}
		metricItems, _ := instance["Metrics"].(map[string]any)
		if metricItems == nil {
			continue
		}
		for _, field := range []string{"Cpu", "Memory", "SysDiskUsed"} {
			values, exists := metricItems[field]
			if !exists {
				continue
			}
			def := podMonitorMetricDefinitions[field]
			if !monitorMetricRequested(def.Metric, metrics) {
				continue
			}
			if points := historicalPointsFromValues(values); len(points) > 0 {
				out = append(out, monitorHistoricalSeries{Key: def.Key, Label: def.Label, Unit: def.Unit, Metric: def.Metric, Points: points})
			}
		}
		gpuItems, _ := metricItems["Gpu"].([]any)
		for i, item := range gpuItems {
			gpu, _ := item.(map[string]any)
			if gpu == nil {
				continue
			}
			addGPU := func(field, defKey string) {
				values, exists := gpu[field]
				if !exists {
					return
				}
				def := podMonitorMetricDefinitions[defKey]
				if !monitorMetricRequested(def.Metric, metrics) {
					return
				}
				points := historicalPointsFromValues(values)
				if len(points) == 0 {
					return
				}
				key, label := def.Key, def.Label
				if suffix := podMonitorResultLabelSuffix(i, len(gpuItems)); suffix != "" {
					key += "." + suffix
					label += " (" + suffix + ")"
				}
				out = append(out, monitorHistoricalSeries{Key: key, Label: label, Unit: def.Unit, Metric: def.Metric, Points: points})
			}
			addGPU("Util", "GpuUtil")
			addGPU("Memory", "GpuMemory")
		}
	}
	return out
}

func historicalPointsFromValues(values any) []monitorHistoricalPoint {
	items, _ := values.([]any)
	out := make([]monitorHistoricalPoint, 0, len(items))
	for _, item := range items {
		point, _ := item.(map[string]any)
		if point == nil {
			continue
		}
		value, ok := monitorFloat(point["Value"])
		if !ok {
			continue
		}
		ts, ok := monitorTimestamp(point["Timestamp"])
		if !ok {
			continue
		}
		out = append(out, monitorHistoricalPoint{Timestamp: ts, Value: value})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out
}

func monitorFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		v, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return v, err == nil
	default:
		return 0, false
	}
}

func monitorTimestamp(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case string:
		v, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return v, err == nil
	default:
		return 0, false
	}
}

func formatMonitorFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func uniqueMonitorMetrics(metrics []Metric) []Metric {
	seen := map[Metric]struct{}{}
	out := make([]Metric, 0, len(metrics))
	for _, metric := range metrics {
		if metric == "" {
			continue
		}
		if _, ok := seen[metric]; ok {
			continue
		}
		seen[metric] = struct{}{}
		out = append(out, metric)
	}
	return out
}

func presentMonitorMetrics(facts []monitorScalarFact) map[Metric]bool {
	present := map[Metric]bool{}
	for _, fact := range facts {
		key := strings.ToLower(fact.Key)
		switch {
		case strings.Contains(key, "cpu"):
			present[MetricCPU] = true
		case strings.Contains(key, "vram"):
			present[MetricVRAM] = true
		case strings.Contains(key, "gpu"):
			present[MetricGPU] = true
		case strings.Contains(key, "memory"):
			present[MetricMemory] = true
		}
	}
	return present
}

func monitorMetricReplyLabel(metric Metric) string {
	switch metric {
	case MetricCPU:
		return "CPU 使用率"
	case MetricMemory:
		return "内存使用率"
	case MetricGPU:
		return "GPU 使用率"
	case MetricVRAM:
		return "显存使用率"
	default:
		return string(metric)
	}
}

func flattenScalars(prefix string, v any, out map[string]string) {
	switch typed := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			flattenScalars(next, typed[key], out)
		}
	case []any:
		for i, item := range typed {
			next := fmt.Sprintf("%s[%d]", prefix, i)
			flattenScalars(next, item, out)
		}
	default:
		if prefix != "" {
			out[prefix] = safeValue(typed)
		}
	}
}

func matchesRequestedMetric(key string, metrics []Metric) bool {
	key = strings.ToLower(key)
	for _, metric := range metrics {
		// Demo route dispatch intentionally uses substring matching over the rendered
		// monitor field paths. Narrow this only if real smoke traces show noisy
		// API metadata in user-visible replies.
		if strings.Contains(key, string(metric)) {
			return true
		}
	}
	return false
}
