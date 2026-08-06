package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureUVPC serves one canned response and records the request body, so the wire
// shape is asserted against what the backend actually reads rather than against our
// own struct.
func captureUVPC(t *testing.T, body string) (*UVPCClient, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewUVPCClient(srv.URL), &got
}

// The field names are the backend's, not ours, and one of them is easy to get wrong:
// the address key is lowercase "ip" while its neighbours are PascalCase (see
// uhost-compshare-api internal/service/uvpc/type.go). A silent rename here would send a
// well-formed request that the backend answers with an empty address, which surfaces
// three layers later as an unexplained failed dial — so the spelling is pinned.
func TestTransformIPv4ToIPv6SendsTheBackendsFieldNames(t *testing.T) {
	c, got := captureUVPC(t, `{"RetCode":0,"IpV6":"2003:da8:2004:1000:a3c:7623:2712:f9c0"}`)

	addr, err := c.TransformIPv4ToIPv6(context.Background(), 1000009, "10.23.55.112", "uvnet-example00001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "2003:da8:2004:1000:a3c:7623:2712:f9c0" {
		t.Fatalf("got address %q", addr)
	}
	for key, want := range map[string]any{
		"Backend":  "UVPCFEGO",
		"Action":   "TransformIPv4ToIPv6",
		"RegionId": float64(1000009),
		"ip":       "10.23.55.112",
		"VPCId":    "uvnet-example00001",
	} {
		if (*got)[key] != want {
			t.Errorf("request[%q] = %#v, want %#v", key, (*got)[key], want)
		}
	}
}

func TestTransformIPv4ToIPv6FailsOnBackendRetCode(t *testing.T) {
	c, _ := captureUVPC(t, `{"RetCode":230,"Message":"vpc not found"}`)

	addr, err := c.TransformIPv4ToIPv6(context.Background(), 1000009, "10.23.55.112", "uvnet-gone")
	if err == nil {
		t.Fatalf("a non-zero RetCode must be an error, got address %q", addr)
	}
	if !strings.Contains(err.Error(), "230") || !strings.Contains(err.Error(), "vpc not found") {
		t.Errorf("the backend's own code and message must survive: %v", err)
	}
}

// A RetCode=0 with nothing usable in it is the shape that would otherwise escape as an
// empty host and fail at the dial as a bare connection error. It is caught here, where
// the cause is still nameable.
func TestTransformIPv4ToIPv6RejectsAnUnusableAddress(t *testing.T) {
	for name, body := range map[string]string{
		"empty":   `{"RetCode":0,"IpV6":""}`,
		"ipv4":    `{"RetCode":0,"IpV6":"10.23.55.112"}`,
		"garbage": `{"RetCode":0,"IpV6":"not-an-address"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := captureUVPC(t, body)
			if addr, err := c.TransformIPv4ToIPv6(context.Background(), 1000009, "10.23.55.112", "uvnet-example00001"); err == nil {
				t.Fatalf("expected an error, got address %q", addr)
			}
		})
	}
}

// The inputs are validated before the call so a missing region id or a public address
// cannot be sent upstream and come back as an opaque backend error.
func TestTransformIPv4ToIPv6ValidatesItsInputs(t *testing.T) {
	c, got := captureUVPC(t, `{"RetCode":0,"IpV6":"2003:da8::1"}`)
	cases := map[string]struct {
		region uint32
		ip     string
		vpc    string
	}{
		"no region id":  {0, "10.23.55.112", "uvnet-example00001"},
		"ipv6 as input": {1000009, "2003:da8::1", "uvnet-example00001"},
		"not an ip":     {1000009, "example.com", "uvnet-example00001"},
		"no vpc":        {1000009, "10.23.55.112", "  "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.TransformIPv4ToIPv6(context.Background(), tc.region, tc.ip, tc.vpc); err == nil {
				t.Fatal("expected an error")
			}
			if len(*got) != 0 {
				t.Fatalf("an invalid input must not reach the backend, sent %#v", *got)
			}
		})
	}
}
