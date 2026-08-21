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

// TransformIPv4ToIPv6 asks UVPC for the private-network fabric address of an
// instance. The mapping depends on region and VPC metadata and is not safely
// derivable locally. The returned address is a fabric mapping, not necessarily
// configured on a guest interface.
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
