// Package sshops implements the consent-gated, read-only in-instance SSH diagnostics lane:
// it fetches an instance's SSH credential out-of-band and hands it to a spawned Agent-SDK
// harness over a stdin handshake. The credential never enters an LLM prompt, a trace, the DB,
// a reply, argv, or a log — see DESIGN-production.md (vendored under deploy/ssh_ops_harness/).
package sshops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// ErrNoSSHTarget marks an instance that has no SSH entrypoint at all: its describe
// carries an empty SshLoginCommand. Confirmed live against a CompShare Windows GPU
// instance (OsType=WINDOWS, SshLoginCommand=""); any image without SSH lands here
// too. It is NON-retryable — the box can never be entered — so callers distinguish
// it from a transient describe failure and tell the user honestly instead of
// "please retry". FetchCredential returns it wrapped; match with errors.Is.
var ErrNoSSHTarget = errors.New("sshops: instance has no SSH target")

// ErrInstanceNotRunning marks an instance that exists and may well have an SSH
// entrypoint, but is not in Running state — ImageMaking, Stopped, Starting. The
// box cannot be entered NOW and the reason is knowable, so callers name the state
// instead of falling back to "请稍后重试，或到控制台查看实例状态", which tells the
// user nothing they did not already know.
//
// Found by a live probe: uhost-…9dd126 sat in ImageMaking with no public address,
// so the SSH dial failed and the whole lane collapsed into that generic line —
// while the read-only instance_access capability, reading the SAME describe
// response, correctly reported 「实例当前状态为 ImageMaking，云侧预检不能确认」.
// The state was one field away the whole time.
//
// The error text carries the raw upstream state verbatim; nothing here translates
// or enumerates states, because only Running/Stopped are attested in this repo and
// ImageMaking was learned from a live response. Callers show it as-is.
var ErrInstanceNotRunning = errors.New("sshops: instance is not running")

// ErrInstanceNotFound marks an instance the describe response does not contain,
// even though the response itself was well-formed and held other instances. The
// id is not in this tenant's account: released, deleted, or simply mistyped.
// Retrying the same id can never succeed, so callers say that instead of the
// generic 「请稍后重试，或到控制台查看实例状态」.
//
// This is the third time this lane has had to learn the same lesson (see
// ErrNoSSHTarget and ErrInstanceNotRunning above), and the case that forced it
// is the most common one of the three: on 2026-08-06 a test account replaced 7
// of its 10 instances within an hour, so a diagnosis launched against an id read
// minutes earlier failed with "please retry" — advice that could not work — and
// the real cause (the box was gone) took an hour to find because nothing said it.
//
// Deliberately NOT used when the response carried no instances at all: an empty
// response can also mean a partial upstream failure, where retrying is genuinely
// the right advice.
var ErrInstanceNotFound = errors.New("sshops: instance not found in this account")

// NotRunningError carries the raw upstream state so a caller can name it without
// parsing an error string. errors.Is(err, ErrInstanceNotRunning) matches it;
// errors.As recovers the state itself.
type NotRunningError struct {
	InstanceID string
	State      string
}

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("%s: instance %s is in state %s", ErrInstanceNotRunning.Error(), e.InstanceID, e.State)
}

// Is makes the typed error satisfy errors.Is(err, ErrInstanceNotRunning) without
// a wrapping Errorf, so the state stays a field instead of becoming text to re-parse.
func (e *NotRunningError) Is(target error) bool { return target == ErrInstanceNotRunning }

// Describer is the narrow slice of *tools.ExternalExecutor the credential fetch needs. Kept as
// an interface so this package does not import internal/tools (no cycle) and tests can stub the
// upstream call.
type Describer interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

// ErrInternalAddressUnavailable marks a failed address rewrite: the lane was configured to
// reach instances at their internal address and could not work out what that address is.
// The box may be perfectly healthy — the failure is the gateway, its configuration, or the
// region lookup — so callers say so instead of emitting the generic "please retry, or check
// the instance in the console", which points the user at something that is not wrong.
//
// It also keeps the first production run of the internal route self-diagnosing: "could not
// derive the address" and "derived it and the dial failed" are different layers, and only
// the second one is about the network to the instance.
var ErrInternalAddressUnavailable = errors.New("sshops: internal address unavailable")

// HostResolver rewrites the address an instance is dialled at, given the instance's own
// describe payload. It exists because SshLoginCommand advertises the public EIP — right
// for the customer's laptop, unreachable from inside the UCloud private network where the
// server runs — and the platform's own control plane reaches the same boxes over an
// internal IPv6 instead (see tools.InstanceIPv6Resolver).
//
// The contract has three outcomes and they are deliberately distinct:
//
//	("", nil)   this instance has nothing to rewrite — keep the advertised address
//	(addr, nil) dial addr instead
//	("", err)   the rewrite should have worked and did not — REFUSE, do not fall back
//
// The third case is not pedantry. Falling back would dial the exact route the operator
// configured this resolver because it does not work, and the resulting timeout would be
// reported as the instance's firewall blocking SSH: the wrong-layer diagnosis this lane
// has already shipped twice (#516, #522).
type HostResolver interface {
	ResolveHost(ctx context.Context, instance map[string]any) (string, error)
}

// Credential is a resolved SSH target. Password is the decoded plaintext root password — the
// crown-jewel secret. The type is deliberately NON-SERIALIZABLE: String/GoString/MarshalJSON all
// return a redacted form, so an accidental log / fmt %v / json.Marshal (a trace field, a DB row,
// an SSE frame) can never leak it. The plaintext reaches ONLY the SSH transport, via the harness
// stdin handshake built in this package — never a tool arg, result, trace, or log.
type Credential struct {
	InstanceID string
	Host       string
	User       string
	Port       int
	password   string // unexported; only the in-package handshake reads it
}

// String / GoString / MarshalJSON are the leak-proofing: every stringification redacts.
func (c Credential) String() string   { return c.redacted() }
func (c Credential) GoString() string { return c.redacted() }

func (c Credential) redacted() string {
	return fmt.Sprintf("Credential{InstanceID:%s Host:%s User:%s Port:%d Password:[REDACTED]}",
		c.InstanceID, c.Host, c.User, c.Port)
}

// MarshalJSON refuses to serialize the password.
func (c Credential) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"instance_id":%q,"host":%q,"user":%q,"port":%d,"password":"[REDACTED]"}`,
		c.InstanceID, c.Host, c.User, c.Port)), nil
}

// HasSecret reports whether a usable password was resolved (without exposing it).
func (c Credential) HasSecret() bool { return c.password != "" }

// FetchCredential calls DescribeCompShareInstance out-of-band and resolves the SSH target for
// instanceID. It reads the raw (un-redacted) Password + SshLoginCommand straight off the upstream
// response map — a trusted in-process consumer, like the snapshot parser. The normal LLM-callable
// describe path is untouched and still redacts these fields; this is a separate caller whose result
// is piped only into the SSH transport. On any error nothing is partially exposed.
func FetchCredential(ctx context.Context, d Describer, instanceID string) (Credential, error) {
	return FetchCredentialWithHostResolver(ctx, d, instanceID, nil)
}

// FetchCredentialWithHostResolver is FetchCredential with an optional address rewrite:
// hr decides where the instance is dialled, while everything else — which instance was
// resolved, its state, its user, its port, its password — is read from the same describe
// response exactly as before. A nil hr is byte-identical to FetchCredential.
//
// Only the HOST moves. The user and port keep coming from SshLoginCommand because they
// are properties of the image, not of the route: a container image answers on 23 as root
// over either address (the container runs with --net host, so its sshd is bound on the
// machine's own stack — the same reason EIP:23 works today), and a plain VM answers on 22
// as ubuntu over either address.
func FetchCredentialWithHostResolver(ctx context.Context, d Describer, instanceID string, hr HostResolver) (Credential, error) {
	return FetchCredentialWithDialPolicy(ctx, d, instanceID, hr, dialPolicy{})
}

// dialPolicy carries the settings that decide WHICH address is dialled, beyond the resolver
// itself. Its zero value is "internal rewrite only" — byte-identical to the behaviour before
// public-IPv6 candidates existed, which is what every existing caller and test gets.
type dialPolicy struct {
	// PublicIPv6Prefix, when set, adds translation-prefix candidates derived from the
	// instance's PUBLIC IPv4 after the internal address. Empty (the default) means the
	// candidate machinery does not run at all — no extra probe, no extra log line.
	PublicIPv6Prefix string
}

// FetchCredentialWithDialPolicy is FetchCredentialWithHostResolver with an explicit address
// policy. It exists so a deployment can ask the lane to TRY a second addressing scheme without
// giving up the first one: the internal address stays the leading candidate, so a zone that
// works today picks the same address it picks today.
func FetchCredentialWithDialPolicy(ctx context.Context, d Describer, instanceID string, hr HostResolver, pol dialPolicy) (Credential, error) {
	cred, _, err := fetchCredentialWithDialPolicy(ctx, d, instanceID, hr, pol)
	return cred, err
}

// fetchCredentialWithDialPolicy is the credential boundary's internal form. In
// addition to the credential it returns the single resolved Describe row so the
// caller can build a separately allowlisted context projection without issuing
// a second Describe request. The raw map is package-private and must never cross
// into the harness or engine: it contains the login command and password.
func fetchCredentialWithDialPolicy(ctx context.Context, d Describer, instanceID string, hr HostResolver, pol dialPolicy) (Credential, map[string]any, error) {
	if strings.TrimSpace(instanceID) == "" {
		return Credential{}, nil, fmt.Errorf("sshops: empty instance id")
	}
	raw, err := d.Execute(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds.0": instanceID})
	if err != nil {
		return Credential{}, nil, fmt.Errorf("sshops: describe instance: %w", err)
	}
	inst, resolvedID, err := resolveInstance(raw, instanceID)
	if err != nil {
		return Credential{}, nil, err
	}
	// State is checked BEFORE SshLoginCommand on purpose. A stopped Linux box can
	// also come back with an empty SshLoginCommand, and telling that user
	// 「没有 SSH 登录入口（如 Windows 实例）」 would be actively wrong. Not-running is
	// the more accurate and more common explanation; a Running instance with no
	// entrypoint still falls through to the structural refusal below.
	stateValue, _ := inst["State"].(string)
	if state := strings.TrimSpace(stateValue); state != "" && !strings.EqualFold(state, "Running") {
		return Credential{}, nil, &NotRunningError{InstanceID: resolvedID, State: state}
	}
	loginCmd, _ := inst["SshLoginCommand"].(string)
	if strings.TrimSpace(loginCmd) == "" {
		// Windows instances (and any without SSH) return an empty SshLoginCommand.
		// Wrap the sentinel so the engine surfaces an honest, non-retryable refusal
		// instead of the generic "please retry" text (see ErrNoSSHTarget).
		return Credential{}, nil, fmt.Errorf("%w: instance %s has no SshLoginCommand", ErrNoSSHTarget, instanceID)
	}
	host, user, port, err := parseSSHLoginCommand(loginCmd)
	if err != nil {
		return Credential{}, nil, err
	}
	if host, err = resolveDialHost(ctx, hr, inst, host, port, pol); err != nil {
		return Credential{}, nil, err
	}
	// Password is base64(plaintext root password) — must be decoded before use.
	encPw, _ := inst["Password"].(string)
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encPw))
	if err != nil || len(dec) == 0 {
		return Credential{}, nil, fmt.Errorf("sshops: instance %s password unavailable", instanceID)
	}
	// InstanceID is the id the credential was actually READ FROM, never the id
	// that was asked for. The caller re-checks the two are equal before showing
	// a consent card, so the box named on the card is the box entered.
	return Credential{InstanceID: resolvedID, Host: host, User: user, Port: port, password: string(dec)}, inst, nil
}

// resolveDialHost applies hr to the address SshLoginCommand advertised.
//
// The rewrite is attempted ONLY when that address is a bare IPv4 literal. That is the
// exact shape the two rewritable products use — `ssh ubuntu@<EIP>` for a VM and
// `ssh -p 23 root@<EIP>` for a container image — and it excludes the one product that
// must not be rewritten: a Pod advertises `<podId>.podtcp.compshare.cn` with a
// cluster-side forwarded port, so its port lives on the forwarder rather than on the
// box, and pointing that port at the box's own address would dial a port nothing serves.
// Gating on the literal keeps that decision in one readable condition instead of asking
// the resolver to recognise a product it cannot see.
func resolveDialHost(ctx context.Context, hr HostResolver, inst map[string]any, advertised string, port int, pol dialPolicy) (string, error) {
	if hr == nil {
		return advertised, nil
	}
	if ip := net.ParseIP(advertised); ip == nil || ip.To4() == nil {
		return advertised, nil
	}
	resolved, err := hr.ResolveHost(ctx, inst)
	if strings.TrimSpace(pol.PublicIPv6Prefix) != "" {
		return pickReachableDialHost(ctx, inst, advertised, port, pol.PublicIPv6Prefix, resolved, err)
	}
	if err != nil {
		// Deliberately fatal — see the HostResolver contract. The caller collapses this
		// into the lane's one "couldn't complete" sentence and logs it verbatim
		// (cmd/instance_ops.go), so the layer is named for the operator without the user
		// being told to retry against an address that cannot work.
		return "", fmt.Errorf("%w: %w", ErrInternalAddressUnavailable, err)
	}
	if strings.TrimSpace(resolved) == "" {
		return advertised, nil
	}
	// Record the address that was actually dialled. It is logged ONLY here, on the success path,
	// because the failure path already logs (cmd/instance_ops.go) and between them no one could see
	// the address at all: a rewrite that produced a real-but-unroutable address and one that produced
	// a wrong address both surfaced as the same silent timeout. Settling which it was then took a
	// day of indirect evidence, and it is the same line that will show a route fix took effect.
	//
	// Safe to log: an internal VPC fabric address is infrastructure, the same class of fact as
	// mysql.host_override in the shipped production config — not a credential, and it names no
	// tenant. The advertised host stays out so the pair cannot be used to map EIP -> internal.
	log.Printf("ssh-ops: dialling internal address %s for instance %s", resolved, instanceIDOf(inst))
	return resolved, nil
}

// instanceIDOf reports the instance id for a log line, or "?" when the describe map has none.
// Kept tiny and total: a missing id must never panic the dial path it is only annotating.
func instanceIDOf(inst map[string]any) string {
	for _, k := range []string{"UHostId", "InstanceId", "Id"} {
		if s, ok := inst[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "?"
}

// resolveInstance finds the requested instance in a describe response and fails
// when it is absent.
//
// It used to fall back to the first element when nothing matched. That made the
// id on the consent card, the id SSH'd into, and the id written to the audit row
// three independent things: the request filters by UHostIds.0, but if upstream
// ever ignores or mis-parses that filter and returns the caller's whole list,
// the fallback silently picks a different box while the caller stamps the
// requested id onto the result. Consent and audit both break, inside one tenant,
// where credential scoping cannot help. There is no case where entering an
// unverified box is better than refusing, so the fallback is gone.
//
// The resolved id is returned rather than echoed by the caller so that "which
// box did we read" has exactly one producer.
func resolveInstance(raw map[string]any, instanceID string) (map[string]any, string, error) {
	seen := 0
	for _, key := range []string{"UHostSet", "UHostInstanceSet", "Instances", "DataSet"} {
		arr, ok := raw[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		seen += len(arr)
		for _, it := range arr {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if id, matched := matchedID(m, instanceID); matched {
				return m, id, nil
			}
		}
	}
	if seen > 0 {
		return nil, "", fmt.Errorf("%w: instance %s not present in describe response (%d returned)",
			ErrInstanceNotFound, instanceID, seen)
	}
	return nil, "", fmt.Errorf("sshops: no instance in describe response")
}

// matchedID reports whether m identifies instanceID, returning the id field that
// matched so the caller can carry the response's own spelling forward.
func matchedID(m map[string]any, id string) (string, bool) {
	for _, k := range []string{"UHostId", "InstanceId", "InstanceID", "ResourceId", "Id"} {
		if v, _ := m[k].(string); v != "" && v == id {
			return v, true
		}
	}
	return "", false
}

// sshCmdRe parses the three SshLoginCommand shapes:
//
//	ssh ubuntu@<ip>                                    (ucloud VM:        user=ubuntu port=22)
//	ssh -p 23 root@<ip>                                (ucloud container: user=root   port=23)
//	ssh -p <ExternalPort> root@<podId>.podtcp.compshare.cn (pod: user=root port=dynamic host=DNS)
var sshCmdRe = regexp.MustCompile(`\bssh\s+(?:-p\s+(\d+)\s+)?([A-Za-z0-9._-]+)@([A-Za-z0-9._-]+)`)

func parseSSHLoginCommand(s string) (host, user string, port int, err error) {
	m := sshCmdRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0, fmt.Errorf("sshops: unparseable SshLoginCommand")
	}
	port = 22
	if m[1] != "" {
		if p, e := strconv.Atoi(m[1]); e == nil {
			port = p
		}
	}
	user, host = m[2], m[3]
	if host == "" || user == "" {
		return "", "", 0, fmt.Errorf("sshops: SshLoginCommand missing host/user")
	}
	return host, user, port, nil
}
