package sshops

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/readprojection"
)

const (
	instanceContextSourceDescribe = "DescribeCompShareInstance"
	instanceContextSourceMonitor  = "GetCompShareInstanceMonitor"
	instanceContextSourceCatalog  = "DescribeCompShareSoftwarePort"
	// Empty monitor payloads are reported as unavailable, never as zero/healthy.
	instanceContextMonitorTimeout = 2 * time.Second
	// The independent software-catalog call has its own timeout.
	instanceContextCatalogTimeout  = 2 * time.Second
	maxInstanceContextMonitorFact  = 16
	maxInstanceContextSoftware     = 12
	maxInstanceContextCatalogPorts = 16
	maxInstanceEndpointTargets     = 16
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
	// Endpoint targets are private probe input and never enter model context.
	base.EndpointTargets = instanceEndpointTargets(inst)
	if !base.Enabled() {
		return base
	}
	observedAt := time.Now().UTC().Format(time.RFC3339)
	result := base
	software, _ := instanceContextDeclaredSoftware(inst)
	result.PlatformFacts = append(result.PlatformFacts, instanceFacts(inst, instanceID, observedAt)...)
	result.PlatformFacts = append(result.PlatformFacts, monitorFacts(ctx, d, instanceID, observedAt)...)
	result.PlatformFacts = append(result.PlatformFacts, catalogFacts(ctx, d, software, observedAt)...)
	result.Coverage = instanceContextCoverage(result.PlatformFacts)
	return result
}

// instanceEndpointTargets derives the only destinations the endpoint probe is permitted to touch.
// The model can never provide a URL or host, which keeps the tool from becoming an SSRF primitive.
// HTTP targets come from the platform's per-instance Softwares[].URL; TCP targets come from the
// instance's reported forward list and use the same advertised host as SshLoginCommand. Neither
// source is copied into the model-visible Context JSON.
func instanceEndpointTargets(inst map[string]any) []opscontext.EndpointTarget {
	targets := make([]opscontext.EndpointTarget, 0, maxInstanceEndpointTargets)
	for _, item := range instanceContextMapSlice(inst["Softwares"]) {
		if len(targets) >= maxInstanceEndpointTargets {
			break
		}
		rawURL, _ := item["URL"].(string)
		rawURL = strings.TrimSpace(rawURL)
		parsed, err := url.ParseRequestURI(rawURL)
		if err != nil || parsed == nil || parsed.User != nil || parsed.Host == "" ||
			(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) {
			continue
		}
		name := cleanEndpointLabel(allowlistedString(item, "Name"))
		if name == "" {
			name = "platform HTTP entry"
		} else {
			name += " platform entry"
		}
		targets = append(targets, opscontext.EndpointTarget{
			ID: fmt.Sprintf("platform-http-%d", len(targets)+1), Kind: "http", Label: name,
			Source: "DescribeCompShareInstance.Softwares.URL", URL: rawURL,
		})
	}

	login, _ := inst["SshLoginCommand"].(string)
	host, _, _, err := parseSSHLoginCommand(login)
	if err != nil || host == "" {
		return targets
	}
	for _, forward := range instanceContextMapSlice(inst["TcpForwards"]) {
		if len(targets) >= maxInstanceEndpointTargets {
			break
		}
		internal, internalOK := allowlistedInt(forward, "InternalPort")
		external, externalOK := allowlistedInt(forward, "ExternalPort")
		if !internalOK || !externalOK || !validInstanceContextPort(internal) || !validInstanceContextPort(external) {
			continue
		}
		targets = append(targets, opscontext.EndpointTarget{
			ID: fmt.Sprintf("platform-tcp-%d", len(targets)+1), Kind: "tcp",
			Label:  fmt.Sprintf("reported TCP forward %d -> %d", internal, external),
			Source: "DescribeCompShareInstance.TcpForwards", Host: host, Port: external,
		})
	}
	return targets
}

func cleanEndpointLabel(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, truncateInstanceContextValue(value, 96)))
}

func instanceFacts(inst map[string]any, instanceID, observedAt string) []opscontext.Fact {
	facts := make([]opscontext.Fact, 0, 9)

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

	// Two facts, not one. v1 merged the Describe Ports block and the TcpForwards list under
	// `instance.reported_ports`, which made "a port is configured on this instance" and "the platform
	// forwards that port" indistinguishable in the payload — and both readable as a guest listener,
	// which neither is. This lane has still not established whether every instance kind reports
	// control-plane exposure, image-declared defaults, or both, so each fact stays deliberately
	// neutral about what it proves; what changed is that the model can no longer confuse the two.
	hints, hintsKnown := instanceContextPortHints(inst)
	facts = append(facts, instanceContextFact("platform.instance_port_hints", hints, instanceContextSourceDescribe, observedAt, statusForKnown(hintsKnown)))
	forwards, forwardsKnown := instanceContextTCPForwards(inst)
	facts = append(facts, instanceContextFact("platform.tcp_forwards", forwards, instanceContextSourceDescribe, observedAt, statusForKnown(forwardsKnown)))

	// Names only. Softwares[] entries also carry a URL, and on a Jupyter image that URL embeds a LIVE
	// access token — the whole point of the allowlist is that a field like that never reaches a prompt
	// by being part of an object somebody forwarded wholesale.
	software, softwareKnown := instanceContextDeclaredSoftware(inst)
	facts = append(facts, instanceContextFact("instance.declared_software", software, instanceContextSourceDescribe, observedAt, statusForKnown(softwareKnown)))

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

// catalogFacts projects the image application-port catalog: what the software on this image is
// EXPECTED to listen on. It is the one fact here the lane could always have stated and never did,
// and it is also the easiest to misread, so its status is never "known" — the catalog describes an
// image, not this box, and only an SSH check can promote it to an observation.
//
// The endpoint takes no instance argument (region is filled in by the executor), so the response is
// the REGION'S whole catalog and correlating it down to the software this instance declares is what
// makes it about this instance at all. When the declared list is unavailable that correlation cannot
// happen — and then the fact ships under a DIFFERENT KEY, because a region-wide list published as
// `catalog.expected_software_ports` would be a name asserting a relevance the value does not have:
// another image's FileBrowser port would arrive as this box's expected port and send the diagnosis
// after a service that was never installed. It is still worth sending (a bounded superset is a real
// prior, and dropping it silently would look identical to the endpoint failing) — under a name, and
// a prompt note, that say it is uncorrelated.
func catalogFacts(ctx context.Context, d Describer, software []string, observedAt string) []opscontext.Fact {
	// Chosen from what is known BEFORE the call, so every return path below — unavailable, empty,
	// populated — uses one key and the audit cannot show a fact under two names for one run.
	key := "catalog.expected_software_ports"
	if len(software) == 0 {
		key = "catalog.region_port_hints"
	}
	if d == nil {
		return []opscontext.Fact{instanceContextFact(key, "unavailable", instanceContextSourceCatalog, observedAt, opscontext.StatusUnknown)}
	}
	catalogCtx, cancel := context.WithTimeout(ctx, instanceContextCatalogTimeout)
	defer cancel()
	started := time.Now()
	raw, err := d.Execute(catalogCtx, instanceContextSourceCatalog, map[string]any{})
	elapsed := time.Since(started)
	if err != nil {
		// Same three-way split as the monitor, for the same reason: the fact the model sees cannot
		// distinguish a short budget from a refused call, and those need different fixes.
		reason := "upstream_error"
		if catalogCtx.Err() == context.DeadlineExceeded {
			reason = "deadline_exceeded"
		}
		log.Printf("ssh-ops: instance context software-port catalog %s after %s (budget %s): %v",
			reason, elapsed.Round(time.Millisecond), instanceContextCatalogTimeout, err)
		return []opscontext.Fact{instanceContextFact(key, "unavailable", instanceContextSourceCatalog, observedAt, opscontext.StatusUnknown)}
	}
	entries := instanceContextCatalogEntries(raw, software)
	if len(entries) == 0 {
		// RetCode 0 and nothing correlated. Deliberately NOT "known: this instance has no expected
		// ports" — the catalog may simply not cover this image, and an empty list asserted as known
		// would be read as "nothing should be listening", which is the opposite of what it says.
		log.Printf("ssh-ops: instance context software-port catalog empty_result in %s (budget %s): %d declared software name(s)",
			elapsed.Round(time.Millisecond), instanceContextCatalogTimeout, len(software))
		return []opscontext.Fact{instanceContextFact(key, entries, instanceContextSourceCatalog, observedAt, opscontext.StatusUnknown)}
	}
	return []opscontext.Fact{instanceContextFact(key, entries, instanceContextSourceCatalog, observedAt, opscontext.StatusReported)}
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
		case "platform.instance_port_hints":
			if fact.Status == opscontext.StatusKnown {
				coverage |= opscontext.CoveragePortHints
			}
		case "platform.tcp_forwards":
			if fact.Status == opscontext.StatusKnown {
				coverage |= opscontext.CoverageTCPForwards
			}
		case "instance.declared_software":
			if fact.Status == opscontext.StatusKnown {
				coverage |= opscontext.CoverageSoftware
			}
		case "catalog.expected_software_ports":
			// Reported, not known: this fact describes the image catalog, and it never gets to
			// claim the stronger status the others use. The bit still has to be set from the
			// status it actually carries, or coverage would under-report a fact that was sent.
			if fact.Status == opscontext.StatusReported {
				coverage |= opscontext.CoverageCatalogPorts
			}
		case "catalog.region_port_hints":
			// Its own bit, so the audit can tell "the model got this instance's expected ports"
			// from "the model got an uncorrelated region list" — the two support very different
			// conclusions, and one bit for both would hide which one a run actually had.
			if fact.Status == opscontext.StatusReported {
				coverage |= opscontext.CoverageRegionPortHints
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

func instanceContextPortHints(inst map[string]any) (map[string]any, bool) {
	result := map[string]any{}
	ports, portsPresent := inst["Ports"].(map[string]any)
	if !portsPresent {
		return result, false
	}
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
	return result, true
}

func instanceContextTCPForwards(inst map[string]any) ([]map[string]int, bool) {
	forwards := make([]map[string]int, 0)
	raw, present := inst["TcpForwards"]
	if !present {
		return forwards, false
	}
	for _, forward := range instanceContextMapSlice(raw) {
		internal, internalOK := allowlistedInt(forward, "InternalPort")
		external, externalOK := allowlistedInt(forward, "ExternalPort")
		if internalOK && externalOK && validInstanceContextPort(internal) && validInstanceContextPort(external) {
			forwards = append(forwards, map[string]int{"internal": internal, "external": external})
		}
	}
	return forwards, true
}

// instanceContextDeclaredSoftware reads Softwares[].Name and NOTHING else from each entry.
// The sibling URL field is the reason this is a named projection rather than a field copy: on a
// Jupyter image it carries a live access token, so an entry must never be forwarded as an object.
func instanceContextDeclaredSoftware(inst map[string]any) ([]string, bool) {
	names := make([]string, 0)
	raw, present := inst["Softwares"]
	if !present {
		return names, false
	}
	for _, item := range instanceContextMapSlice(raw) {
		name := allowlistedString(item, "Name")
		if name == "" || len(names) >= maxInstanceContextSoftware {
			continue
		}
		names = append(names, name)
	}
	// Present-but-empty stays "unknown" on purpose. An image response uses a MAP under the same key,
	// and a shape this projection does not read must not be reported as "declares no software".
	return names, len(names) > 0
}

// instanceContextCatalogEntries projects SoftwarePort[] down to {software, port}, correlated to the
// names this instance declares. An empty `software` means the correlation is unavailable, not that
// nothing matched, so the catalog is passed through capped rather than filtered to nothing.
func instanceContextCatalogEntries(raw map[string]any, software []string) []map[string]any {
	entries := make([]map[string]any, 0)
	for _, item := range instanceContextMapSlice(raw["SoftwarePort"]) {
		name := allowlistedString(item, "Software")
		port, ok := allowlistedInt(item, "Port")
		if name == "" || !ok || !validInstanceContextPort(port) {
			continue
		}
		if len(software) > 0 && !instanceContextDeclares(software, name) {
			continue
		}
		if len(entries) >= maxInstanceContextCatalogPorts {
			break
		}
		entries = append(entries, map[string]any{"software": name, "port": port})
	}
	return entries
}

// instanceContextDeclares matches case-insensitively and exactly, the same rule the instance-access
// capability already uses to line an instance's Softwares[].Name up with a catalog Software value.
func instanceContextDeclares(software []string, name string) bool {
	for _, declared := range software {
		if strings.EqualFold(declared, name) {
			return true
		}
	}
	return false
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
