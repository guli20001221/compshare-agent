package tools

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
)

// ExternalExecutor calls CompShare API via public endpoint with AK/SK signing.
// Credentials are obtained from creds on every Execute / executeJSON call so
// that STS temporary tokens are refreshed transparently.
type ExternalExecutor struct {
	apiURL     string
	creds      CredentialProvider
	region     string
	projectId  string
	httpClient *http.Client
}

// NewExternalExecutor constructs an ExternalExecutor from AgentConfig.
// When STS service credentials are present they take priority; otherwise the
// legacy PublicKey/PrivateKey pair is wrapped in a StaticCredentialProvider
// for backward compatibility.
func NewExternalExecutor(cfg config.AgentConfig) *ExternalExecutor {
	var provider CredentialProvider
	if cfg.STS.ServiceAK != "" && cfg.STS.ServiceSK != "" {
		provider = NewSTSProvider(cfg.STS.ServiceAK, cfg.STS.ServiceSK, cfg.STS.URL,
			WithDurationSeconds(cfg.STS.DurationSeconds),
			WithRefreshBefore(cfg.STS.RefreshBefore))
	} else if cfg.PublicKey != "" && cfg.PrivateKey != "" {
		provider = StaticCredentialProvider{Cred: &Credentials{
			AccessKeyId:     cfg.PublicKey,
			AccessKeySecret: cfg.PrivateKey,
		}}
	}
	return &ExternalExecutor{
		apiURL:    strings.TrimRight(cfg.CompShareAPIURL, "/") + "/",
		creds:     provider,
		region:    cfg.Region,
		projectId: cfg.ProjectId,
		// 60s is the last-resort safety net; per-action TimeoutMS in
		// ToolExecutionPolicy (PR #5) is the primary deadline applied
		// via context.WithTimeout in SafeToolExecutor.executeWithRetry.
		// Keeping the client timeout above the largest per-action
		// budget (monitor = 30s) means policy-level deadlines dominate
		// in the common case, while runaway calls still cap at 60s.
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// NewExternalExecutorWithProvider constructs an ExternalExecutor with an
// explicit CredentialProvider. Intended for HTTP path and tests.
func NewExternalExecutorWithProvider(apiURL, region, projectId string, provider CredentialProvider) *ExternalExecutor {
	return &ExternalExecutor{
		apiURL:     strings.TrimRight(apiURL, "/") + "/",
		creds:      provider,
		region:     region,
		projectId:  projectId,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetProjectId and ProjectId getters were removed in PR9 to close a
// cross-session leak: ExternalExecutor lives in SharedDeps process-wide
// across sessions, so a runtime setter let session A's auto-discovered
// project id auto-inject into session B's tool calls. ProjectId now only
// comes from cfg at construction time. If mutating tools later need a
// per-session ProjectId, pass it via args["ProjectId"] in the call path
// (per-session field on Engine), NOT by re-introducing a setter here.

func (e *ExternalExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if usesJSONBody(action) {
		return e.executeJSON(ctx, action, args)
	}

	if e.creds == nil {
		return nil, fmt.Errorf("ExternalExecutor: no credential provider configured")
	}
	cred, err := e.creds.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("ExternalExecutor: get credentials: %w", err)
	}

	// Resolve region and projectId: prefer UserContext when present.
	region, project := e.region, e.projectId
	if u, ok := UserFrom(ctx); ok {
		if u.Region != "" {
			region = u.Region
		}
		if u.ProjectId != "" {
			project = u.ProjectId
		}
	}

	// Build params: Action + Region + args + PublicKey
	params := map[string]string{
		"Action":    action,
		"Region":    region,
		"PublicKey": cred.AccessKeyId,
	}
	flattenInto(params, args, "")

	// Include SecurityToken for STS temporary credentials (must be before signing).
	if cred.SecurityToken != "" {
		params["SecurityToken"] = cred.SecurityToken
	}

	// Auto-inject ProjectId if configured and caller didn't provide one.
	// Some APIs (e.g. UpdateCompShareStopScheduler) require it; others
	// accept it without side effects. We inject unconditionally to avoid
	// per-action allowlisting.
	if project != "" {
		if _, provided := params["ProjectId"]; !provided {
			params["ProjectId"] = project
		}
	}

	// Sign: UCloud HMAC-SHA1 signature
	params["Signature"] = ucloudSign(params, cred.AccessKeySecret)

	// POST as application/x-www-form-urlencoded
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.apiURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 1 << 20 // 1MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if err := httpStatusError(resp.StatusCode, body); err != nil {
		return nil, err
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	// Check RetCode
	if retCode, ok := result["RetCode"].(float64); ok && retCode != 0 {
		msg, _ := result["Message"].(string)
		return nil, fmt.Errorf("API error (RetCode=%d): %s", int(retCode), msg)
	}

	return result, nil
}

func (e *ExternalExecutor) executeJSON(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if e.creds == nil {
		return nil, fmt.Errorf("ExternalExecutor: no credential provider configured")
	}
	cred, err := e.creds.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("ExternalExecutor: get credentials: %w", err)
	}

	// Resolve region and projectId: prefer UserContext when present.
	region, project := e.region, e.projectId
	if u, ok := UserFrom(ctx); ok {
		if u.Region != "" {
			region = u.Region
		}
		if u.ProjectId != "" {
			project = u.ProjectId
		}
	}

	body := map[string]any{
		"Action":    action,
		"Region":    region,
		"PublicKey": cred.AccessKeyId,
	}
	for k, v := range args {
		body[k] = v
	}

	if project != "" {
		if _, provided := body["ProjectId"]; !provided {
			body["ProjectId"] = project
		}
	}

	// Include SecurityToken for STS temporary credentials (must be before signing).
	if cred.SecurityToken != "" {
		body["SecurityToken"] = cred.SecurityToken
	}

	body["Signature"] = ucloudSignJSON(body, cred.AccessKeySecret)

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", e.apiURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 1 << 20 // 1MB
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if err := httpStatusError(resp.StatusCode, respBody); err != nil {
		return nil, err
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if retCode, ok := result["RetCode"].(float64); ok && retCode != 0 {
		msg, _ := result["Message"].(string)
		return nil, fmt.Errorf("API error (RetCode=%d): %s", int(retCode), msg)
	}

	return result, nil
}

func httpStatusError(statusCode int, body []byte) error {
	if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
		return nil
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 512 {
		snippet = snippet[:512] + "..."
	}
	if snippet == "" {
		return fmt.Errorf("api call status code %d", statusCode)
	}
	return fmt.Errorf("api call status code %d: %s", statusCode, snippet)
}

func usesJSONBody(action string) bool {
	return action == "GetCompShareInstanceMonitor"
}

// ucloudSign computes the UCloud API signature.
// Algorithm: SHA1( sorted_params_concatenation + private_key )
func ucloudSign(params map[string]string, privateKey string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteString(params[k])
	}
	buf.WriteString(privateKey)

	h := sha1.New()
	h.Write([]byte(buf.String()))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func ucloudSignJSON(params map[string]any, privateKey string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "Signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteString(jsonSignValue(params[k]))
	}
	buf.WriteString(privateKey)

	h := sha1.New()
	h.Write([]byte(buf.String()))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func jsonSignValue(v any) string {
	switch val := v.(type) {
	case bool:
		if val {
			return "true"
		}
		return "false"
	case string:
		return val
	case int:
		return fmt.Sprintf("%d", val)
	case int64:
		return fmt.Sprintf("%d", val)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case []any:
		var buf strings.Builder
		for _, item := range val {
			buf.WriteString(jsonSignValue(item))
		}
		return buf.String()
	case []string:
		var buf strings.Builder
		for _, item := range val {
			buf.WriteString(item)
		}
		return buf.String()
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf strings.Builder
		for _, k := range keys {
			buf.WriteString(k)
			buf.WriteString(jsonSignValue(val[k]))
		}
		return buf.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

// flattenInto converts nested args to flat "Key.N.SubKey" form for UCloud API.
func flattenInto(dst map[string]string, src map[string]any, prefix string) {
	for k, v := range src {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch val := v.(type) {
		case map[string]any:
			flattenInto(dst, val, key)
		case []string:
			// Phase 1 handlers emit []string directly. Without this case the
			// default branch would encode the slice as "[a b]" instead of Key.N.
			for i, item := range val {
				dst[fmt.Sprintf("%s.%d", key, i)] = item
			}
		case []any:
			for i, item := range val {
				subKey := fmt.Sprintf("%s.%d", key, i)
				if m, ok := item.(map[string]any); ok {
					flattenInto(dst, m, subKey)
				} else {
					dst[subKey] = fmt.Sprintf("%v", item)
				}
			}
		default:
			dst[key] = fmt.Sprintf("%v", val)
		}
	}
}
