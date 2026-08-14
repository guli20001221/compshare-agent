package sshops

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/readprojection"
)

const (
	instanceContextSourceDescribe = "DescribeCompShareInstance"
	instanceContextSourceMonitor  = "GetCompShareInstanceMonitor"
	// 2s, and NOT because the monitor fact never goes missing: it does, on roughly half the live UHost
	// calls. The cause is not this budget. Measured 2026-08-14 through the production executor,
	// GetCompShareInstanceMonitor answered in 81ms..1.25s over 12 samples (max on the UHost) — and the
	// runs that lost the fact answered in ~140ms with RetCode 0 and `Data.List[0].Metrics: []`, an
	// EMPTY metric list, seconds after the same call for the same instance had returned 8 scalars.
	// A live 2s-loses-it / 10s-keeps-it pair had made this look like a deadline; it was the same
	// intermittent empty window, and raising the budget would have shipped a 2.5x change against a
	// falsified cause. Two consecutive empty responses ~1s apart also say an immediate retry would
	// not help. So the budget only has to clear the measured latency (1.6x), the empty payload is
	// reported honestly as `monitor: unavailable` / unknown — never as 0%/healthy — and monitorFacts
	// logs elapsed plus which of the three (deadline / upstream error / empty) actually happened,
	// so a future change here is driven by production numbers rather than by one coincidence.
	instanceContextMonitorTimeout = 2 * time.Second
	maxInstanceContextMonitorFact = 16
)

// enrichInstanceOpsContext adds a deliberately small allowlist projection of
// the Describe response already fetched for the SSH credential. It must never
// forward that raw response: it contains SshLoginCommand, Password, IP fields,
// Jupyter tokens and image-specific credentials that belong only at their
// respective trust boundaries.
//
// Monitoring is best effort. A slow or unavailable monitor endpoint produces an
// explicit unknown fact and cannot prevent a consented SSH diagnosis from
// starting.
func enrichInstanceOpsContext(ctx context.Context, d Describer, base opscontext.Context, inst map[string]any, instanceID string) opscontext.Context {
	if !base.Enabled() {
		return base
	}
	observedAt := time.Now().UTC().Format(time.RFC3339)
	result := base
	result.PlatformFacts = append(result.PlatformFacts, instanceFacts(inst, instanceID, observedAt)...)
	result.PlatformFacts = append(result.PlatformFacts, monitorFacts(ctx, d, instanceID, observedAt)...)
	result.Coverage = instanceContextCoverage(result.PlatformFacts)
	return result
}

func instanceFacts(inst map[string]any, instanceID, observedAt string) []opscontext.Fact {
	facts := make([]opscontext.Fact, 0, 7)

	state := allowlistedString(inst, "State")
	facts = append(facts, instanceContextFact("instance.state", state, instanceContextSourceDescribe, observedAt, statusForString(state)))

	gpu := map[string]any{}
	if count, ok := allowlistedInt(inst, "GPU"); ok {
		gpu["count"] = count
	}
	if gpuType := allowlistedString(inst, "GpuType"); gpuType != "" {
		gpu["type"] = gpuType
	}
	facts = append(facts, instanceContextFact("instance.gpu", gpu, instanceContextSourceDescribe, observedAt, statusForMap(gpu)))

	image := map[string]any{}
	if name := firstAllowlistedString(inst, "CompShareImageName", "ImageName"); name != "" {
		image["name"] = name
	}
	if imageType := firstAllowlistedString(inst, "CompShareImageType", "ImageType"); imageType != "" {
		image["type"] = imageType
	}
	facts = append(facts, instanceContextFact("instance.image", image, instanceContextSourceDescribe, observedAt, statusForMap(image)))

	disks, disksKnown := instanceContextDisks(inst)
	facts = append(facts, instanceContextFact("instance.disks", disks, instanceContextSourceDescribe, observedAt, statusForKnown(disksKnown)))

	ports, portsKnown := instanceContextPorts(inst)
	// Describe's Ports/TcpForwards fields are useful leads, but this lane has not established whether
	// every instance kind reports control-plane exposure, image-declared defaults, or both. Keep the
	// fact deliberately neutral: neither it nor its presence proves an external route or guest listener.
	facts = append(facts, instanceContextFact("instance.reported_ports", ports, instanceContextSourceDescribe, observedAt, statusForKnown(portsKnown)))
	// This fact guards the most damaging inference in historical incidents: reported port metadata is
	// not evidence that a guest process is listening. Only a subsequent SSH command can change this status.
	facts = append(facts, instanceContextFact("guest.listeners", "not_checked", "ssh", observedAt, opscontext.StatusNotObserved))

	if instanceID != "" {
		facts = append(facts, instanceContextFact("instance.id", instanceID, instanceContextSourceDescribe, observedAt, opscontext.StatusKnown))
	}
	return facts
}

func monitorFacts(ctx context.Context, d Describer, instanceID, observedAt string) []opscontext.Fact {
	if d == nil {
		return []opscontext.Fact{instanceContextFact("monitor", "unavailable", instanceContextSourceMonitor, observedAt, opscontext.StatusUnknown)}
	}
	monitorCtx, cancel := context.WithTimeout(ctx, instanceContextMonitorTimeout)
	defer cancel()
	started := time.Now()
	raw, err := d.Execute(monitorCtx, instanceContextSourceMonitor, map[string]any{"UHostIds": []string{instanceID}})
	elapsed := time.Since(started)
	if err != nil {
		// Three different failures collapse into the same "unavailable" fact for the model — it can
		// act on none of them — but they need different fixes and the fact cannot tell them apart:
		// a deadline says the budget is short, an upstream error says the call is wrong or refused,
		// and an empty-but-successful payload (logged below) says neither, which is exactly the one
		// that got misread as a deadline. Elapsed is logged on every path so the budget stays
		// measured. INV-6 holds: this endpoint returns monitor scalars, never a credential.
		reason := "upstream_error"
		if monitorCtx.Err() == context.DeadlineExceeded {
			reason = "deadline_exceeded"
		}
		log.Printf("ssh-ops: instance context monitor %s for instance %s after %s (budget %s): %v",
			reason, instanceID, elapsed.Round(time.Millisecond), instanceContextMonitorTimeout, err)
		return []opscontext.Fact{instanceContextFact("monitor", "unavailable", instanceContextSourceMonitor, observedAt, opscontext.StatusUnknown)}
	}
	scalars := readprojection.ExtractMonitorScalars(raw, nil)
	if len(scalars) == 0 {
		// RetCode 0 with an empty metric list. Named explicitly so it is never read as "the call
		// failed" or "the box reports 0%" — it is the upstream having no points for this window.
		log.Printf("ssh-ops: instance context monitor empty_result for instance %s in %s (budget %s): RetCode 0, no metrics",
			instanceID, elapsed.Round(time.Millisecond), instanceContextMonitorTimeout)
	}
	facts := make([]opscontext.Fact, 0, len(scalars))
	for _, scalar := range scalars {
		if scalar.SubjectID != instanceID || len(facts) >= maxInstanceContextMonitorFact {
			continue
		}
		value := map[string]any{"value": scalar.Value}
		if scalar.Unit != "" {
			value["unit"] = scalar.Unit
		}
		facts = append(facts, instanceContextFact("monitor."+scalar.Key, value, instanceContextSourceMonitor, observedAt, opscontext.StatusKnown))
	}
	if len(facts) == 0 {
		return []opscontext.Fact{instanceContextFact("monitor", "unavailable", instanceContextSourceMonitor, observedAt, opscontext.StatusUnknown)}
	}
	return facts
}

func instanceContextCoverage(facts []opscontext.Fact) uint32 {
	var coverage uint32
	for _, fact := range facts {
		switch fact.Key {
		case "instance.state":
			coverage |= opscontext.CoverageInstance
		case "instance.gpu":
			if fact.Status == opscontext.StatusKnown {
				coverage |= opscontext.CoverageGPU
			}
		case "instance.image":
			if fact.Status == opscontext.StatusKnown {
				coverage |= opscontext.CoverageImage
			}
		case "instance.disks":
			if fact.Status == opscontext.StatusKnown {
				coverage |= opscontext.CoverageDisk
			}
		case "instance.reported_ports":
			if fact.Status == opscontext.StatusKnown {
				coverage |= opscontext.CoveragePorts
			}
		default:
			if instanceContextMonitorKey(fact.Key) {
				coverage |= opscontext.CoverageMonitor
			}
		}
	}
	return coverage
}

func instanceContextDisks(inst map[string]any) ([]map[string]any, bool) {
	raw, present := inst["DiskSet"]
	if !present {
		return []map[string]any{}, false
	}
	disks := make([]map[string]any, 0)
	for _, item := range instanceContextMapSlice(raw) {
		disk := map[string]any{}
		if diskType := firstAllowlistedString(item, "DiskType", "Type"); diskType != "" {
			disk["type"] = diskType
		}
		if size, ok := firstAllowlistedInt(item, "Size", "DiskSpace", "SizeGB"); ok {
			disk["size_gb"] = size
		}
		if status := allowlistedString(item, "Status"); status != "" {
			disk["status"] = status
		}
		if len(disk) > 0 {
			disks = append(disks, disk)
		}
	}
	return disks, true
}

func instanceContextPorts(inst map[string]any) (map[string]any, bool) {
	result := map[string]any{}
	ports, portsPresent := inst["Ports"].(map[string]any)
	if portsPresent {
		for _, entry := range []struct {
			rawKey string
			key    string
		}{
			{rawKey: "HttpPorts", key: "http"},
			{rawKey: "TcpPorts", key: "tcp"},
			{rawKey: "UdpPorts", key: "udp"},
		} {
			if raw, ok := ports[entry.rawKey]; ok {
				result[entry.key] = instanceContextPortList(raw)
			}
		}
	}
	forwardsRaw, forwardsPresent := inst["TcpForwards"]
	if forwardsPresent {
		forwards := make([]map[string]int, 0)
		for _, forward := range instanceContextMapSlice(forwardsRaw) {
			internal, internalOK := allowlistedInt(forward, "InternalPort")
			external, externalOK := allowlistedInt(forward, "ExternalPort")
			if validInstanceContextPort(internal) && internalOK && validInstanceContextPort(external) && externalOK {
				forwards = append(forwards, map[string]int{"internal": internal, "external": external})
			}
		}
		result["tcp_forwards"] = forwards
	}
	return result, portsPresent || forwardsPresent
}

func instanceContextPortList(raw any) []int {
	ports := make([]int, 0)
	for _, value := range instanceContextSlice(raw) {
		port, ok := instanceContextInt(value)
		if ok && validInstanceContextPort(port) {
			ports = append(ports, port)
		}
	}
	return ports
}

func instanceContextFact(key string, value any, source, observedAt, status string) opscontext.Fact {
	return opscontext.Fact{Key: key, Value: value, Source: source, ObservedAt: observedAt, Status: status}
}

func statusForString(value string) string {
	if value == "" {
		return opscontext.StatusUnknown
	}
	return opscontext.StatusKnown
}

func statusForMap(value map[string]any) string { return statusForKnown(len(value) > 0) }

func statusForKnown(known bool) string {
	if known {
		return opscontext.StatusKnown
	}
	return opscontext.StatusUnknown
}

func firstAllowlistedString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := allowlistedString(raw, key); value != "" {
			return value
		}
	}
	return ""
}

func allowlistedString(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return truncateInstanceContextValue(strings.TrimSpace(value), 256)
}

func firstAllowlistedInt(raw map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if value, ok := allowlistedInt(raw, key); ok {
			return value, true
		}
	}
	return 0, false
}

func allowlistedInt(raw map[string]any, key string) (int, bool) {
	return instanceContextInt(raw[key])
}

func instanceContextInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

func validInstanceContextPort(port int) bool { return port >= 1 && port <= 65535 }

func instanceContextMonitorKey(key string) bool {
	const prefix = "monitor."
	return len(key) >= len(prefix) && key[:len(prefix)] == prefix
}

func instanceContextSlice(raw any) []any {
	switch typed := raw.(type) {
	case []any:
		return typed
	case []int:
		items := make([]any, len(typed))
		for i, value := range typed {
			items[i] = value
		}
		return items
	case []float64:
		items := make([]any, len(typed))
		for i, value := range typed {
			items[i] = value
		}
		return items
	}
	return nil
}

func instanceContextMapSlice(raw any) []map[string]any {
	items := instanceContextSlice(raw)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if mapped, ok := item.(map[string]any); ok {
			result = append(result, mapped)
		}
	}
	return result
}

func truncateInstanceContextValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}
