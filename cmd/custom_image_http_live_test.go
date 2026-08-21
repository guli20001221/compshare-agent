//go:build live

package main

// Real HTTP/WebSocket acceptance for the asynchronous custom-image workflow.
//
// This gate deliberately starts the production handler and production
// SharedDeps in-process. Conversation persistence is the only substituted
// dependency: using the small in-memory store lets an unavailable development
// database neither hide a valid STS path nor turn a live Agent regression into
// a database false negative. The handler still builds UserContext from the
// gateway headers, the executor still obtains tenant credentials through STS,
// and the Agent still chooses and runs the workflow.
//
// The test is opt-in because it creates one custom image in the selected test
// tenant. It never stops, starts, or deletes the source instance. The upstream
// may temporarily report the source as ImageMaking while it snapshots it; that
// state is not a stop/start action and must not be treated as one.
//
// Run (all identity values come from the gateway/integration configuration,
// never from this source file):
//
//   COMPSHARE_CUSTOM_IMAGE_HTTP_LIVE=1 go test ./cmd -tags live \
//     -run TestCreateCustomImageOverRealHTTP -count=1 -v -timeout 15m \
//     -custom-image-http-top-org ... -custom-image-http-org ...
//
// An explicit -custom-image-http-source is preferred. When omitted, the gate
// chooses a Running or Stopped source returned by the supplied disposable test
// tenant, excluding the upstream's unsupported 2C/4GB no-GPU shape.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var (
	customImageHTTPLive    = flag.Bool("custom-image-http-live", false, "run the real HTTP/WS custom-image acceptance gate")
	customImageHTTPTopOrg  = flag.Uint("custom-image-http-top-org", 0, "disposable test tenant top organization id")
	customImageHTTPOrg     = flag.Uint("custom-image-http-org", 0, "disposable test tenant organization id")
	customImageHTTPAccount = flag.Uint("custom-image-http-account", 0, "gateway-authenticated account id (optional)")
	customImageHTTPEmail   = flag.String("custom-image-http-email", "", "gateway-authenticated user email (optional)")
	customImageHTTPProject = flag.String("custom-image-http-project", "", "project id override")
	customImageHTTPSource  = flag.String("custom-image-http-source", "", "explicit disposable source instance id; optional")
)

var customImageIDInReply = regexp.MustCompile(`ID:\s*([^）。\s]+)`)

func TestCreateCustomImageOverRealHTTP(t *testing.T) {
	if !*customImageHTTPLive && os.Getenv("COMPSHARE_CUSTOM_IMAGE_HTTP_LIVE") != "1" {
		t.Skip("set COMPSHARE_CUSTOM_IMAGE_HTTP_LIVE=1 or -custom-image-http-live to run real HTTP/WS custom-image acceptance")
	}
	if *customImageHTTPTopOrg == 0 || *customImageHTTPOrg == 0 {
		t.Fatal("-custom-image-http-top-org and -custom-image-http-org are required")
	}

	root := behavioralRepoRoot(t)
	originalDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	cfg, err := config.Load(filepath.Join(root, "deploy", "conf", "config.local.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Agent.LLM.APIKey, "real Agent acceptance needs the configured model key")
	if project := strings.TrimSpace(*customImageHTTPProject); project != "" {
		cfg.Agent.ProjectId = project
	}

	topOrg, org := uint32(*customImageHTTPTopOrg), uint32(*customImageHTTPOrg)
	base, err := upstreamContractUserContext(cfg, topOrg, org, uint32(*customImageHTTPAccount), *customImageHTTPEmail)
	require.NoError(t, err)
	directExecutor := tools.NewExternalExecutor(cfg.Agent)

	ctx, cancel := context.WithTimeout(base, 8*time.Minute)
	defer cancel()
	sourceID, sourceState, err := customImageLiveSource(ctx, directExecutor, strings.TrimSpace(*customImageHTTPSource))
	require.NoError(t, err)
	require.Contains(t, []string{"Running", "Stopped"}, sourceState, "source must remain a workflow-supported state")

	messages := serverTestMessageStore{}
	pool, err := buildHTTPServerPool(cfg, messages, cfg.RuntimeGetenv(os.Getenv), nil)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sessions := newCatalogLiveSessions()
	sessionID := fmt.Sprintf("custom-image-http-live-%d", time.Now().UnixNano())
	sessions.add(sessionID, topOrg, org)
	handlers := newServerHandlers(cfg, sessions, messages, nil, pool, nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", handlers.HandleWS)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	headers := http.Header{}
	headers.Set("X-Company-Id", fmt.Sprint(topOrg))
	headers.Set("X-Organization-Id", fmt.Sprint(org))
	if *customImageHTTPAccount != 0 {
		headers.Set("X-Account-Id", fmt.Sprint(*customImageHTTPAccount))
	}
	if email := strings.TrimSpace(*customImageHTTPEmail); email != "" {
		headers.Set("X-User-Email", email)
	}
	headers.Set("X-Request-Id", sessionID+"-request")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/?Action=CreateCSAgentWS&ProjectId=" + url.QueryEscape(cfg.Agent.ProjectId)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	imageName := fmt.Sprintf("codex-http-custom-image-%d", time.Now().UnixNano())
	request, err := json.Marshal(map[string]any{
		"Action":       "SendCSAgentChat",
		"SessionId":    sessionID,
		"Message":      fmt.Sprintf("请将实例 %s 制作为自制镜像，名称为 %s。", sourceID, imageName),
		"request_uuid": sessionID + "-turn",
		"Features":     []string{"confirm_form_v1"},
	})
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, request))

	var actions []string
	var reply string
	confirmations := 0
	for {
		_, raw, readErr := conn.Read(ctx)
		require.NoError(t, readErr)
		var event map[string]any
		require.NoError(t, json.Unmarshal(raw, &event))
		switch event["event"] {
		case "step":
			if action, _ := event["Action"].(string); action != "" {
				actions = append(actions, action)
			}
		case "confirmation":
			action, _ := event["Action"].(string)
			require.Equal(t, "CreateCustomImageWorkflow", action, "this acceptance must reach only the custom-image confirmation")
			confirmationID, _ := event["ConfirmationId"].(string)
			require.NotEmpty(t, confirmationID)
			confirmations++
			approval, marshalErr := json.Marshal(map[string]any{
				"Action": "ConfirmCSAgentAction", "SessionId": sessionID,
				"ConfirmationId": confirmationID, "Confirmed": true,
			})
			require.NoError(t, marshalErr)
			require.NoError(t, conn.Write(ctx, websocket.MessageText, approval))
		case "done":
			reply, _ = event["Content"].(string)
			goto complete
		case "error":
			t.Fatal("Agent returned an HTTP/WS error frame for the live custom-image request")
		}
	}

complete:
	require.Equal(t, 1, confirmations, "custom-image creation must have exactly one user confirmation")
	require.Contains(t, actions, "DescribeCompShareInstance")
	require.Contains(t, actions, "DescribeCompShareSupportZone")
	require.Contains(t, actions, "CreateCompShareCustomImage")
	require.NotContains(t, actions, "StopCompShareInstance", "custom-image creation must not stop its source instance")
	require.NotContains(t, actions, "StartCompShareInstance", "custom-image creation must not start its source instance")
	require.Contains(t, reply, "Making")
	require.Contains(t, reply, "Available")
	require.NotContains(t, reply, "制作完成")
	require.NotContains(t, reply, "镜像已完成")

	match := customImageIDInReply.FindStringSubmatch(reply)
	require.Len(t, match, 2, "the deterministic asynchronous reply must include the new image id")
	status, err := waitForCustomImageStatus(ctx, directExecutor, match[1])
	require.NoError(t, err)
	require.Contains(t, []string{"Making", "Available"}, status,
		"the create response must be observed as in-progress or already available, never inferred as complete from the create call alone")

	stateAfter, err := customImageLiveSourceState(ctx, directExecutor, sourceID)
	require.NoError(t, err)
	require.Contains(t, append([]string{sourceState}, "ImageMaking"), stateAfter,
		"upstream may temporarily report ImageMaking, but the Agent must not stop or start the source instance")
}

// customImageLiveSource chooses a source only for the explicit opt-in test
// tenant. The regular workflow never selects a source itself; it requires the
// user-provided UHostId that the Agent passes in the chat request above.
func customImageLiveSource(ctx context.Context, executor tools.ToolExecutor, explicitID string) (string, string, error) {
	if explicitID != "" {
		state, err := customImageLiveSourceState(ctx, executor, explicitID)
		return explicitID, state, err
	}
	raw, err := executor.Execute(ctx, "DescribeCompShareInstance", map[string]any{})
	if err != nil {
		return "", "", err
	}
	hosts, _ := raw["UHostSet"].([]any)
	for _, wantedState := range []string{"Stopped", "Running"} {
		for _, item := range hosts {
			host, _ := item.(map[string]any)
			if customImageHostState(host) != wantedState || customImageUnsupportedLiveHost(host) {
				continue
			}
			if id, _ := host["UHostId"].(string); strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id), wantedState, nil
			}
		}
	}
	return "", "", fmt.Errorf("no Running or Stopped source eligible for a custom image was found in the explicitly selected test tenant")
}

func customImageLiveSourceState(ctx context.Context, executor tools.ToolExecutor, sourceID string) (string, error) {
	raw, err := executor.Execute(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds": []any{sourceID}})
	if err != nil {
		return "", err
	}
	hosts, _ := raw["UHostSet"].([]any)
	for _, item := range hosts {
		host, _ := item.(map[string]any)
		if id, _ := host["UHostId"].(string); id == sourceID {
			return customImageHostState(host), nil
		}
	}
	return "", fmt.Errorf("source instance was not returned by DescribeCompShareInstance")
}

func customImageHostState(host map[string]any) string {
	state, _ := host["State"].(string)
	return strings.TrimSpace(state)
}

func customImageUnsupportedLiveHost(host map[string]any) bool {
	if withoutGPU, ok := host["WithoutGpuSpec"].(map[string]any); ok {
		if spec, _ := withoutGPU["Spec"].(string); spec == "A" {
			return true
		}
	}
	return customImageLiveNumber(host["GPU"]) == 0 &&
		customImageLiveNumber(host["CPU"]) == 2 &&
		customImageLiveNumber(host["Memory"]) == 4096
}

func customImageLiveNumber(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		n, _ := v.Float64()
		return n
	default:
		return -1
	}
}

func waitForCustomImageStatus(ctx context.Context, executor tools.ToolExecutor, imageID string) (string, error) {
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		raw, err := executor.Execute(ctx, "DescribeCompShareCustomImages", map[string]any{"CompShareImageId": imageID, "Limit": 1})
		if err == nil {
			if status := customImageStatus(raw, imageID); status != "" {
				return status, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("created custom image was not visible through the custom-image list within the observation window")
		case <-ticker.C:
		}
	}
}

func customImageStatus(raw map[string]any, imageID string) string {
	images, _ := raw["ImageSet"].([]any)
	for _, item := range images {
		image, _ := item.(map[string]any)
		id, _ := image["CompShareImageId"].(string)
		if id != imageID {
			continue
		}
		status, _ := image["Status"].(string)
		return strings.TrimSpace(status)
	}
	return ""
}
