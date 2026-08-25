package sshops

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPublicIPv6CandidateUsesTheProductionLow32BitEncoding(t *testing.T) {
	got, err := publicIPv6Candidate("2002:a40:2e05::", "203.0.113.9")
	if err != nil {
		t.Fatalf("build candidate: %v", err)
	}
	want := dialCandidate{label: "public-v6-simple", host: "2002:a40:2e05::cb00:7109"}
	if got != want {
		t.Errorf("candidate = %+v, want %+v", got, want)
	}
	if literal := net.ParseIP("2002:a40:2e05::203.0.113.9"); literal == nil || literal.String() != want.host {
		t.Errorf("the dotted-quad spelling parses to %v, want %s", literal, want.host)
	}
}

func TestPublicIPv6CandidatePreservesLongerConfiguredPrefix(t *testing.T) {
	got, err := publicIPv6Candidate("2002:a40:2e05:dead::", "203.0.113.9")
	if err != nil {
		t.Fatalf("build candidate: %v", err)
	}
	if got.host != "2002:a40:2e05:dead::cb00:7109" {
		t.Errorf("got %+v, want the configured prefix with the IPv4 in its low 32 bits", got)
	}
}

// Ordering is the safety property that lets this ship at all: a zone reached over the internal
// address today must keep dialling that same address, so the unproven prefix can only ever be
// reached by a zone the internal route has already failed on.
func TestInternalAddressIsAlwaysTheFirstCandidate(t *testing.T) {
	cands, _, err := dialCandidatesFor("203.0.113.9", "2002:a40:2e05::", "2003:da8:2004:1000::5", nil)
	if err != nil {
		t.Fatalf("build candidates: %v", err)
	}
	if len(cands) == 0 || cands[0].label != "internal-ipv6" {
		t.Fatalf("internal address must lead the list, got %+v", cands)
	}
}

// The advertised public IPv4 is the address this deployment provably cannot reach. Dialling it
// reports that fact as the customer's firewall, which is the wrong-layer diagnosis #516 and #522
// each removed once. It must not reappear as a "last resort" candidate.
func TestAdvertisedPublicIPv4IsNeverACandidate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		internal string
		err      error
	}{
		{"internal resolved", "2003:da8:2004:1000::5", nil},
		{"internal failed", "", errors.New("gateway unreachable")},
		{"internal has no mapping", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cands, _, err := dialCandidatesFor("203.0.113.9", "2002:a40:2e05::", tc.internal, tc.err)
			if err != nil {
				t.Fatalf("build candidates: %v", err)
			}
			for _, c := range cands {
				if c.host == "203.0.113.9" {
					t.Fatalf("the advertised EIP became a candidate: %+v", cands)
				}
			}
			if len(cands) == 0 {
				t.Fatal("no candidates at all — the translated address must always be offered")
			}
		})
	}
}

// A resolver failure is fatal on the internal-only path, and deliberately is not here: the whole
// point of a second scheme is to survive the first being unavailable. The reason still has to
// reach the log, or a run that fell through says nothing about why.
func TestResolverFailureStillTriesThePrefixAndRecordsWhy(t *testing.T) {
	cands, notes, err := dialCandidatesFor("203.0.113.9", "2002:a40:2e05::", "", errors.New("gateway timeout"))
	if err != nil {
		t.Fatalf("build candidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want the translated candidate, got %+v", cands)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "gateway timeout") {
		t.Errorf("the resolver failure must be recorded, got %v", notes)
	}
}

func TestMalformedPrefixIsAnErrorNotASilentDowngrade(t *testing.T) {
	for _, prefix := range []string{"not-an-address", "10.64.46.5", "2002:a40:2e05::/48"} {
		if _, _, err := dialCandidatesFor("203.0.113.9", prefix, "2003:da8::5", nil); err == nil {
			t.Errorf("prefix %q must be rejected, not ignored", prefix)
		}
	}
}

// With no prefix configured the dial path must be exactly what it was before candidates existed:
// a resolver error refuses, and nothing is probed. This is the assertion that lets every
// deployment that does not opt in stay untouched.
func TestEmptyPrefixKeepsTheInternalOnlyContract(t *testing.T) {
	boom := errors.New("region lookup failed")
	hr := hostResolverFunc(func(context.Context, map[string]any) (string, error) { return "", boom })

	_, err := resolveDialHost(context.Background(), hr, map[string]any{"UHostId": "uhost-x"},
		"203.0.113.9", 22, dialPolicy{})
	if !errors.Is(err, ErrInternalAddressUnavailable) || !errors.Is(err, boom) {
		t.Fatalf("want a refusal wrapping the resolver error, got %v", err)
	}
}

// A pod advertises a DNS host with a cluster-side forwarded port. It is excluded before any of
// this runs, and turning the prefix on must not change that: the prefix forms are derived from a
// bare IPv4 the pod does not have, and its port lives on the forwarder rather than on the box.
func TestPodHostIsStillNeverRewrittenWithAPrefixConfigured(t *testing.T) {
	hr := hostResolverFunc(func(context.Context, map[string]any) (string, error) {
		t.Fatal("the resolver must not be consulted for a pod")
		return "", nil
	})
	host, err := resolveDialHost(context.Background(), hr, map[string]any{"UHostId": "cpod-x"},
		"cpod-x-s1.podtcp.compshare.cn", 27802, dialPolicy{PublicIPv6Prefix: "2002:a40:2e05::"})
	if err != nil || host != "cpod-x-s1.podtcp.compshare.cn" {
		t.Fatalf("got (%q, %v), want the advertised host untouched", host, err)
	}
}

// Failed candidates retain their label and address so operators can distinguish
// routing failures without exposing credentials.
func TestProbeReportsTheAddressAndFailureClass(t *testing.T) {
	port := closedLocalPort(t)
	_, trace, ok := firstReachable(context.Background(),
		[]dialCandidate{{label: "internal-ipv6", host: "::1"}}, port)
	if ok {
		t.Fatal("nothing is listening; the probe must not report success")
	}
	if len(trace) != 1 || !strings.Contains(trace[0], "::1") || !strings.Contains(trace[0], "internal-ipv6") {
		t.Fatalf("trace must name the label and address, got %v", trace)
	}
}

func TestProbePicksTheFirstCandidateThatListens(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// Prefix "::" over 0.0.0.1 is exactly ::1, so the listener stands in for a reachable
	// translated address without needing a real translator.
	got, _, ok := firstReachable(context.Background(), []dialCandidate{
		{label: "internal-ipv6", host: "100::1"}, // RFC 6666 discard prefix — never answers
		{label: "public-v6-simple", host: "::1"},
	}, port)
	if !ok {
		t.Skip("the discard-prefix candidate did not fail fast on this host")
	}
	if got.host != "::1" || got.label != "public-v6-simple" {
		t.Fatalf("got %+v, want the listening candidate", got)
	}
}

// When nothing answers, refuse instead of returning an unverified address. This
// is a TCP preflight failure, not an address-derivation failure: candidates were
// successfully built and actually tried.
func TestNothingReachableReturnsSSHPreflightFailureAndNamesWhatWasTried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	host, err := pickReachableDialHost(ctx, map[string]any{"UHostId": "uhost-x"},
		"203.0.113.9", closedLocalPort(t), "::", "::1", nil)
	if err == nil {
		t.Fatalf("want a refusal, got host %q", host)
	}
	if !errors.Is(err, ErrSSHPreflightUnreachable) {
		t.Fatalf("want ErrSSHPreflightUnreachable, got %v", err)
	}
	if errors.Is(err, ErrInternalAddressUnavailable) {
		t.Fatalf("reachable candidates were derived, so this must not claim address derivation failed: %v", err)
	}
	if !strings.Contains(err.Error(), "::1") {
		t.Errorf("the refusal must name an address that was tried, got %v", err)
	}
}

// closedLocalPort returns a port on loopback with nothing listening, so a connect fails
// immediately instead of burning the candidate timeout.
func closedLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Skipf("cannot reserve a loopback port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

type hostResolverFunc func(context.Context, map[string]any) (string, error)

func (f hostResolverFunc) ResolveHost(ctx context.Context, inst map[string]any) (string, error) {
	return f(ctx, inst)
}
