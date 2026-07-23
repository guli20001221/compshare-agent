package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	recentObservationPrefix = "【最近观测缓存，5 分钟内；实时状态请重新查询】"
	maxFactContextRunes     = 600
)

type factContextSubject struct {
	subjectID string
	newest    int64
	instance  []string
	monitor   []string
	other     []string
}

func assembleFactContext(facts []ToolFact, now time.Time) string {
	if len(facts) == 0 {
		return ""
	}
	nowUnix := now.Unix()
	bySubject := make(map[string]*factContextSubject)
	for _, fact := range facts {
		if !factFresh(fact, nowUnix) || fact.SubjectID == "" {
			continue
		}
		subject := bySubject[fact.SubjectID]
		if subject == nil {
			subject = &factContextSubject{subjectID: fact.SubjectID}
			bySubject[fact.SubjectID] = subject
		}
		if fact.ProducedAtUnix > subject.newest {
			subject.newest = fact.ProducedAtUnix
		}
		switch fact.Kind {
		case FactKindInstanceState:
			subject.instance = renderInstanceFact(fact.Payload)
		case FactKindMonitorSample:
			subject.monitor = renderMonitorFact(fact.Payload)
		case FactKindStockSnapshot:
			subject.other = append(subject.other, "库存 "+strings.Join(renderStockFact(fact.Payload), "，"))
		case FactKindPriceQuote:
			subject.other = append(subject.other, "价格 "+strings.Join(renderPriceFact(fact.Payload), "，"))
		case FactKindBillingQuote:
			subject.other = append(subject.other, "费用 "+strings.Join(renderBillingFact(fact.Payload), "，"))
		}
	}
	if len(bySubject) == 0 {
		return ""
	}
	subjects := make([]*factContextSubject, 0, len(bySubject))
	for _, subject := range bySubject {
		if len(subject.instance) == 0 && len(subject.monitor) == 0 && len(subject.other) == 0 {
			continue
		}
		subjects = append(subjects, subject)
	}
	if len(subjects) == 0 {
		return ""
	}
	sort.Slice(subjects, func(i, j int) bool {
		if subjects[i].newest != subjects[j].newest {
			return subjects[i].newest > subjects[j].newest
		}
		return subjects[i].subjectID < subjects[j].subjectID
	})
	lines := []string{recentObservationPrefix}
	for _, subject := range subjects {
		var parts []string
		if len(subject.instance) > 0 {
			parts = append(parts, "实例 "+strings.Join(subject.instance, "，"))
		}
		if len(subject.monitor) > 0 {
			parts = append(parts, "监控 "+strings.Join(subject.monitor, "，"))
		}
		parts = append(parts, subject.other...)
		lines = append(lines, "- "+subject.subjectID+"："+strings.Join(parts, "；"))
	}
	return truncateFactContext(strings.Join(lines, "\n"))
}

func factFresh(fact ToolFact, nowUnix int64) bool {
	if fact.TTLSeconds <= 0 || fact.ProducedAtUnix <= 0 {
		return false
	}
	return nowUnix-fact.ProducedAtUnix <= int64(fact.TTLSeconds)
}

// oldestFreshFactAgeSeconds returns the age in seconds of the oldest still-fresh,
// subject-bearing fact in the cache, or -1 when none qualify. It is the single
// stale-cache observable for the #3 StateTrace: a fact approaching its TTL is
// the case where the engine may answer from a near-expired observation. Bounded
// by the TTL (a stale fact is skipped), so the caller can bucket it safely.
func oldestFreshFactAgeSeconds(facts []ToolFact, now time.Time) int {
	nowUnix := now.Unix()
	oldest := int64(-1)
	for _, fact := range facts {
		if !factFresh(fact, nowUnix) || fact.SubjectID == "" {
			continue
		}
		if oldest < 0 || fact.ProducedAtUnix < oldest {
			oldest = fact.ProducedAtUnix
		}
	}
	if oldest < 0 {
		return -1
	}
	age := nowUnix - oldest
	if age < 0 {
		age = 0
	}
	return int(age)
}

func renderInstanceFact(payload map[string]any) []string {
	var parts []string
	if name := factString(payload, "name"); name != "" {
		parts = append(parts, name)
	}
	if state := factString(payload, "state"); state != "" {
		parts = append(parts, "状态 "+state)
	}
	gpuType := factString(payload, "gpu_type")
	gpuCount := factScalar(payload, "gpu")
	switch {
	case gpuType != "" && gpuCount != "":
		parts = append(parts, "GPU "+gpuType+" x"+gpuCount)
	case gpuType != "":
		parts = append(parts, "GPU "+gpuType)
	case gpuCount != "":
		parts = append(parts, "GPU x"+gpuCount)
	}
	if cpu := factScalar(payload, "cpu"); cpu != "" {
		parts = append(parts, "CPU "+cpu)
	}
	if memory := factScalar(payload, "memory"); memory != "" {
		parts = append(parts, "内存 "+memory)
	}
	if zone := factString(payload, "zone"); zone != "" {
		parts = append(parts, "可用区 "+zone)
	}
	return parts
}

func renderMonitorFact(payload map[string]any) []string {
	specs := []struct {
		key   string
		label string
	}{
		{key: "cpu_usage", label: "CPU"},
		{key: "memory_usage", label: "内存"},
		{key: "gpu_usage", label: "GPU"},
		{key: "vram_usage", label: "显存"},
		{key: "system_disk_usage", label: "系统盘"},
		{key: "data_disk_usage", label: "数据盘"},
	}
	var parts []string
	for _, spec := range specs {
		if value := factScalar(payload, spec.key); value != "" {
			parts = append(parts, spec.label+" "+value+"%")
		}
		for key, raw := range payload {
			if !strings.HasPrefix(key, spec.key+".") {
				continue
			}
			if value := scalarString(raw); value != "" {
				suffix := strings.TrimPrefix(key, spec.key+".")
				parts = append(parts, spec.label+"("+suffix+") "+value+"%")
			}
		}
	}
	return parts
}

func renderStockFact(payload map[string]any) []string {
	var parts []string
	if model := factString(payload, "model"); model != "" {
		parts = append(parts, "机型 "+model)
	}
	if status := factString(payload, "status"); status != "" {
		parts = append(parts, "状态 "+status)
	}
	if zone := factString(payload, "zone"); zone != "" {
		parts = append(parts, "可用区 "+zone)
	}
	if count := factScalar(payload, "count"); count != "" {
		parts = append(parts, "数量 "+count)
	}
	if enough := factScalar(payload, "enough"); enough != "" {
		parts = append(parts, "容量预检 "+enough)
	}
	return parts
}

func renderPriceFact(payload map[string]any) []string {
	var parts []string
	if target := factString(payload, "target"); target != "" {
		parts = append(parts, "对象 "+target)
	}
	if gpu := factString(payload, "gpu_type"); gpu != "" {
		parts = append(parts, "GPU "+gpu)
	}
	if zone := factString(payload, "zone"); zone != "" {
		parts = append(parts, "可用区 "+zone)
	}
	if charge := factString(payload, "charge_type"); charge != "" {
		parts = append(parts, "计费 "+charge)
	}
	if price := factScalar(payload, "price"); price != "" {
		parts = append(parts, "价格 "+price)
	}
	if original := factScalar(payload, "original_price"); original != "" {
		parts = append(parts, "原价 "+original)
	}
	return parts
}

func renderBillingFact(payload map[string]any) []string {
	var parts []string
	if target := factString(payload, "target"); target != "" {
		parts = append(parts, "对象 "+target)
	}
	if id := factString(payload, "resource_id"); id != "" {
		parts = append(parts, "资源 "+id)
	}
	if amount := factScalar(payload, "amount"); amount != "" {
		parts = append(parts, "金额 "+amount)
	}
	if note := factString(payload, "note"); note != "" {
		parts = append(parts, note)
	}
	return parts
}

func factString(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok {
		return ""
	}
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func factScalar(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok {
		return ""
	}
	return scalarString(value)
}

func scalarString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return formatFactFloat(v)
	case float32:
		return formatFactFloat(float64(v))
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case int32:
		return fmt.Sprintf("%d", v)
	case uint:
		return fmt.Sprintf("%d", v)
	case uint64:
		return fmt.Sprintf("%d", v)
	case uint32:
		return fmt.Sprintf("%d", v)
	default:
		return ""
	}
}

func formatFactFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%g", v)
}

func truncateFactContext(text string) string {
	runes := []rune(text)
	if len(runes) <= maxFactContextRunes {
		return text
	}
	suffix := "\n..."
	keep := maxFactContextRunes - len([]rune(suffix))
	if keep < len([]rune(recentObservationPrefix)) {
		keep = len([]rune(recentObservationPrefix))
	}
	return string(runes[:keep]) + suffix
}
