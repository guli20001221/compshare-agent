package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// liveIPSet reproduces the IPSet a real DescribeCompShareInstance returns, field
// spellings and all, captured from the production account on 2026-08-06 with the ids
// and addresses replaced. The shape is the load-bearing part and it is not guessable:
// the private entry is the ONLY one carrying VPCId (the public BGP entry has none),
// the booleans arrive as the strings "true"/"false", and Type is "Private"/"BGP" —
// a hand-invented fixture would likely have put VPCId at the top level and made
// Default a bool, and every assertion below would then have passed against a response
// shape that does not exist.
func liveIPSet() []any {
	return []any{
		map[string]any{
			"Default": "true", "IP": "10.23.55.112", "IPMode": "IPv4",
			"Mac": "52:54:00:00:00:01", "NetworkInterfaceId": "",
			"SubnetId": "subnet-example00001", "Type": "Private",
			"VPCId": "uvnet-example00001", "Weight": float64(0),
		},
		map[string]any{
			"Bandwidth": float64(150), "IP": "203.0.113.10", "IPId": "eip-example00001",
			"IPMode": "IPv4", "NetworkInterfaceId": "", "Type": "BGP", "Weight": float64(50),
		},
	}
}

func liveInstance() map[string]any {
	return map[string]any{
		"UHostId": "uhost-example00001",
		"Region":  "cn-sh2",
		"Zone":    "cn-sh2-02",
		"State":   "Running",
		"IPSet":   liveIPSet(),
	}
}

// zoneStub answers DescribeCompShareSupportZone with the real response's shape: several
// zones per region, RegionId arriving as a JSON number (float64 once through map[string]any).
type zoneStub struct {
	calls int
	err   error
}

func (z *zoneStub) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	z.calls++
	if z.err != nil {
		return nil, z.err
	}
	if action != "DescribeCompShareSupportZone" {
		return nil, fmt.Errorf("unexpected action %q", action)
	}
	return map[string]any{"ZoneInfo": []any{
		map[string]any{"Region": "cn-wlcb", "RegionId": float64(1000039), "Zone": "cn-wlcb-01", "ZoneId": float64(10027)},
		map[string]any{"Region": "cn-sh2", "RegionId": float64(1000009), "Zone": "cn-sh2-02", "ZoneId": float64(8200)},
		map[string]any{"Region": "cn-sh2", "RegionId": float64(1000009), "Zone": "cn-sh2-05", "ZoneId": float64(8205), "IsPod": true},
	}}, nil
}

// uvpcStub records what the resolver asked the backend for.
func uvpcStub(t *testing.T, addr string) (string, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"RetCode":0,"IpV6":%q}`, addr))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got
}

func TestResolveHostMapsThePrivateEntryNotThePublicOne(t *testing.T) {
	url, sent := uvpcStub(t, "2003:da8:2004:1000:a3c:7623:2712:f9c0")
	zones := &zoneStub{}
	r := NewInstanceIPv6Resolver(url, zones)

	host, err := r.ResolveHost(context.Background(), liveInstance())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "2003:da8:2004:1000:a3c:7623:2712:f9c0" {
		t.Fatalf("got host %q", host)
	}
	// The public address must never be what gets mapped: it belongs to no VPC, and the
	// transform is defined on the private address the instance actually holds.
	if (*sent)["ip"] != "10.23.55.112" {
		t.Errorf("mapped ip = %#v, want the private address", (*sent)["ip"])
	}
	if (*sent)["VPCId"] != "uvnet-example00001" {
		t.Errorf("VPCId = %#v, want the VPC from the same IPSet entry", (*sent)["VPCId"])
	}
	if (*sent)["RegionId"] != float64(1000009) {
		t.Errorf("RegionId = %#v, want cn-sh2's id from the support-zone response", (*sent)["RegionId"])
	}
}

// A secondary NIC would produce a perfectly valid IPv6 for an address sshd is not
// listening on, so the default interface wins regardless of list order.
func TestResolveHostPrefersTheDefaultInterface(t *testing.T) {
	url, sent := uvpcStub(t, "2003:da8::1")
	inst := liveInstance()
	inst["IPSet"] = append([]any{
		map[string]any{"Default": "false", "IP": "10.23.99.99", "Type": "Private", "VPCId": "uvnet-secondary001"},
	}, liveIPSet()...)

	if _, err := NewInstanceIPv6Resolver(url, &zoneStub{}).ResolveHost(context.Background(), inst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if (*sent)["ip"] != "10.23.55.112" {
		t.Errorf("mapped ip = %#v, want the default interface's address", (*sent)["ip"])
	}
}

// The selector keys on Type, not on "this entry happens to carry a VPCId".
//
// Today the two are equivalent — no public entry in a real response has a VPCId — so this
// fixture is deliberately CONSTRUCTED rather than captured, and it is not a claim that
// upstream returns this shape. It pins the direction of the dependency: if a public entry
// ever gains a VPCId, keying on the field would silently map the public address and send
// the lane back to dialling the exact route it cannot reach, with nothing in the message
// to say so. Types are the documented enum (Internation / Bgp / Private / Proxy —
// uhost-compshare-api pkg/api/describe_compshare_instance.go).
func TestResolveHostSelectsOnTypeNotOnTheVPCFieldBeingPresent(t *testing.T) {
	url, sent := uvpcStub(t, "2003:da8::1")
	inst := liveInstance()
	inst["IPSet"] = []any{
		map[string]any{"IP": "203.0.113.10", "IPMode": "IPv4", "Type": "Internation", "VPCId": "uvnet-example00001"},
	}

	host, err := NewInstanceIPv6Resolver(url, &zoneStub{}).ResolveHost(context.Background(), inst)
	if err != nil || host != "" {
		t.Fatalf("host=%q err=%v, want no mapping for a non-private entry", host, err)
	}
	if len(*sent) != 0 {
		t.Errorf("a public address must never be sent to the transform, sent %#v", *sent)
	}
}

// "Nothing to map" is a legitimate answer, not a failure: a Pod reaches SSH through a
// cluster-side forward and has no private/VPC pair to transform. Turning this into an
// error would break a product that works today.
func TestResolveHostReturnsNothingWhenThereIsNoPrivateEndpoint(t *testing.T) {
	url, sent := uvpcStub(t, "2003:da8::1")
	inst := liveInstance()
	inst["IPSet"] = []any{
		map[string]any{"IP": "203.0.113.10", "IPMode": "IPv4", "Type": "BGP", "Weight": float64(50)},
	}

	host, err := NewInstanceIPv6Resolver(url, &zoneStub{}).ResolveHost(context.Background(), inst)
	if err != nil {
		t.Fatalf("no private endpoint must not be an error: %v", err)
	}
	if host != "" {
		t.Fatalf("got host %q, want the caller to keep the advertised address", host)
	}
	if len(*sent) != 0 {
		t.Errorf("the backend must not be called with nothing to map, sent %#v", *sent)
	}
}

// A private entry without a VPCId is unmappable too — the transform is defined on the
// pair, and sending one half would ask the backend to guess.
func TestResolveHostReturnsNothingWhenThePrivateEntryHasNoVPC(t *testing.T) {
	url, _ := uvpcStub(t, "2003:da8::1")
	inst := liveInstance()
	inst["IPSet"] = []any{
		map[string]any{"Default": "true", "IP": "10.23.55.112", "Type": "Private", "VPCId": ""},
	}

	host, err := NewInstanceIPv6Resolver(url, &zoneStub{}).ResolveHost(context.Background(), inst)
	if err != nil || host != "" {
		t.Fatalf("host=%q err=%v, want the advertised address kept", host, err)
	}
}

// The region table is a property of the platform, not of the diagnosis, so it is read
// once. Re-reading it would put an extra upstream call in front of every SSH — the
// lane already has a rate limit and a wall clock to live inside.
func TestResolveHostReadsTheRegionTableOnlyOnce(t *testing.T) {
	url, _ := uvpcStub(t, "2003:da8::1")
	zones := &zoneStub{}
	r := NewInstanceIPv6Resolver(url, zones)

	for i := 0; i < 3; i++ {
		if _, err := r.ResolveHost(context.Background(), liveInstance()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if zones.calls != 1 {
		t.Fatalf("support zones read %d times, want 1", zones.calls)
	}
}

func TestResolveHostFailsWhenTheRegionIsUnknown(t *testing.T) {
	url, _ := uvpcStub(t, "2003:da8::1")
	inst := liveInstance()
	inst["Region"] = "cn-nowhere"

	_, err := NewInstanceIPv6Resolver(url, &zoneStub{}).ResolveHost(context.Background(), inst)
	if err == nil {
		t.Fatal("an unmappable region must be an error, not a silent fallback to the public address")
	}
	if !strings.Contains(err.Error(), "cn-nowhere") {
		t.Errorf("the error must name the region: %v", err)
	}
}

func TestResolveHostFailsWhenTheRegionLookupFails(t *testing.T) {
	url, _ := uvpcStub(t, "2003:da8::1")
	zones := &zoneStub{err: fmt.Errorf("sts expired")}

	_, err := NewInstanceIPv6Resolver(url, zones).ResolveHost(context.Background(), liveInstance())
	if err == nil || !strings.Contains(err.Error(), "sts expired") {
		t.Fatalf("the underlying cause must survive so the log names the layer: %v", err)
	}
}
