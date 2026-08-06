package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// UVPCClient calls the internal UVPC backend (UVPCFEGO) through the UCloud
// private-network gateway. Same transport as UAccountClient — see
// postInternalGateway for why the internal gateway needs no signing.
type UVPCClient struct {
	url        string
	httpClient *http.Client
}

// NewUVPCClient creates a client pointed at the internal gateway URL (the same
// endpoint as agent.sts.iam_url; the gateway routes by the body's Backend field,
// so the path is just "/").
func NewUVPCClient(url string) *UVPCClient {
	return &UVPCClient{
		url:        url,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// TransformIPv4ToIPv6 maps an instance's PRIVATE IPv4 — plus the VPC it lives in —
// to the IPv6 address that same box answers on from inside the UCloud network.
//
// Why this exists at all: a CompShare instance advertises a public EIP, and a
// process running inside the private network (this service runs in-cluster) has no
// route to it. The platform's own control plane does not use the EIP either —
// compshare-access dials [IPv6]:22 to reach the very same machines
// (compshare-access internal/logic/ssh/ssh.go), and uhost-compshare-api derives
// that address with exactly this call before starting a container
// (uhost-compshare-api internal/logic/uvpc/uvpc.go). So the IPv6 is not an
// alternative route bolted on here; it is the route the platform already uses.
//
// The mapping is NOT computable locally, which is why this is a call and not a
// formula. It does embed the IPv4 verbatim — the production database's own
// 10.60.118.35 appears at bytes 8..11 of the IPv6 that deploy/conf/config.prod.yaml
// already dials for it — but the surrounding /64 prefix and the trailing 32 bits
// come from VPC/region metadata that only UVPC holds. Deriving the address by
// pattern-matching that one sample would be a guess; asking the service that owns
// it is not.
//
// The returned address is a FABRIC MAPPING, not an address configured on the guest.
// Measured 2026-08-06 from inside two running instances (both container-image boxes,
// entered over their EIP): /proc/net/if_inet6 held only ::1 and a link-local fe80::/64
// on eth0 — no global IPv6 anywhere. So SSHing in to "check the IPv6" finds nothing and
// proves nothing; the translation happens in the VPC, which is what "Transform" in the
// action name means and why only UVPC can produce the value. What the same probe did
// establish is that sshd accepts it once translated: /proc/net/tcp6 shows LISTEN on the
// :: wildcard for BOTH 0016 (22) and 0017 (23), and /proc/net/tcp the same on 0.0.0.0.
//
// Field names are the wire's, not ours: RegionId and VPCId are PascalCase while
// the address is lowercase "ip" (uhost-compshare-api internal/service/uvpc/type.go).
func (c *UVPCClient) TransformIPv4ToIPv6(ctx context.Context, regionID uint32, privateIP, vpcID string) (string, error) {
	if regionID == 0 {
		return "", fmt.Errorf("uvpc: TransformIPv4ToIPv6 needs a region id")
	}
	if ip := net.ParseIP(strings.TrimSpace(privateIP)); ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("uvpc: TransformIPv4ToIPv6 needs an IPv4 address, got %q", privateIP)
	}
	if strings.TrimSpace(vpcID) == "" {
		return "", fmt.Errorf("uvpc: TransformIPv4ToIPv6 needs a VPC id")
	}
	body, err := postInternalGateway(ctx, c.httpClient, c.url, "uvpc", map[string]any{
		"Backend":  "UVPCFEGO",
		"Action":   "TransformIPv4ToIPv6",
		"RegionId": regionID,
		"ip":       strings.TrimSpace(privateIP),
		"VPCId":    strings.TrimSpace(vpcID),
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		RetCode int    `json:"RetCode"`
		Message string `json:"Message"`
		IpV6    string `json:"IpV6"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("uvpc: TransformIPv4ToIPv6 parse: %w", err)
	}
	if resp.RetCode != 0 {
		return "", fmt.Errorf("uvpc: TransformIPv4ToIPv6 RetCode=%d: %s", resp.RetCode, resp.Message)
	}
	// A RetCode=0 with an empty or non-IPv6 address is treated as a failure rather
	// than passed on: the caller's only use for it is to dial it, and a blank host
	// would fail three layers later as an unexplained connection error.
	got := strings.TrimSpace(resp.IpV6)
	if ip := net.ParseIP(got); ip == nil || ip.To4() != nil {
		return "", fmt.Errorf("uvpc: TransformIPv4ToIPv6 returned %q, which is not an IPv6 address", got)
	}
	return got, nil
}
