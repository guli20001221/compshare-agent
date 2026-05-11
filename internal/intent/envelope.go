package intent

import (
	"sort"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
)

func BuildResourceEnvelope(instances []entity.InstanceSnapshot) envelope.Envelope {
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
	return env
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
		env.Facts = append(env.Facts, envelope.Fact{
			SubjectID:   subjectID,
			Key:         item.Key,
			Label:       item.Label,
			Value:       item.Value,
			Source:      envelope.FactSourceAPI,
			Period:      "latest",
			Aggregation: "latest",
		})
	}
	return env
}

type monitorScalarFact struct {
	Key   string
	Label string
	Value string
}

func monitorScalarFacts(metrics []Metric, payload map[string]any) []monitorScalarFact {
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
