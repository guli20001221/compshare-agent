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
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
)

// ExternalExecutor calls CompShare API via public endpoint with AK/SK signing.
type ExternalExecutor struct {
	apiURL     string
	publicKey  string
	privateKey string
	region     string
	projectId  string
	httpClient *http.Client
}

func NewExternalExecutor(cfg config.AgentConfig) *ExternalExecutor {
	return &ExternalExecutor{
		apiURL:     strings.TrimRight(cfg.CompShareAPIURL, "/") + "/",
		publicKey:  cfg.PublicKey,
		privateKey: cfg.PrivateKey,
		region:     cfg.Region,
		projectId:  cfg.ProjectId,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetProjectId updates the ProjectId to auto-inject into every request.
// Used when the caller discovers ProjectId at runtime (e.g. via GetProjectList
// during Init) instead of providing it via config.
func (e *ExternalExecutor) SetProjectId(id string) {
	e.projectId = id
}

// ProjectId returns the currently configured ProjectId, or "" if unset.
func (e *ExternalExecutor) ProjectId() string {
	return e.projectId
}

func (e *ExternalExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	// Build params: Action + Region + args + PublicKey
	params := map[string]string{
		"Action":    action,
		"Region":    e.region,
		"PublicKey": e.publicKey,
	}
	flattenInto(params, args, "")

	// Auto-inject ProjectId if configured and caller didn't provide one.
	// Some APIs (e.g. UpdateCompShareStopScheduler) require it; others
	// accept it without side effects. We inject unconditionally to avoid
	// per-action allowlisting.
	if e.projectId != "" {
		if _, provided := params["ProjectId"]; !provided {
			params["ProjectId"] = e.projectId
		}
	}

	// Sign: UCloud HMAC-SHA1 signature
	params["Signature"] = ucloudSign(params, e.privateKey)

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
