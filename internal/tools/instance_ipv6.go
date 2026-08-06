package tools

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
)

// InstanceIPv6Resolver answers one question for the SSH-ops lane: which address is
// this instance reachable at FROM HERE.
//
// An instance's SshLoginCommand advertises its public EIP, which is correct for the
// customer's own laptop and useless from inside the UCloud private network, where
// this service runs: the packets are silently dropped, so the dial does not fail
// fast, it times out — measured across 3 instances, 2 ports and 2 regions on
// 2026-08-06, each of which connected in under 1.2s from a normal network using the
// identical transport. The platform's own answer to the same problem is the
// instance's internal IPv6 (see UVPCClient.TransformIPv4ToIPv6), so this resolver
// derives that address from the describe payload the lane has already fetched.
//
// It is deliberately per-instance and read-only: it holds no credential, and the
// only state it keeps is the region-name -> region-id table, which is account-wide
// and effectively static.
type InstanceIPv6Resolver struct {
	uvpc  *UVPCClient
	zones ToolExecutor

	mu        sync.Mutex
	regionIDs map[string]uint32
}

// NewInstanceIPv6Resolver wires a resolver against the internal gateway URL and the
// executor used to look up region ids (DescribeCompShareSupportZone — a public,
// account-scoped read, so no region table is hardcoded here and none can go stale).
func NewInstanceIPv6Resolver(gatewayURL string, zones ToolExecutor) *InstanceIPv6Resolver {
	return &InstanceIPv6Resolver{uvpc: NewUVPCClient(gatewayURL), zones: zones}
}

// ResolveHost returns the internal IPv6 to dial for this instance.
//
// It returns ("", nil) — keep the advertised address — when the instance carries no
// private IPv4 / VPC pair to map. That is the honest answer for a Pod, whose
// SshLoginCommand names a DNS host and whose SSH port is a cluster-side forward, not
// a port on the box; inventing an IPv6 for it would move the dial to an address that
// never served that port.
//
// A non-nil error means the mapping should have worked and did not. Callers must NOT
// silently fall back to the public address in that case: falling back would dial the
// route the operator has already declared unreachable, then blame the instance's
// firewall for the timeout — the wrong-layer diagnosis this lane has been burned by
// twice (#516, #522).
func (r *InstanceIPv6Resolver) ResolveHost(ctx context.Context, instance map[string]any) (string, error) {
	privateIP, vpcID := privateEndpoint(instance)
	if privateIP == "" || vpcID == "" {
		return "", nil
	}
	region, _ := instance["Region"].(string)
	region = strings.TrimSpace(region)
	if region == "" {
		return "", fmt.Errorf("instance ipv6: describe response carries no Region")
	}
	regionID, err := r.regionID(ctx, region)
	if err != nil {
		return "", err
	}
	return r.uvpc.TransformIPv4ToIPv6(ctx, regionID, privateIP, vpcID)
}

// privateEndpoint picks the instance's private IPv4 and the VPC it belongs to out of
// IPSet. Both live on the SAME entry (Type=Private) — the public BGP entry carries no
// VPCId at all — so they are read together rather than scanned for independently,
// which would risk pairing an address with another interface's VPC.
func privateEndpoint(instance map[string]any) (ip, vpcID string) {
	entries, _ := instance["IPSet"].([]any)
	for _, raw := range entries {
		e, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := e["Type"].(string); !strings.EqualFold(strings.TrimSpace(t), "Private") {
			continue
		}
		candidate, _ := e["IP"].(string)
		vpc, _ := e["VPCId"].(string)
		candidate, vpc = strings.TrimSpace(candidate), strings.TrimSpace(vpc)
		if parsed := net.ParseIP(candidate); parsed == nil || parsed.To4() == nil || vpc == "" {
			continue
		}
		// The default NIC is the one whose address the platform maps; a secondary
		// interface would produce a valid IPv6 for an address sshd is not bound to.
		if def, _ := e["Default"].(string); strings.EqualFold(strings.TrimSpace(def), "true") {
			return candidate, vpc
		}
		if ip == "" {
			ip, vpcID = candidate, vpc
		}
	}
	return ip, vpcID
}

// regionID maps a region name to the numeric id UVPC wants, via the public
// DescribeCompShareSupportZone read. Cached for the process lifetime: the mapping is
// a property of the platform's regions, not of the account or the session, and a
// per-diagnosis lookup would put an avoidable upstream call in front of every SSH.
func (r *InstanceIPv6Resolver) regionID(ctx context.Context, region string) (uint32, error) {
	r.mu.Lock()
	if id, ok := r.regionIDs[region]; ok {
		r.mu.Unlock()
		return id, nil
	}
	r.mu.Unlock()

	if r.zones == nil {
		return 0, fmt.Errorf("instance ipv6: no executor wired to look up region %q", region)
	}
	raw, err := r.zones.Execute(ctx, "DescribeCompShareSupportZone", map[string]any{})
	if err != nil {
		return 0, fmt.Errorf("instance ipv6: describe support zones: %w", err)
	}
	table := regionIDTable(raw)
	if len(table) == 0 {
		return 0, fmt.Errorf("instance ipv6: support-zone response carried no region ids")
	}
	r.mu.Lock()
	if r.regionIDs == nil {
		r.regionIDs = make(map[string]uint32, len(table))
	}
	for name, id := range table {
		r.regionIDs[name] = id
	}
	id, ok := r.regionIDs[region]
	r.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("instance ipv6: region %q is not in the support-zone response", region)
	}
	return id, nil
}

// regionIDTable folds the zone list into region -> region id. Several zones share a
// region and agree on its id, so the fold is a plain overwrite.
func regionIDTable(raw map[string]any) map[string]uint32 {
	zones, _ := raw["ZoneInfo"].([]any)
	out := make(map[string]uint32, len(zones))
	for _, item := range zones {
		z, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := z["Region"].(string)
		name = strings.TrimSpace(name)
		id, ok := asUint32(z["RegionId"])
		if name == "" || !ok || id == 0 {
			continue
		}
		out[name] = id
	}
	return out
}

// asUint32 accepts the shapes a JSON number survives as through map[string]any.
func asUint32(v any) (uint32, bool) {
	switch n := v.(type) {
	case float64:
		if n <= 0 || n != float64(uint32(n)) {
			return 0, false
		}
		return uint32(n), true
	case int:
		if n <= 0 {
			return 0, false
		}
		return uint32(n), true
	case uint32:
		return n, n != 0
	case string:
		var parsed uint64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &parsed); err != nil || parsed == 0 || parsed > 1<<32-1 {
			return 0, false
		}
		return uint32(parsed), true
	}
	return 0, false
}
