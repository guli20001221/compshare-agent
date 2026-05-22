package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
)

func TestUcloudSign(t *testing.T) {
	// UCloud signing: SHA1(sorted key-value concatenation + privateKey)
	params := map[string]string{
		"Action":    "DescribeCompShareInstance",
		"Region":    "cn-wlcb",
		"PublicKey": "testpubkey",
	}
	privateKey := "testprivkey"

	sig1 := ucloudSign(params, privateKey)
	if sig1 == "" {
		t.Fatal("ucloudSign returned empty string")
	}

	// Same input → same signature (deterministic)
	sig2 := ucloudSign(params, privateKey)
	if sig1 != sig2 {
		t.Errorf("ucloudSign not deterministic: %q != %q", sig1, sig2)
	}

	// Different privateKey → different signature
	sig3 := ucloudSign(params, "otherprivkey")
	if sig1 == sig3 {
		t.Error("different privateKey should produce different signature")
	}

	// Different params → different signature
	params2 := map[string]string{
		"Action":    "StartCompShareInstance",
		"Region":    "cn-wlcb",
		"PublicKey": "testpubkey",
	}
	sig4 := ucloudSign(params2, privateKey)
	if sig1 == sig4 {
		t.Error("different params should produce different signature")
	}
}

func TestUcloudSign_SortOrder(t *testing.T) {
	// Signature must be based on sorted keys, not insertion order
	params1 := map[string]string{"A": "1", "B": "2", "C": "3"}
	params2 := map[string]string{"C": "3", "A": "1", "B": "2"}

	sig1 := ucloudSign(params1, "key")
	sig2 := ucloudSign(params2, "key")
	if sig1 != sig2 {
		t.Errorf("signature should be order-independent: %q != %q", sig1, sig2)
	}
}

func TestFlattenInto_Simple(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{
		"Zone":    "cn-wlcb-a",
		"GpuType": "4090",
		"Gpu":     1,
	}
	flattenInto(dst, src, "")

	if dst["Zone"] != "cn-wlcb-a" {
		t.Errorf("Zone = %q, want cn-wlcb-a", dst["Zone"])
	}
	if dst["GpuType"] != "4090" {
		t.Errorf("GpuType = %q, want 4090", dst["GpuType"])
	}
	if dst["Gpu"] != "1" {
		t.Errorf("Gpu = %q, want 1", dst["Gpu"])
	}
}

func TestFlattenInto_NestedMap(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{
		"Config": map[string]any{
			"CPU":    16,
			"Memory": 65536,
		},
	}
	flattenInto(dst, src, "")

	if dst["Config.CPU"] != "16" {
		t.Errorf("Config.CPU = %q, want 16", dst["Config.CPU"])
	}
	if dst["Config.Memory"] != "65536" {
		t.Errorf("Config.Memory = %q, want 65536", dst["Config.Memory"])
	}
}

func TestFlattenInto_Array(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{
		"Disks": []any{
			map[string]any{"IsBoot": true, "Size": 40},
			map[string]any{"IsBoot": false, "Size": 100},
		},
	}
	flattenInto(dst, src, "")

	if dst["Disks.0.IsBoot"] != "true" {
		t.Errorf("Disks.0.IsBoot = %q, want true", dst["Disks.0.IsBoot"])
	}
	if dst["Disks.0.Size"] != "40" {
		t.Errorf("Disks.0.Size = %q, want 40", dst["Disks.0.Size"])
	}
	if dst["Disks.1.Size"] != "100" {
		t.Errorf("Disks.1.Size = %q, want 100", dst["Disks.1.Size"])
	}
}

func TestFlattenInto_StringArray(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{
		"UHostIds": []string{"uhost-a", "uhost-b"},
	}
	flattenInto(dst, src, "")

	if dst["UHostIds"] != "" {
		t.Errorf("UHostIds = %q, want empty top-level slice encoding", dst["UHostIds"])
	}
	if dst["UHostIds.0"] != "uhost-a" {
		t.Errorf("UHostIds.0 = %q, want uhost-a", dst["UHostIds.0"])
	}
	if dst["UHostIds.1"] != "uhost-b" {
		t.Errorf("UHostIds.1 = %q, want uhost-b", dst["UHostIds.1"])
	}
}

func TestFlattenInto_WithPrefix(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{"Name": "test"}
	flattenInto(dst, src, "Prefix")

	if dst["Prefix.Name"] != "test" {
		t.Errorf("Prefix.Name = %q, want test", dst["Prefix.Name"])
	}
}

// --- ProjectId injection ---

// TestExternalExecutor_ProjectIdFromConfig and the now-deleted
// TestExternalExecutor_SetProjectId used to share coverage of two
// ProjectId entry points. PR9 collapsed that to cfg-only: SetProjectId
// is gone, ProjectId() getter is gone. We assert ProjectId reaches the
// signed request body by inspecting captured forms instead of reading
// the field back, since the field is now package-private with no
// accessor (intentional: removes any reflection-survivable handle on
// the mutation surface).
func TestExternalExecutor_ProjectIdFromConfig(t *testing.T) {
	apiURL, captured, cleanup := captureForm(t)
	defer cleanup()

	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: apiURL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		ProjectId:       "org-from-cfg",
	})
	_, err := ext.Execute(context.Background(), "DescribeCompShareInstance", nil)
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if got := captured.Get("ProjectId"); got != "org-from-cfg" {
		t.Errorf("signed form ProjectId = %q, want org-from-cfg", got)
	}
}

// captureForm starts an httptest server that records the form body of the
// first POST request. Callers use the returned URL as CompShareAPIURL and
// read the captured form after Execute.
func captureForm(t *testing.T) (apiURL string, captured *url.Values, cleanup func()) {
	t.Helper()
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "TestResponse"}`))
	}))
	return srv.URL, &form, srv.Close
}

func TestExternalExecutor_AutoInjectsProjectId(t *testing.T) {
	apiURL, form, cleanup := captureForm(t)
	defer cleanup()

	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: apiURL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		ProjectId:       "org-cfg",
	})

	if _, err := ext.Execute(context.Background(), "UpdateCompShareStopScheduler", map[string]any{
		"UHostId": "uhost-xxx",
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if (*form).Get("ProjectId") != "org-cfg" {
		t.Errorf("form ProjectId = %q, want org-cfg", (*form).Get("ProjectId"))
	}
}

func TestExternalExecutor_ExplicitProjectIdOverridesConfig(t *testing.T) {
	apiURL, form, cleanup := captureForm(t)
	defer cleanup()

	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: apiURL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		ProjectId:       "org-cfg",
	})

	if _, err := ext.Execute(context.Background(), "SomeAction", map[string]any{
		"ProjectId": "org-explicit",
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if (*form).Get("ProjectId") != "org-explicit" {
		t.Errorf("form ProjectId = %q, want org-explicit (explicit args must win)", (*form).Get("ProjectId"))
	}
}

func TestExternalExecutor_NoProjectIdWhenUnset(t *testing.T) {
	apiURL, form, cleanup := captureForm(t)
	defer cleanup()

	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: apiURL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		// ProjectId intentionally empty
	})

	if _, err := ext.Execute(context.Background(), "DescribeCompShareInstance", nil); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := (*form).Get("ProjectId"); got != "" {
		t.Errorf("form ProjectId = %q, want empty when unset", got)
	}
}

func TestExternalExecutor_MonitorUsesJSONBodyForUHostIds(t *testing.T) {
	var contentType string
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "GetCompShareInstanceMonitorResponse"}`))
	}))
	defer srv.Close()

	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: srv.URL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		ProjectId:       "org-cfg",
	})

	if _, err := ext.Execute(context.Background(), "GetCompShareInstanceMonitor", map[string]any{
		"UHostIds":  []any{"uhost-1"},
		"StartTime": 1777442400,
		"EndTime":   1777444200,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json; body=%s", contentType, string(rawBody))
	}
	if form, err := url.ParseQuery(string(rawBody)); err == nil && form.Get("UHostIds.0") != "" {
		t.Fatalf("monitor request used form-style UHostIds.0=%q; body=%s", form.Get("UHostIds.0"), string(rawBody))
	}

	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatalf("monitor request body is not JSON: %v; body=%s", err, string(rawBody))
	}
	ids, ok := body["UHostIds"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "uhost-1" {
		t.Fatalf("UHostIds = %#v, want [uhost-1]", body["UHostIds"])
	}
	if body["ProjectId"] != "org-cfg" {
		t.Fatalf("ProjectId = %#v, want org-cfg", body["ProjectId"])
	}
	if body["Signature"] == "" {
		t.Fatal("Signature is empty")
	}
}

// --- STS / CredentialProvider integration tests ---

// TestExecuteWithSTSCredentials verifies that Execute includes SecurityToken in
// the form params and signs with the temporary AccessKeySecret.
func TestExecuteWithSTSCredentials(t *testing.T) {
	const (
		tempAK    = "sts-ak-temp"
		tempSK    = "sts-sk-temp"
		tempToken = "sts-token-xyz"
	)

	var capturedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "TestResponse"}`))
	}))
	defer srv.Close()

	provider := StaticCredentialProvider{Cred: &Credentials{
		AccessKeyId:     tempAK,
		AccessKeySecret: tempSK,
		SecurityToken:   tempToken,
		ExpireAt:        time.Now().Add(time.Hour),
	}}
	ext := NewExternalExecutorWithProvider(srv.URL, "cn-wlcb", "org-001", provider)

	if _, err := ext.Execute(context.Background(), "DescribeCompShareInstance", map[string]any{
		"UHostId": "uhost-test",
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// PublicKey must be the temp AK
	if got := capturedForm.Get("PublicKey"); got != tempAK {
		t.Errorf("PublicKey = %q, want %q", got, tempAK)
	}
	// SecurityToken must be present
	if got := capturedForm.Get("SecurityToken"); got != tempToken {
		t.Errorf("SecurityToken = %q, want %q", got, tempToken)
	}
	// Signature must be computed with temp SK — recompute and compare
	params := make(map[string]string)
	for k, vals := range capturedForm {
		if k == "Signature" {
			continue
		}
		params[k] = vals[0]
	}
	expectedSig := ucloudSign(params, tempSK)
	if got := capturedForm.Get("Signature"); got != expectedSig {
		t.Errorf("Signature = %q, want %q (signed with temp SK)", got, expectedSig)
	}
}

// TestExecuteJSONWithSTSCredentials verifies that executeJSON includes
// SecurityToken in the JSON body and signs with the temporary AccessKeySecret.
func TestExecuteJSONWithSTSCredentials(t *testing.T) {
	const (
		tempAK    = "sts-ak-json"
		tempSK    = "sts-sk-json"
		tempToken = "sts-token-json"
	)

	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "GetCompShareInstanceMonitorResponse"}`))
	}))
	defer srv.Close()

	provider := StaticCredentialProvider{Cred: &Credentials{
		AccessKeyId:     tempAK,
		AccessKeySecret: tempSK,
		SecurityToken:   tempToken,
		ExpireAt:        time.Now().Add(time.Hour),
	}}
	ext := NewExternalExecutorWithProvider(srv.URL, "cn-wlcb", "org-001", provider)

	if _, err := ext.Execute(context.Background(), "GetCompShareInstanceMonitor", map[string]any{
		"UHostIds":  []any{"uhost-1"},
		"StartTime": 1000,
		"EndTime":   2000,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// PublicKey must be the temp AK
	if got, _ := capturedBody["PublicKey"].(string); got != tempAK {
		t.Errorf("PublicKey = %q, want %q", got, tempAK)
	}
	// SecurityToken must be in the body
	if got, _ := capturedBody["SecurityToken"].(string); got != tempToken {
		t.Errorf("SecurityToken = %q, want %q", got, tempToken)
	}
	// Signature must be non-empty
	if sig, _ := capturedBody["Signature"].(string); sig == "" {
		t.Error("Signature is empty")
	}
	// Recompute expected signature (without Signature key itself)
	params := make(map[string]any)
	for k, v := range capturedBody {
		if k == "Signature" {
			continue
		}
		params[k] = v
	}
	expectedSig := ucloudSignJSON(params, tempSK)
	if got, _ := capturedBody["Signature"].(string); got != expectedSig {
		t.Errorf("Signature = %q, want %q (signed with temp SK)", got, expectedSig)
	}
}

// TestExecuteWithUserContextOverridesRegionProject verifies that region and
// projectId from UserContext override the executor defaults.
func TestExecuteWithUserContextOverridesRegionProject(t *testing.T) {
	var capturedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "TestResponse"}`))
	}))
	defer srv.Close()

	provider := StaticCredentialProvider{Cred: &Credentials{
		AccessKeyId:     "ak",
		AccessKeySecret: "sk",
	}}
	// Executor defaults: region=cn-wlcb, project=default-project
	ext := NewExternalExecutorWithProvider(srv.URL, "cn-wlcb", "default-project", provider)

	// UserContext overrides both
	ctx := WithUser(context.Background(), UserContext{
		Region:    "cn-sh2",
		ProjectId: "override-project",
	})

	if _, err := ext.Execute(ctx, "DescribeCompShareInstance", nil); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := capturedForm.Get("Region"); got != "cn-sh2" {
		t.Errorf("Region = %q, want cn-sh2 (UserContext override)", got)
	}
	if got := capturedForm.Get("ProjectId"); got != "override-project" {
		t.Errorf("ProjectId = %q, want override-project (UserContext override)", got)
	}
}

// TestExecuteNilProviderErrors verifies that Execute returns a meaningful error
// when no credential provider is configured.
func TestExecuteNilProviderErrors(t *testing.T) {
	ext := &ExternalExecutor{
		apiURL:     "http://example.invalid/",
		creds:      nil,
		region:     "cn-wlcb",
		httpClient: &http.Client{Timeout: time.Second},
	}

	_, err := ext.Execute(context.Background(), "DescribeCompShareInstance", nil)
	if err == nil {
		t.Fatal("expected error when creds is nil, got nil")
	}
	if !strings.Contains(err.Error(), "no credential provider") {
		t.Errorf("error = %q, want to mention 'no credential provider'", err.Error())
	}
}

// TestExecuteStaticNoSecurityToken verifies that when using legacy static
// credentials (no SecurityToken), SecurityToken is NOT present in the form.
func TestExecuteStaticNoSecurityToken(t *testing.T) {
	var capturedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "TestResponse"}`))
	}))
	defer srv.Close()

	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: srv.URL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
	})

	if _, err := ext.Execute(context.Background(), "DescribeCompShareInstance", nil); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := capturedForm.Get("SecurityToken"); got != "" {
		t.Errorf("SecurityToken = %q, want empty for static credentials", got)
	}
	if got := capturedForm.Get("PublicKey"); got != "pk" {
		t.Errorf("PublicKey = %q, want pk", got)
	}
}

func TestSafeExecutorWithExternalExecutorUsesSTSWithinAttemptBudget(t *testing.T) {
	expiration := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	var stsCalls int
	stsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stsCalls++
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeSTSResponse("tmp-ak", "tmp-sk", "tmp-token", expiration))
	}))
	defer stsSrv.Close()

	var capturedForm url.Values
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RetCode": 0, "ImageSet": []}`))
	}))
	defer apiSrv.Close()

	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: apiSrv.URL,
		Region:          "cn-wlcb",
		STS: config.STSConfig{
			ServiceAK: "svc-ak",
			ServiceSK: "svc-sk",
			URL:       stsSrv.URL,
		},
	})
	policies := DefaultToolExecutionPolicies()
	policy := policies["DescribeCompShareImages"]
	policy.TimeoutMS = 100
	policy.BackoffBaseMS = 0
	policies["DescribeCompShareImages"] = policy
	safe := NewSafeToolExecutor(ext, WithPolicies(policies))

	ctx := WithUser(context.Background(), UserContext{
		RoleUrn:     "ucs:iam::1:role/test",
		SessionName: "tool-retry-test",
		ProjectId:   "project-from-user",
	})
	result, err := safe.ExecuteSafe(ctx, SafeToolRequest{
		Action: "DescribeCompShareImages",
		Args:   map[string]any{"Limit": 1},
		Origin: OriginDirectLLM,
	})
	if err != nil {
		t.Fatalf("ExecuteSafe error: %v", err)
	}
	if result.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", result.Attempts)
	}
	if stsCalls != 1 {
		t.Fatalf("STS calls = %d, want 1", stsCalls)
	}
	if got := capturedForm.Get("PublicKey"); got != "tmp-ak" {
		t.Fatalf("PublicKey = %q, want tmp-ak", got)
	}
	if got := capturedForm.Get("SecurityToken"); got != "tmp-token" {
		t.Fatalf("SecurityToken = %q, want tmp-token", got)
	}
	if got := capturedForm.Get("ProjectId"); got != "project-from-user" {
		t.Fatalf("ProjectId = %q, want project-from-user", got)
	}
}

func TestSafeExecutorRetriesHTTP5xxStatus(t *testing.T) {
	cases := []struct {
		name   string
		action string
		args   map[string]any
	}{
		{name: "form", action: "DescribeCompShareImages", args: map[string]any{"Limit": 1}},
		{name: "json", action: "GetCompShareInstanceMonitor", args: map[string]any{"UHostIds": []any{"uhost-1"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "Ignored"}`))
					return
				}
				_, _ = w.Write([]byte(`{"RetCode": 0, "Action": "Success"}`))
			}))
			defer srv.Close()

			ext := NewExternalExecutor(config.AgentConfig{
				CompShareAPIURL: srv.URL,
				PublicKey:       "pk",
				PrivateKey:      "sk",
				Region:          "cn-wlcb",
			})
			policies := DefaultToolExecutionPolicies()
			policy := policies[tc.action]
			policy.BackoffBaseMS = 0
			policies[tc.action] = policy
			safe := NewSafeToolExecutor(ext, WithPolicies(policies))

			result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: tc.action,
				Args:   tc.args,
				Origin: OriginDirectLLM,
			})
			if err != nil {
				t.Fatalf("ExecuteSafe error: %v", err)
			}
			if result.Attempts != 2 {
				t.Fatalf("Attempts = %d, want 2", result.Attempts)
			}
			if calls != 2 {
				t.Fatalf("HTTP calls = %d, want 2", calls)
			}
		})
	}
}
