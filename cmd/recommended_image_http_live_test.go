//go:build live

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestRecommendedImageCarryOverRealHTTP is the reported production-shaped
// acceptance: a prior real recommendation supplies an exact community-image id,
// then the user restates its readable shorthand and starts a create flow.  It
// drives the production HTTP/WebSocket handler, real answer model and live
// community catalog for the follow-up. The test declines the first guided card,
// before any create call.
//
// The controlled historical reply keeps this regression independent of the
// model's mutable recommendation ranking. TestCatalogAndChargeAcceptanceOverRealHTTP
// separately covers a live recommendation; this test must prove that the exact
// ID printed in the reported historical reply remains usable when the follow-up
// uses a whitespace-normalized shorthand.
//
// Run:
//
//	COMPSHARE_RECOMMENDED_IMAGE_HTTP_LIVE=1 go test ./cmd -tags live \
//	  -run TestRecommendedImageCarryOverRealHTTP -count=1 -v -timeout 12m
func TestRecommendedImageCarryOverRealHTTP(t *testing.T) {
	if os.Getenv("COMPSHARE_RECOMMENDED_IMAGE_HTTP_LIVE") != "1" {
		t.Skip("set COMPSHARE_RECOMMENDED_IMAGE_HTTP_LIVE=1 to run real HTTP/model/catalog acceptance")
	}

	root := behavioralRepoRoot(t)
	originalDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	cfg, err := config.Load(filepath.Join(root, "deploy", "conf", "config.local.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Agent.LLM.APIKey)
	const (
		topOrganizationID = 2384301
		organizationID    = 2384302
		infiniteTalkID    = "compshareImage-1mefk6bv35xn"
	)
	sessionID := fmt.Sprintf("recommended-image-http-live-%d", time.Now().UnixNano())
	// This workstation's local STS test role may read the community catalog but
	// cannot read the hardware catalog (RetCode=299). The latter is not the
	// subject of this gate and is only a prerequisite to rendering the first
	// guided image card, so preserve the production HTTP/model path and replace
	// only those unrelated hardware reads with the existing deterministic test
	// catalog. The carried image itself is still revalidated against the LIVE
	// community API; any write action fails closed in imageCarryHTTPExecutor.
	liveCatalogConfig := *cfg
	liveCatalogConfig.Agent.ProjectId = ""
	liveCatalogConfig.Agent.PublicKey = cfg.Agent.STS.ServiceAK
	liveCatalogConfig.Agent.PrivateKey = cfg.Agent.STS.ServiceSK
	liveCatalogConfig.Agent.STS.ServiceAK = ""
	liveCatalogConfig.Agent.STS.ServiceSK = ""
	deps, mutating, err := configureSharedDepsFromEnv(cfg, cfg.RuntimeGetenv(os.Getenv), nil)
	require.NoError(t, err)
	deps.ExternalExecutor = imageCarryHTTPExecutor{
		liveCommunityCatalog: tools.NewExternalExecutor(liveCatalogConfig.Agent),
	}

	messages := newRecommendedImageHistoryStore(sessionID, infiniteTalkID)
	pool := agentpool.NewWithDeps(deps, messages, agentpool.Options{
		Capacity: cfg.Agent.HTTP.PoolCapacity, IdleTTL: cfg.Agent.HTTP.PoolIdleTTL,
		MutatingToolsEnabled: mutating,
	})
	t.Cleanup(pool.Close)

	sessions := newCatalogLiveSessions()
	sessions.add(sessionID, topOrganizationID, organizationID)
	// The persisted pair above is historical conversation, so match the session
	// count the production store would have after that completed first turn.
	sessions.mu.Lock()
	historicalSession := sessions.byID[sessionID]
	historicalSession.MessageCount = 2
	sessions.byID[sessionID] = historicalSession
	sessions.mu.Unlock()
	handlers := newServerHandlers(cfg, sessions, messages, nil, pool, nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", handlers.HandleWS)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/?Action=CreateCSAgentWS"
	dial := func(t *testing.T, requestID string) (*websocket.Conn, context.Context, context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		headers := http.Header{}
		headers.Set("X-Company-Id", fmt.Sprint(topOrganizationID))
		headers.Set("X-Organization-Id", fmt.Sprint(organizationID))
		headers.Set("X-Account-Id", sessionID)
		headers.Set("X-Request-Id", requestID)
		conn, _, dialErr := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
		require.NoError(t, dialErr)
		return conn, ctx, cancel
	}
	sendChat := func(t *testing.T, conn *websocket.Conn, ctx context.Context, requestID, message string, features []string) {
		t.Helper()
		frame, marshalErr := json.Marshal(map[string]any{
			"Action":       "SendCSAgentChat",
			"SessionId":    sessionID,
			"Message":      message,
			"request_uuid": requestID,
			"Features":     features,
		})
		require.NoError(t, marshalErr)
		require.NoError(t, conn.Write(ctx, websocket.MessageText, frame))
	}

	// The reported regression used a human-readable shorthand with whitespace;
	// the full upstream label in the prior reply has none.  This second turn uses
	// a fresh socket, exactly like the console's one-turn-per-WebSocket protocol.
	createConn, createCtx, createCancel := dial(t, sessionID+"-create")
	defer createCancel()
	defer createConn.Close(websocket.StatusNormalClosure, "")
	sendChat(t, createConn, createCtx, sessionID+"-create-turn",
		"最强 AI 数字人 InfiniteTalk，用这个镜像为我创建机器",
		[]string{"confirm_form_v1", "guided_create_v1"})

	var createActions []string
	var firstForm any
	var firstFormJSON string
	var reply string
	confirmations := 0
	for {
		_, raw, readErr := createConn.Read(createCtx)
		require.NoError(t, readErr)
		var event map[string]any
		require.NoError(t, json.Unmarshal(raw, &event))
		switch event["event"] {
		case "step":
			if action := stringField(event, "Action"); action != "" {
				createActions = append(createActions, action)
			}
		case "confirmation":
			confirmations++
			if confirmations != 1 {
				t.Fatalf("expected the first guided card to be declined, got confirmation #%d", confirmations)
			}
			firstForm = event["Form"]
			formJSON, marshalErr := json.Marshal(firstForm)
			require.NoError(t, marshalErr)
			firstFormJSON = string(formJSON)
			confirmationID := stringField(event, "ConfirmationId")
			require.NotEmpty(t, confirmationID)
			decline, marshalErr := json.Marshal(map[string]any{
				"Action": "ConfirmCSAgentAction", "SessionId": sessionID,
				"ConfirmationId": confirmationID, "Confirmed": false,
			})
			require.NoError(t, marshalErr)
			require.NoError(t, createConn.Write(createCtx, websocket.MessageText, decline))
		case "done":
			reply = stringField(event, "Content")
			goto createComplete
		case "error":
			t.Fatalf("create follow-up returned HTTP/WS error: %s", string(raw))
		}
	}

createComplete:
	t.Logf("create actions=%v first_form=%s reply=%s", createActions, firstFormJSON, reply)
	require.Contains(t, createActions, "ProposeAction",
		"the model must start the workflow instead of asking the user to restate the recommendation")
	require.Contains(t, createActions, "DescribeCommunityImages",
		"the carried id must be revalidated against the live community catalog")
	require.NotNil(t, firstForm, "the create flow must reach a confirmation-gated card")
	require.False(t, formContainsField(firstForm, "ImageSource"),
		"a carried exact community-image id must skip the redundant source card")
	require.True(t, formContainsField(firstForm, "ImageId"),
		"the user must still receive the concrete-image picker before any write")
	require.Contains(t, firstFormJSON, infiniteTalkID,
		"the picker must retain and preselect the exact recommended image id")
	require.NotContains(t, createActions, "CreateCompShareInstance",
		"the acceptance must stop at the first card and never create a machine")
	require.Equal(t, 1, confirmations)
}

// recommendedImageHistoryStore is only the controlled precondition for the
// opt-in live gate above. Its follow-up rows are fully recorded so a pool miss
// remains a faithful cold reconstruction rather than accidentally relying on a
// hot in-memory Engine.
type recommendedImageHistoryStore struct {
	mu       sync.Mutex
	session  string
	messages []store.Message
}

func newRecommendedImageHistoryStore(sessionID, imageID string) *recommendedImageHistoryStore {
	return &recommendedImageHistoryStore{
		session: sessionID,
		messages: []store.Message{
			{
				ID: sessionID + "-historic-user", SessionID: sessionID, Role: "user", Status: "ok",
				Content: "我想做数字人，为我推荐镜像。",
			},
			{
				ID: sessionID + "-historic-assistant", SessionID: sessionID, Role: "assistant", Status: "ok",
				Content: "首选社区镜像：最强AI数字人InfiniteTalk-图片和视频数字人。镜像 ID：" + imageID + "。",
			},
		},
	}
}

func (s *recommendedImageHistoryStore) Append(_ context.Context, message store.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
	return nil
}

func (s *recommendedImageHistoryStore) UpdateAssistant(_ context.Context, _ store.Owner, messageID string, patch store.AssistantPatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ID == messageID {
			s.messages[i].Content = patch.Content
			s.messages[i].Status = patch.Status
			s.messages[i].ErrorCode = patch.ErrorCode
			return nil
		}
	}
	return sql.ErrNoRows
}

func (s *recommendedImageHistoryStore) ListBySession(_ context.Context, sessionID string, limit int, _ string) ([]store.Message, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID != s.session || limit <= 0 {
		return nil, "", nil
	}
	messages := append([]store.Message(nil), s.messages...)
	if len(messages) > limit {
		messages = messages[:limit]
	}
	return messages, "", nil
}

func (s *recommendedImageHistoryStore) GetWithOwnerCheck(_ context.Context, _ store.Owner, _ string) (store.Message, error) {
	return store.Message{}, sql.ErrNoRows
}

// imageCarryHTTPExecutor is intentionally narrow: it makes the exact
// community-image resolution live, supplies only the hardware prerequisites
// required to reach an image card, and refuses every unlisted action. In
// particular, the test can never write an instance even if its confirmation
// handling regresses.
type imageCarryHTTPExecutor struct {
	liveCommunityCatalog tools.ToolExecutor
}

func (e imageCarryHTTPExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeCommunityImages":
		return e.liveCommunityCatalog.Execute(ctx, action, args)
	case "DescribeAvailableCompShareInstanceTypes", "DescribeCompShareSupportZone",
		"DescribeCompShareImages", "CheckCompShareResourceCapacity", "GetCompShareInstanceUserPrice":
		return (catalogChargeExecutor{}).Execute(ctx, action, args)
	case "DescribeCompShareGpuInventory":
		return map[string]any{"GpuInventory": map[string]any{
			"Exclusive": map[string]any{}, "Spot": map[string]any{},
		}}, nil
	default:
		return nil, fmt.Errorf("live image-carry acceptance refused unexpected action %q", action)
	}
}
