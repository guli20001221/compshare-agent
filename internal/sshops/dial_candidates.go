package sshops

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// perCandidateDialTimeout bounds one candidate's TCP connect. Reachable
// production candidates have consistently connected well below this value.
const perCandidateDialTimeout = 4 * time.Second

// dialCandidate is one address the lane may dial and its routing scheme.
type dialCandidate struct {
	label string
	host  string
}

// publicIPv6Candidate expresses a public IPv4 in the low 32 bits of the
// configured translation prefix, matching the production pod ingress mapping.
func publicIPv6Candidate(prefix, ipv4 string) (dialCandidate, error) {
	p := net.ParseIP(strings.TrimSpace(prefix))
	if p == nil || p.To4() != nil {
		return dialCandidate{}, fmt.Errorf("sshops: public ipv6 prefix %q is not an IPv6 address", prefix)
	}
	v4 := net.ParseIP(strings.TrimSpace(ipv4))
	if v4 == nil || v4.To4() == nil {
		return dialCandidate{}, fmt.Errorf("sshops: %q is not an IPv4 address", ipv4)
	}
	base, b := p.To16(), v4.To4()

	simple := make(net.IP, net.IPv6len)
	copy(simple, base)
	for i := range 4 {
		simple[12+i] |= b[i]
	}
	return dialCandidate{label: "public-v6-simple", host: simple.String()}, nil
}

// firstReachable tries candidates in order. Each probe is the same TCP reachability
// prerequisite the SSH client needs; it writes nothing and closes immediately.
func firstReachable(ctx context.Context, cands []dialCandidate, port int) (dialCandidate, []string, bool) {
	trace := make([]string, 0, len(cands))
	for _, c := range cands {
		line, ok := probeCandidate(ctx, c, port)
		trace = append(trace, line)
		if ok {
			return c, trace, true
		}
		if ctx.Err() != nil {
			break
		}
	}
	return dialCandidate{}, trace, false
}

// probeCandidate is one TCP connect, rendered as the trace line the operator reads.
func probeCandidate(ctx context.Context, c dialCandidate, port int) (string, bool) {
	start := time.Now()
	d := net.Dialer{Timeout: perCandidateDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(port)))
	took := time.Since(start).Round(time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Sprintf("%s=[%s]:%d open in %s", c.label, c.host, port, took), true
	}
	return fmt.Sprintf("%s=[%s]:%d %s after %s (%v)",
		c.label, c.host, port, dialFailureClass(err), took, err), false
}

// dialCandidatesFor builds the ordered candidate list, plus notes explaining any candidate that
// could not be built. It is separate from the probing so the two safety properties — internal
// address FIRST, advertised public IPv4 NEVER — can be asserted without a network.
//
// A malformed configured prefix is an error rather than a silent downgrade.
func dialCandidatesFor(advertised, prefix, internal string, internalErr error) ([]dialCandidate, []string, error) {
	var cands []dialCandidate
	var notes []string
	switch {
	case internalErr != nil:
		notes = append(notes, fmt.Sprintf("internal-ipv6 unavailable (%v)", internalErr))
	case strings.TrimSpace(internal) == "":
		notes = append(notes, "internal-ipv6 produced no mapping for this instance")
	default:
		cands = append(cands, dialCandidate{label: "internal-ipv6", host: internal})
	}
	public, err := publicIPv6Candidate(prefix, advertised)
	if err != nil {
		return nil, notes, err
	}
	return append(cands, public), notes, nil
}

// pickReachableDialHost chooses among the addresses an instance could be dialled at, once a
// public-IPv6 translation prefix has been configured.
//
// Internal mapping is always tried before translation; the advertised EIP is
// never a candidate. Resolver failures are recorded while translation still
// gets a chance. If nothing answers, the lane refuses rather than guessing.
func pickReachableDialHost(ctx context.Context, inst map[string]any, advertised string, port int, prefix, internal string, internalErr error) (string, error) {
	id := instanceIDOf(inst)
	cands, notes, err := dialCandidatesFor(advertised, prefix, internal, internalErr)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInternalAddressUnavailable, err)
	}

	winner, trace, ok := firstReachable(ctx, cands, port)
	report := strings.Join(append(notes, trace...), "; ")
	log.Printf("ssh-ops: dial candidates for instance %s port %d: %s", id, port, report)
	if !ok {
		return "", fmt.Errorf("%w: no candidate address accepted a connection on port %d: %s",
			ErrSSHPreflightUnreachable, port, report)
	}
	log.Printf("ssh-ops: dialling %s address %s for instance %s", winner.label, winner.host, id)
	return winner.host, nil
}

// dialFailureClass keeps routing failures coarse and factual. The caller retains
// the original error alongside this label for operator diagnosis.
func dialFailureClass(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns-failed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "error"
}
