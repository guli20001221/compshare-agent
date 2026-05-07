package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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

func TestUcloudSignJSON_MonitorWithTimeRange(t *testing.T) {
	params := map[string]any{
		"Action":    "GetCompShareInstanceMonitor",
		"Region":    "cn-wlcb",
		"PublicKey": "pk",
		"ProjectId": "org-cfg",
		"UHostIds":  []any{"uhost-1", "uhost-2"},
		// Tool-call JSON args decode numbers as float64; whole-number
		// timestamps must sign like integer request parameters.
		"StartTime": float64(1712563200),
		"EndTime":   float64(1712566800),
	}

	got := ucloudSignJSON(params, "sk")
	const want = "919a6fb333e2652a7c1671938e00c1b4dd351979"
	if got != want {
		t.Fatalf("ucloudSignJSON() = %q, want %q", got, want)
	}
	if got := jsonSignValue(float64(1712563200)); got != "1712563200" {
		t.Fatalf("jsonSignValue(float64 timestamp) = %q, want plain integer string", got)
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

func TestFlattenInto_WithPrefix(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{"Name": "test"}
	flattenInto(dst, src, "Prefix")

	if dst["Prefix.Name"] != "test" {
		t.Errorf("Prefix.Name = %q, want test", dst["Prefix.Name"])
	}
}

// --- ProjectId injection ---

func TestExternalExecutor_ProjectIdFromConfig(t *testing.T) {
	ext := NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: "http://example.invalid",
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		ProjectId:       "org-from-cfg",
	})
	if got := ext.ProjectId(); got != "org-from-cfg" {
		t.Errorf("ProjectId() = %q, want org-from-cfg", got)
	}
}

func TestExternalExecutor_SetProjectId(t *testing.T) {
	ext := NewExternalExecutor(config.AgentConfig{CompShareAPIURL: "http://example.invalid"})
	if got := ext.ProjectId(); got != "" {
		t.Errorf("initial ProjectId() = %q, want empty", got)
	}
	ext.SetProjectId("org-runtime")
	if got := ext.ProjectId(); got != "org-runtime" {
		t.Errorf("after SetProjectId, ProjectId() = %q, want org-runtime", got)
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

func TestExternalExecutor_MonitorUsesJSONBodyForArrayParams(t *testing.T) {
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
		"UHostIds": []any{"uhost-1"},
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatalf("request body is not JSON: %v; body=%s", err, string(rawBody))
	}
	ids, ok := body["UHostIds"].([]any)
	if !ok {
		t.Fatalf("UHostIds encoded as %T, want JSON array; body=%v", body["UHostIds"], body)
	}
	if len(ids) != 1 || ids[0] != "uhost-1" {
		t.Fatalf("UHostIds = %#v, want [uhost-1]", ids)
	}
	if body["UHostIds.0"] != nil {
		t.Fatalf("body should not contain flattened UHostIds.0: %v", body)
	}
	if body["ProjectId"] != "org-cfg" {
		t.Fatalf("ProjectId = %v, want org-cfg", body["ProjectId"])
	}
	if body["Signature"] == "" {
		t.Fatal("Signature missing from JSON body")
	}
}
