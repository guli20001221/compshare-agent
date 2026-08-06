package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// postInternalGateway sends one request to the UCloud private-network API gateway
// (agent.sts.iam_url, e.g. http://internal.api.ucloud.cn).
//
// The gateway is NOT the signed public OpenAPI: it routes by the request body's
// "Backend" field and authenticates by network trust, so there is no AK/SK
// signature, no Region param and no per-backend URL — every internal backend is
// the same POST to the same "/" with a different Backend/Action pair. That is why
// this transport is a plain shared helper rather than another ExternalExecutor.
//
// `what` prefixes every error so a failure names its caller rather than surfacing
// as an anonymous HTTP problem.
func postInternalGateway(ctx context.Context, hc *http.Client, url, what string, params map[string]any) ([]byte, error) {
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%s marshal: %w", what, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s build request: %w", what, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s http: %w", what, err)
	}
	defer resp.Body.Close()
	const maxResponseSize = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("%s read: %w", what, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s status %d: %s", what, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
