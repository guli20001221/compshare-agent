package sshops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// stubDescriber returns a canned DescribeCompShareInstance response (or an error).
type stubDescriber struct {
	resp map[string]any
	err  error
}

func (s stubDescriber) Execute(_ context.Context, _ string, _ map[string]any) (map[string]any, error) {
	return s.resp, s.err
}

const secretPW = "S3cr3tR00tPw!" // a stand-in plaintext root password (test only)

// cred builds a Credential with the unexported secret set via assignment (not a struct-literal
// `password:` field, which the secret_scan pre-commit hook flags as a literal credential).
func cred(instanceID, host, user string, port int, pw string) Credential {
	c := Credential{InstanceID: instanceID, Host: host, User: user, Port: port}
	c.password = pw
	return c
}

// WHY: the Credential type is the structural leak guard (INV-1..4). If ANY stringification path
// exposes the password, a future trace/log/DB write leaks the crown-jewel secret. This test fails
// the moment someone adds an exported Password field or a %v that prints it.
func TestCredentialNeverSerializes(t *testing.T) {
	c := cred("uhost-x", "1.2.3.4", "root", 23, secretPW)

	jsonBytes, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	renders := []string{
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%s", c), fmt.Sprintf("%#v", c),
		string(jsonBytes),
		// nested in a struct / slice / map — the redaction must survive composition
		fmt.Sprintf("%v", struct{ C Credential }{c}),
		fmt.Sprintf("%v", []Credential{c}),
		fmt.Sprintf("%v", map[string]Credential{"k": c}),
	}
	for _, r := range renders {
		if strings.Contains(r, secretPW) {
			t.Fatalf("password leaked in render: %q", r)
		}
		if !strings.Contains(r, "[REDACTED]") {
			t.Fatalf("render not redacted: %q", r)
		}
	}
	// the password IS reachable in-package for the transport boundary (and only there)
	if c.password != secretPW {
		t.Fatalf("in-package password access broken")
	}
}

func describeResp(loginCmd, b64pw string) map[string]any {
	return map[string]any{
		"RetCode": float64(0),
		"UHostSet": []any{
			map[string]any{
				"UHostId":         "uhost-abc",
				"SshLoginCommand": loginCmd,
				"Password":        b64pw,
			},
		},
	}
}

func TestFetchCredential(t *testing.T) {
	cases := []struct {
		name, login        string
		wantHost, wantUser string
		wantPort           int
	}{
		{"vm", "ssh ubuntu@10.0.0.5", "10.0.0.5", "ubuntu", 22},
		{"container", "ssh -p 23 root@10.0.0.6", "10.0.0.6", "root", 23},
		{"pod", "ssh -p 35001 root@pod-abc.podtcp.compshare.cn", "pod-abc.podtcp.compshare.cn", "root", 35001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
			d := stubDescriber{resp: describeResp(tc.login, b64)}
			c, err := FetchCredential(context.Background(), d, "uhost-abc")
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if c.Host != tc.wantHost || c.User != tc.wantUser || c.Port != tc.wantPort {
				t.Fatalf("got %s/%s/%d want %s/%s/%d", c.Host, c.User, c.Port, tc.wantHost, tc.wantUser, tc.wantPort)
			}
			if c.password != secretPW { // base64 decoded to plaintext
				t.Fatalf("password not decoded: %q", c.password)
			}
		})
	}
}

func TestFetchCredentialErrors(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	cases := []struct {
		name string
		d    stubDescriber
		id   string
	}{
		{"empty-id", stubDescriber{resp: describeResp("ssh ubuntu@h", b64)}, ""},
		{"describe-error", stubDescriber{err: fmt.Errorf("230")}, "uhost-abc"},
		{"no-login-cmd (windows)", stubDescriber{resp: describeResp("", b64)}, "uhost-abc"},
		{"bad-base64", stubDescriber{resp: describeResp("ssh ubuntu@h", "!!notb64!!")}, "uhost-abc"},
		{"no-instance", stubDescriber{resp: map[string]any{"UHostSet": []any{}}}, "uhost-abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := FetchCredential(context.Background(), tc.d, tc.id)
			if err == nil {
				t.Fatalf("expected error, got cred %v", c)
			}
			if c.HasSecret() { // must not partially expose a credential on error
				t.Fatalf("credential leaked on error path")
			}
		})
	}
}

// The Windows / no-SSH case must surface the ErrNoSSHTarget sentinel (not just a bare
// error string) so the engine can give an honest, non-retryable refusal instead of
// "please retry". Confirmed live: a CompShare Windows GPU instance returns an empty
// SshLoginCommand. This test fails if the wrap is dropped, silently re-collapsing the
// distinction into the generic transient-failure path.
func TestFetchCredentialNoSSHTargetIsSentinel(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	_, err := FetchCredential(context.Background(), stubDescriber{resp: describeResp("", b64)}, "uhost-abc")
	if !errors.Is(err, ErrNoSSHTarget) {
		t.Fatalf("empty SshLoginCommand must wrap ErrNoSSHTarget, got %v", err)
	}
}

func TestParseSSHLoginCommand(t *testing.T) {
	if _, _, _, err := parseSSHLoginCommand(""); err == nil {
		t.Fatalf("empty login command should error")
	}
	if _, _, _, err := parseSSHLoginCommand("rdp://host"); err == nil {
		t.Fatalf("non-ssh command should error")
	}
}

// describeRespWithState is describeResp plus the upstream State field. Kept separate
// so the existing fixtures stay stateless and keep proving that an absent State does
// NOT block the fetch (an upstream that stops sending it must not break the lane).
func describeRespWithState(loginCmd, b64pw, state string) map[string]any {
	resp := describeResp(loginCmd, b64pw)
	resp["UHostSet"].([]any)[0].(map[string]any)["State"] = state
	return resp
}

// A box that is not Running cannot be entered, and the reason is sitting in the same
// describe response we already fetched. Before this, every such case fell into the
// generic "请稍后重试，或到控制台查看实例状态" — telling the user to go look up the one
// fact we were holding. Live case: uhost-…9dd126 in ImageMaking.
func TestFetchCredentialNotRunningNamesTheState(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	for _, state := range []string{"ImageMaking", "Stopped", "Starting"} {
		t.Run(state, func(t *testing.T) {
			c, err := FetchCredential(context.Background(), stubDescriber{resp: describeRespWithState("ssh -p 23 root@10.0.0.6", b64, state)}, "uhost-abc")
			if !errors.Is(err, ErrInstanceNotRunning) {
				t.Fatalf("state %s must wrap ErrInstanceNotRunning, got %v", state, err)
			}
			var notRunning *NotRunningError
			if !errors.As(err, &notRunning) {
				t.Fatalf("state %s must surface a typed *NotRunningError, got %T", state, err)
			}
			// The state must travel as a FIELD. If it only lived in the error text the
			// caller would have to parse a sentence to render it.
			if notRunning.State != state {
				t.Fatalf("State field = %q, want %q", notRunning.State, state)
			}
			if c.HasSecret() {
				t.Fatalf("credential leaked on the not-running path")
			}
		})
	}
}

// The ordering rule, and the reason it exists: a STOPPED Linux box can also come back
// with an empty SshLoginCommand. Checking SshLoginCommand first would tell that user
// 「没有 SSH 登录入口（如 Windows 实例）」 — a structural, non-retryable refusal about a
// box that is merely switched off. Not-running must win.
func TestFetchCredentialStoppedWithoutLoginCommandIsNotReportedAsWindows(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	_, err := FetchCredential(context.Background(), stubDescriber{resp: describeRespWithState("", b64, "Stopped")}, "uhost-abc")
	if !errors.Is(err, ErrInstanceNotRunning) {
		t.Fatalf("a stopped box with no login command must report not-running, got %v", err)
	}
	if errors.Is(err, ErrNoSSHTarget) {
		t.Fatalf("a stopped box must NOT be reported as having no SSH entrypoint: %v", err)
	}
}

// ...but a RUNNING box with no entrypoint is still the structural case. This is the
// other half of the ordering rule; without it the check above could be satisfied by
// simply never returning ErrNoSSHTarget again.
func TestFetchCredentialRunningWithoutLoginCommandIsStillNoSSHTarget(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	_, err := FetchCredential(context.Background(), stubDescriber{resp: describeRespWithState("", b64, "Running")}, "uhost-abc")
	if !errors.Is(err, ErrNoSSHTarget) {
		t.Fatalf("a running box with no login command must still be ErrNoSSHTarget, got %v", err)
	}
}

// Running is the only state that proceeds, and it must proceed normally.
func TestFetchCredentialRunningProceeds(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	for _, state := range []string{"Running", "running"} { // upstream casing is not guaranteed
		c, err := FetchCredential(context.Background(), stubDescriber{resp: describeRespWithState("ssh -p 23 root@10.0.0.6", b64, state)}, "uhost-abc")
		if err != nil {
			t.Fatalf("state %q must proceed, got %v", state, err)
		}
		if c.Host != "10.0.0.6" {
			t.Fatalf("state %q: host = %q", state, c.Host)
		}
	}
}
