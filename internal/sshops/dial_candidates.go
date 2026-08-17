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

// perCandidateDialTimeout bounds one candidate's TCP connect.
//
// 4s is chosen against measurement, not taste: the addresses that DO work connect in
// well under 1.2s from a network that has a route to them (three instances, two ports,
// two regions, 2026-08-06), so 4s is more than three times the observed headroom, while
// three candidates in the worst case (12s) still costs less than the 15.6-16.3s the
// lane used to burn on a single silent timeout before giving up.
const perCandidateDialTimeout = 4 * time.Second

// dialCandidate is one address the lane may dial, with the name of the scheme that produced it.
//
// The label is not decoration. More than one candidate exists precisely because nobody can
// tell us which addressing scheme this deployment supports, so a run that does not say WHICH
// address answered would leave the question exactly where it started — which is how the last
// three rounds of this investigation ended.
type dialCandidate struct {
	label string
	host  string
}

// publicIPv6Candidates expresses a public IPv4 as addresses under a translation prefix.
//
// "把前缀加在 IPv4 前面" names two different addresses, and they are not interchangeable:
//
//	simple   <prefix> | v4 in the low 32 bits            e.g. 2002:a40:2e05::203.0.113.9
//	rfc6052  <prefix/48> | v4[0:3] | u=0 | v4[3] | zero  RFC 6052 §2.2, the standard /48 form
//
// Both are generated so that a failed probe means "no translator" rather than the ambiguous
// "no translator, OR the right translator with the wrong encoding". The one real mapping in
// this tree (config.prod.yaml's mysql.host_override) puts its embedded IPv4 at bits 64-95,
// which is neither of these, so the encoding genuinely is an open question here.
func publicIPv6Candidates(prefix, ipv4 string) ([]dialCandidate, error) {
	p := net.ParseIP(strings.TrimSpace(prefix))
	if p == nil || p.To4() != nil {
		return nil, fmt.Errorf("sshops: public ipv6 prefix %q is not an IPv6 address", prefix)
	}
	v4 := net.ParseIP(strings.TrimSpace(ipv4))
	if v4 == nil || v4.To4() == nil {
		return nil, fmt.Errorf("sshops: %q is not an IPv4 address", ipv4)
	}
	base, b := p.To16(), v4.To4()

	simple := make(net.IP, net.IPv6len)
	copy(simple, base)
	for i := range 4 {
		simple[12+i] |= b[i]
	}
	out := []dialCandidate{{label: "public-v6-simple", host: simple.String()}}

	// RFC 6052 defines this layout only for a /48 prefix, so it is offered only when the
	// configured prefix really is one — every bit below /48 clear. Emitting it for a longer
	// prefix would overwrite prefix bits and dial an address nobody configured, which is a
	// worse outcome than not offering the encoding at all.
	if allZero(base[6:]) {
		rfc := make(net.IP, net.IPv6len)
		copy(rfc, base)
		copy(rfc[6:9], b[0:3])
		rfc[9] = 0
		rfc[10] = b[3]
		out = append(out, dialCandidate{label: "public-v6-rfc6052", host: rfc.String()})
	}
	return out, nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// firstReachable dials each candidate's port in order and returns the first that accepts a TCP
// connection, plus one trace line per candidate describing what happened to it.
//
// A probe is needed because the harness dials exactly ONCE: without it the lane would have to
// choose an address on faith, and a wrong choice is indistinguishable from an unreachable box —
// that is the 15.6-16.3s silent timeout which made every earlier failure unattributable. A TCP
// connect is the first thing paramiko does anyway, from this same host, so a candidate that
// passes here is one the harness can use.
//
// The connection is closed immediately and nothing is written to it: this reads reachability,
// it does not authenticate and it does not touch the instance.
func firstReachable(ctx context.Context, cands []dialCandidate, port int) (dialCandidate, int, []string, bool) {
	trace := make([]string, 0, len(cands))
	for i, c := range cands {
		line, ok := probeCandidate(ctx, c, port)
		trace = append(trace, line)
		if ok {
			return c, i, trace, true
		}
		if ctx.Err() != nil {
			break
		}
	}
	return dialCandidate{}, -1, trace, false
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

// probeRemaining reports how the candidates AFTER the winner would have fared, without
// affecting which address is dialled.
//
// Without it the experiment cannot be run where it gives a clean answer. On a zone the internal
// route already reaches, the prefix candidates are never dialled — first-reachable-wins stops at
// the first one — so a run there says nothing about the prefix. And that zone is precisely where
// the prefix CAN be judged: the box is known reachable from here, so a prefix failure there is
// the prefix's, whereas a failure on a zone with no route at all is ambiguous between "no
// translator" and "translator that also cannot reach that cluster".
//
// It runs detached and after the decision, so it costs the user nothing: the credential is
// already resolved by the time these fire, and the request's own cancellation must not kill them
// (the diagnosis moves on immediately). Bounded by its own deadline, read-only, and it only ever
// logs — nothing here can change which address the harness receives.
//
// The returned channel closes when the probes are done. Production ignores it; tests wait on it
// instead of sleeping.
func probeRemaining(ctx context.Context, cands []dialCandidate, port int, instanceID string) <-chan struct{} {
	done := make(chan struct{})
	if len(cands) == 0 {
		close(done)
		return done
	}
	// Detached from the request on purpose — see above. The budget is the same per-candidate
	// timeout the foreground uses, so a hung probe cannot outlive the run by an unbounded margin.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(len(cands))*perCandidateDialTimeout+time.Second)
	go func() {
		defer close(done)
		defer cancel()
		lines := make([]string, 0, len(cands))
		for _, c := range cands {
			line, _ := probeCandidate(probeCtx, c, port)
			lines = append(lines, line)
			if probeCtx.Err() != nil {
				break
			}
		}
		log.Printf("ssh-ops: candidates NOT dialled for instance %s port %d (diagnostic only): %s",
			instanceID, port, strings.Join(lines, "; "))
	}()
	return done
}

// dialCandidatesFor builds the ordered candidate list, plus notes explaining any candidate that
// could not be built. It is separate from the probing so the two safety properties — internal
// address FIRST, advertised public IPv4 NEVER — can be asserted without a network.
//
// A malformed prefix is an error rather than a silent "internal only": the operator set it
// because they want the second scheme tried, and a lane that quietly ignored it would answer
// the experiment's question with the wrong run.
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
	public, err := publicIPv6Candidates(prefix, advertised)
	if err != nil {
		return nil, notes, err
	}
	return append(cands, public...), notes, nil
}

// diagnosticProbes lists the addresses probed for INFORMATION only, after the dial is decided.
//
// Two things go in, for two different reasons:
//
//   - the candidates the winner cut short. On a zone the internal route reaches, this is the only
//     way the prefix is ever exercised — and that is the zone that can judge it. This is what
//     established (in-cluster, 2026-08-16) that public-v6-rfc6052 answers nowhere while
//     public-v6-simple answers on a box the internal route already reached.
//   - the advertised public EIP. It is never a dial candidate and must never become one, but
//     "this deployment has no route to customer EIPs" is the single most load-bearing premise in
//     the whole design, and an instance fleet turns over fast enough that any single measurement
//     of it is stale within the day (re-measured 2026-08-16: timeout on every target in both
//     regions). Re-measuring it on every run costs nothing and means a network change that opened
//     that route would show up as a line in the log, instead of us going on routing around a
//     problem that had already been fixed.
//
// The separation is structural, not a convention: this list is built AFTER the address is chosen
// and its only consumer is probeRemaining, which logs. dialCandidatesFor — the function that
// decides what gets dialled — never sees it, and has its own test that the EIP is absent.
func diagnosticProbes(cands []dialCandidate, winnerIdx int, advertised string) []dialCandidate {
	out := make([]dialCandidate, 0, len(cands)-winnerIdx)
	out = append(out, cands[winnerIdx+1:]...)
	return append(out, dialCandidate{label: "advertised-eip-CONTROL-never-dialled", host: advertised})
}

// pickReachableDialHost chooses among the addresses an instance could be dialled at, once a
// public-IPv6 translation prefix has been configured.
//
// Order is the safety property. The internal address goes FIRST, so every zone that works today
// keeps dialling exactly the address it dials today, and the unproven prefix can only ever be
// reached by a zone the internal route has already failed on. The advertised public EIP is never
// a candidate in any position: this deployment sits on the side of the network that cannot reach
// it, and dialling it would report that fact as the customer's firewall (#516, #522).
//
// A resolver error is NOT fatal here, unlike the internal-only path. The entire reason a second
// scheme exists is to survive the first being unavailable, so the error is recorded in the trace
// instead — a run that fell through to the prefix says why it did.
//
// When nothing answers this REFUSES rather than picking one hopefully. A refusal that names every
// address tried is the result the experiment needs; a hopeful pick would be another 16-second
// silent timeout that settles nothing.
func pickReachableDialHost(ctx context.Context, inst map[string]any, advertised string, port int, prefix, internal string, internalErr error) (string, error) {
	id := instanceIDOf(inst)
	cands, notes, err := dialCandidatesFor(advertised, prefix, internal, internalErr)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInternalAddressUnavailable, err)
	}

	winner, idx, trace, ok := firstReachable(ctx, cands, port)
	report := strings.Join(append(notes, trace...), "; ")
	log.Printf("ssh-ops: dial candidates for instance %s port %d: %s", id, port, report)
	if !ok {
		return "", fmt.Errorf("%w: no candidate address accepted a connection on port %d: %s",
			ErrInternalAddressUnavailable, port, report)
	}
	probeRemaining(ctx, diagnosticProbes(cands, idx, advertised), port, id)
	// Safe to log: these are infrastructure addresses, the same class of fact as
	// mysql.host_override in the shipped production config, and no tenant is named beyond the
	// instance id the audit row already carries. The diagnostic line above now also prints the
	// advertised EIP, so the two together do pair EIP with internal address — acceptable because
	// anyone who can read these logs has the instance id and can get both from one Describe.
	log.Printf("ssh-ops: dialling %s address %s for instance %s", winner.label, winner.host, id)
	return winner.host, nil
}

// dialFailureClass names the failure in the vocabulary this investigation actually uses:
// a timeout means the SYN went nowhere (no route, or dropped in flight), and a DNS error
// means the name never resolved. Anything else stays "error" AND the raw error is kept
// alongside the class by the caller — collapsing distinct causes into one confident label
// is what made the old catch-all assert 「安全组未放通」 for every non-auth exception (#516).
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
