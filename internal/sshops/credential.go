// Package sshops implements the consent-gated, read-only in-instance SSH diagnostics lane:
// it fetches an instance's SSH credential out-of-band and hands it to a spawned Agent-SDK
// harness over a stdin handshake. The credential never enters an LLM prompt, a trace, the DB,
// a reply, argv, or a log — see DESIGN-production.md (vendored under deploy/ssh_ops_harness/).
package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Describer is the narrow slice of *tools.ExternalExecutor the credential fetch needs. Kept as
// an interface so this package does not import internal/tools (no cycle) and tests can stub the
// upstream call.
type Describer interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
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
	if strings.TrimSpace(instanceID) == "" {
		return Credential{}, fmt.Errorf("sshops: empty instance id")
	}
	raw, err := d.Execute(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds.0": instanceID})
	if err != nil {
		return Credential{}, fmt.Errorf("sshops: describe instance: %w", err)
	}
	inst, resolvedID, err := resolveInstance(raw, instanceID)
	if err != nil {
		return Credential{}, err
	}
	loginCmd, _ := inst["SshLoginCommand"].(string)
	if strings.TrimSpace(loginCmd) == "" {
		// Windows instances (and any without SSH) return an empty SshLoginCommand.
		return Credential{}, fmt.Errorf("sshops: instance %s has no SshLoginCommand (no SSH target)", instanceID)
	}
	host, user, port, err := parseSSHLoginCommand(loginCmd)
	if err != nil {
		return Credential{}, err
	}
	// Password is base64(plaintext root password) — must be decoded before use.
	encPw, _ := inst["Password"].(string)
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encPw))
	if err != nil || len(dec) == 0 {
		return Credential{}, fmt.Errorf("sshops: instance %s password unavailable", instanceID)
	}
	// InstanceID is the id the credential was actually READ FROM, never the id
	// that was asked for. The caller re-checks the two are equal before showing
	// a consent card, so the box named on the card is the box entered.
	return Credential{InstanceID: resolvedID, Host: host, User: user, Port: port, password: string(dec)}, nil
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
		return nil, "", fmt.Errorf("sshops: instance %s not present in describe response (%d returned)", instanceID, seen)
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
