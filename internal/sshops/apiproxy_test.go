package sshops

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// recordingDescriber records every Execute call so tests can assert the deny-by-default and
// token gates NEVER reach the upstream executor.
type recordingDescriber struct {
	resp       map[string]any
	err        error
	calls      int
	lastAction string
	lastParams map[string]any
}

func (d *recordingDescriber) Execute(_ context.Context, action string, params map[string]any) (map[string]any, error) {
	d.calls++
	d.lastAction = action
	d.lastParams = params
	return d.resp, d.err
}

func postAPIRead(t *testing.T, url, token, action string, params map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(apiReadRequest{Action: action, Params: params})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded) // error bodies are plain text; decoded stays nil then
	return resp, decoded
}

// WHY: the proxy is the ONLY door between a third-party model and signed tenant API reads. If an
// allowlisted action ever returned an UN-sanitized body, the base64 SSH password (key "Password")
// would flow to the model. This asserts the happy path returns data AND that the credential field
// is stripped by the shared sanitizer before it leaves the process.
func TestAPIProxyAllowlistedActionReturnsSanitized(t *testing.T) {
	d := &recordingDescriber{resp: map[string]any{
		"UHostSet": []any{map[string]any{
			"UHostId":  "uhost-abc",
			"Name":     "billing-probe",
			"Password": "cGxhaW50ZXh0LXB3", // base64 SSH pw — MUST be redacted out
		}},
	}}
	p, err := startAPIProxy(context.Background(), d, []string{"DescribeCompShareInstance"})
	if err != nil {
		t.Fatalf("startAPIProxy: %v", err)
	}
	defer p.Close()

	resp, decoded := postAPIRead(t, p.URL(), p.Token(), "DescribeCompShareInstance", map[string]any{"UHostIds.0": "uhost-abc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if d.calls != 1 || d.lastAction != "DescribeCompShareInstance" {
		t.Fatalf("executor calls=%d action=%q, want 1 DescribeCompShareInstance", d.calls, d.lastAction)
	}
	if got := d.lastParams["UHostIds.0"]; got != "uhost-abc" {
		t.Fatalf("params not forwarded: %v", d.lastParams)
	}
	// The password must be gone from the returned body.
	if bytes.Contains(mustJSON(t, decoded), []byte("cGxhaW50ZXh0LXB3")) {
		t.Fatalf("password leaked in response: %v", decoded)
	}
	// but the non-sensitive data survives
	set, _ := decoded["UHostSet"].([]any)
	if len(set) != 1 {
		t.Fatalf("data dropped: %v", decoded)
	}
}

// WHY: deny-by-default is the core control. A non-allowlisted action (e.g. a mutating one the model
// invents) must be refused BEFORE the executor is touched.
func TestAPIProxyDeniesNonAllowlistedAction(t *testing.T) {
	d := &recordingDescriber{resp: map[string]any{"ok": true}}
	p, err := startAPIProxy(context.Background(), d, []string{"DescribeCompShareInstance"})
	if err != nil {
		t.Fatalf("startAPIProxy: %v", err)
	}
	defer p.Close()

	resp, _ := postAPIRead(t, p.URL(), p.Token(), "TerminateCompShareInstance", map[string]any{"UHostId": "uhost-abc"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if d.calls != 0 {
		t.Fatalf("executor was called %d times for a denied action, want 0", d.calls)
	}
}

// WHY: the per-task token is the loopback defense-in-depth. A missing/wrong token must 401 before
// the body is read or the executor is touched.
func TestAPIProxyRejectsBadToken(t *testing.T) {
	d := &recordingDescriber{resp: map[string]any{"ok": true}}
	p, err := startAPIProxy(context.Background(), d, []string{"DescribeCompShareInstance"})
	if err != nil {
		t.Fatalf("startAPIProxy: %v", err)
	}
	defer p.Close()

	for _, tok := range []string{"", "wrong-token"} {
		resp, _ := postAPIRead(t, p.URL(), tok, "DescribeCompShareInstance", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token=%q status = %d, want 401", tok, resp.StatusCode)
		}
	}
	if d.calls != 0 {
		t.Fatalf("executor called %d times despite bad token, want 0", d.calls)
	}
}

// WHY: a GET (or any non-POST) must not be served — only the POST read contract exists.
func TestAPIProxyRejectsNonPost(t *testing.T) {
	d := &recordingDescriber{resp: map[string]any{"ok": true}}
	p, err := startAPIProxy(context.Background(), d, []string{"DescribeCompShareInstance"})
	if err != nil {
		t.Fatalf("startAPIProxy: %v", err)
	}
	defer p.Close()

	resp, err := http.Get(p.URL())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if d.calls != 0 {
		t.Fatalf("executor called on GET, want 0")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
