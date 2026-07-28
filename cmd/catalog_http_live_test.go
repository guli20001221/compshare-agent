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
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestCatalogAndChargeAcceptanceOverRealHTTP exercises the same HTTP/WebSocket
// handler, real answer model, real retrieval stack and real CompShare executor
// as the server. It uses only in-memory conversation persistence so a database
// outage cannot turn a catalog/Prompt acceptance run into a false failure.
//
// Run:
//
//	COMPSHARE_CATALOG_HTTP_LIVE=1 go test ./cmd -tags live \
//	  -run TestCatalogAndChargeAcceptanceOverRealHTTP -count=1 -v -timeout 20m
func TestCatalogAndChargeAcceptanceOverRealHTTP(t *testing.T) {
	if os.Getenv("COMPSHARE_CATALOG_HTTP_LIVE") != "1" {
		t.Skip("set COMPSHARE_CATALOG_HTTP_LIVE=1 to run real HTTP/model/catalog acceptance")
	}

	root := behavioralRepoRoot(t)
	originalDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	cfg, err := config.Load(filepath.Join(root, "deploy", "conf", "config.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Agent.LLM.APIKey)
	cfg.Agent.ProjectId = ""
	// Community catalog reads do not need tenant-scoped resources. Use the
	// configured service credential directly so this gate remains independent
	// of whether the chosen test tenant has already provisioned its STS role.
	cfg.Agent.PublicKey = cfg.Agent.STS.ServiceAK
	cfg.Agent.PrivateKey = cfg.Agent.STS.ServiceSK
	cfg.Agent.STS.ServiceAK = ""
	cfg.Agent.STS.ServiceSK = ""

	messages := serverTestMessageStore{}
	pool, err := buildHTTPServerPool(cfg, messages, cfg.RuntimeGetenv(os.Getenv), nil)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sessions := newCatalogLiveSessions()
	handlers := newServerHandlers(
		cfg, sessions, messages, nil, pool, nil, cfg.RuntimeGetenv(os.Getenv),
	)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", handlers.HandleWS)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	catalogRuns := 5
	if os.Getenv("COMPSHARE_CATALOG_HTTP_LIVE_ONLY_CHARGE") == "1" {
		catalogRuns = 0
	}
	for i := 1; i <= catalogRuns; i++ {
		sessionID := fmt.Sprintf("catalog-http-live-%d", i)
		sessions.add(sessionID, 2384301, 2384302)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		headers := http.Header{}
		headers.Set("X-Company-Id", "2384301")
		headers.Set("X-Organization-Id", "2384302")
		headers.Set("X-Account-Id", sessionID)
		headers.Set("X-Request-Id", sessionID+"-request")
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/?Action=CreateCSAgentWS"
		conn, _, dialErr := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
		require.NoError(t, dialErr)

		frame, marshalErr := json.Marshal(map[string]any{
			"Action":       "SendCSAgentChat",
			"SessionId":    sessionID,
			"Message":      "为我推荐一个做数字人的镜像",
			"request_uuid": sessionID + "-turn",
		})
		require.NoError(t, marshalErr)
		require.NoError(t, conn.Write(ctx, websocket.MessageText, frame))

		var reply string
		var actions []string
		var stepDetails []string
		var confirmations int
		for {
			_, raw, readErr := conn.Read(ctx)
			require.NoError(t, readErr)
			var event map[string]any
			require.NoError(t, json.Unmarshal(raw, &event))
			switch event["event"] {
			case "step":
				if action, _ := event["Action"].(string); action != "" {
					actions = append(actions, action)
					stepDetails = append(stepDetails, action+" | "+stringField(event, "Type")+" | "+stringField(event, "Message"))
				}
			case "confirmation":
				confirmations++
			case "done":
				reply, _ = event["Content"].(string)
				goto complete
			case "error":
				t.Fatalf("session %d error frame: %s", i, string(raw))
			}
		}

	complete:
		_ = conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		t.Logf("session=%d actions=%v steps=%v reply=%s", i, actions, stepDetails, reply)
		require.Contains(t, actions, "DescribeCommunityImages", "session %d must query the live community catalog", i)
		require.Contains(t, reply, "InfiniteTalk", "session %d reply=%s", i, reply)
		require.NotContains(t, actions, "RequestCreateInstance", "recommendation must not become a create request")
		require.Zero(t, confirmations, "recommendation must stay read-only")
	}
	if os.Getenv("COMPSHARE_CATALOG_HTTP_LIVE_ONLY_CATALOG") == "1" {
		return
	}

	chargeCases := []struct {
		name             string
		message          string
		wantCharge       string
		requireCardSkip  bool
		allowChosenValue bool
	}{
		{name: "postpay", message: "按量创建一台 4090", wantCharge: "Postpay", requireCardSkip: true},
		{name: "day", message: "按天创建一台 4090", wantCharge: "Day", requireCardSkip: true},
		{name: "month", message: "按月创建一台 4090", wantCharge: "Month", requireCardSkip: true},
		{name: "spot", message: "抢占式创建一台 4090", wantCharge: "Spot", requireCardSkip: true},
		{name: "negated", message: "我不要包月，用按量创建一台 4090", wantCharge: "Postpay", allowChosenValue: true},
		{name: "comparison", message: "包月和按量哪个便宜？先建一台 4090"},
		{name: "reported-opinion", message: "同事说包月划算，但我先随便建一台 4090"},
	}
	chargeDeps, mutating, err := configureSharedDepsFromEnv(cfg, cfg.RuntimeGetenv(os.Getenv), nil)
	require.NoError(t, err)
	chargeDeps.ExternalExecutor = catalogChargeExecutor{}
	chargePool := agentpool.NewWithDeps(chargeDeps, messages, agentpool.Options{
		Capacity: cfg.Agent.HTTP.PoolCapacity, IdleTTL: cfg.Agent.HTTP.PoolIdleTTL,
		MutatingToolsEnabled: mutating,
	})
	t.Cleanup(chargePool.Close)
	chargeHandlers := newServerHandlers(
		cfg, sessions, messages, nil, chargePool, nil, cfg.RuntimeGetenv(os.Getenv),
	)
	chargeRouter := gin.New()
	chargeRouter.GET("/", chargeHandlers.HandleWS)
	chargeServer := httptest.NewServer(chargeRouter)
	t.Cleanup(chargeServer.Close)

	for i, tc := range chargeCases {
		sessionID := fmt.Sprintf("charge-http-live-%d", i+1)
		sessions.add(sessionID, 2384301, 2384302)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		headers := http.Header{}
		headers.Set("X-Company-Id", "2384301")
		headers.Set("X-Organization-Id", "2384302")
		headers.Set("X-Account-Id", sessionID)
		headers.Set("X-Request-Id", sessionID+"-request")
		wsURL := "ws" + strings.TrimPrefix(chargeServer.URL, "http") + "/?Action=CreateCSAgentWS"
		conn, _, dialErr := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
		require.NoError(t, dialErr)

		frame, marshalErr := json.Marshal(map[string]any{
			"Action":       "SendCSAgentChat",
			"SessionId":    sessionID,
			"Message":      tc.message,
			"request_uuid": sessionID + "-turn",
			"Features":     []string{"confirm_form_v1", "guided_create_v1"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, conn.Write(ctx, websocket.MessageText, frame))

		var reply string
		var firstForm any
		var firstSummary map[string]any
		confirmationRounds := 0
		for {
			_, raw, readErr := conn.Read(ctx)
			require.NoError(t, readErr)
			var event map[string]any
			require.NoError(t, json.Unmarshal(raw, &event))
			switch event["event"] {
			case "confirmation":
				confirmationRounds++
				form := event["Form"]
				summary, _ := event["Summary"].(map[string]any)
				summaryCharge, _ := summary["ChargeType"].(string)
				stopBeforeWrite := form == nil || formContainsField(form, "ChargeType") || summaryCharge != "" ||
					confirmationRounds >= 12
				if stopBeforeWrite {
					firstForm = event["Form"]
					firstSummary = summary
				}
				confirmationID, _ := event["ConfirmationId"].(string)
				decline, err := json.Marshal(map[string]any{
					"Action": "ConfirmCSAgentAction", "SessionId": sessionID,
					"ConfirmationId": confirmationID, "Confirmed": !stopBeforeWrite,
				})
				require.NoError(t, err)
				require.NoError(t, conn.Write(ctx, websocket.MessageText, decline))
			case "done":
				reply, _ = event["Content"].(string)
				goto chargeComplete
			case "error":
				t.Fatalf("%s error frame: %s", tc.name, string(raw))
			}
		}

	chargeComplete:
		_ = conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		cardOffersCharge := formContainsField(firstForm, "ChargeType")
		chosenCharge, _ := firstSummary["ChargeType"].(string)
		t.Logf("charge=%s card_offers_charge=%t chosen=%s form=%v summary=%v reply=%s",
			tc.name, cardOffersCharge, chosenCharge, firstForm, firstSummary, reply)

		if tc.requireCardSkip {
			require.NotNil(t, firstSummary, "%s should reach a guided confirmation card", tc.name)
			require.False(t, cardOffersCharge, "%s explicitly selected a charge type; its card should be skipped", tc.name)
			require.Equal(t, tc.wantCharge, chosenCharge)
			continue
		}
		if firstSummary == nil {
			continue // asking the user to choose is also safe
		}
		if cardOffersCharge {
			continue // unresolved choice stays editable
		}
		if tc.allowChosenValue {
			require.Equal(t, tc.wantCharge, chosenCharge, "%s must not select the negated charge type", tc.name)
			continue
		}
		t.Fatalf("%s silently skipped the charge card with %q", tc.name, chosenCharge)
	}
}

type catalogChargeExecutor struct{}

func (catalogChargeExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{
				"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}},
			},
		}}, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{
			map[string]any{
				"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001),
				"ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false,
			},
		}}, nil
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{
				"CompShareImageId": "img-pytorch", "Name": "PyTorch", "Status": "Available",
				"ImageType": "App", "Zone": "cn-wlcb-01",
			},
		}}, nil
	case "CheckCompShareResourceCapacity":
		return map[string]any{"Specs": []any{
			map[string]any{
				"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true,
			},
		}}, nil
	case "GetCompShareInstanceUserPrice":
		return map[string]any{"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": 1.58},
			map[string]any{"ChargeType": "Dynamic", "Instance": 1.58},
			map[string]any{"ChargeType": "Day", "Instance": 34.9},
			map[string]any{"ChargeType": "Month", "Instance": 951.85},
			map[string]any{"ChargeType": "Spot", "Instance": 1.1},
		}}, nil
	default:
		return map[string]any{"RetCode": 0}, nil
	}
}

func stringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func formContainsField(value any, name string) bool {
	switch typed := value.(type) {
	case map[string]any:
		fieldName, _ := typed["Name"].(string)
		fieldKey, _ := typed["Key"].(string)
		if fieldName == name || fieldKey == name {
			return true
		}
		for _, child := range typed {
			if formContainsField(child, name) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if formContainsField(child, name) {
				return true
			}
		}
	}
	return false
}

type catalogLiveSessions struct {
	mu     sync.Mutex
	byID   map[string]store.Session
	nextID int
}

func newCatalogLiveSessions() *catalogLiveSessions {
	return &catalogLiveSessions{byID: map[string]store.Session{}}
}

func (s *catalogLiveSessions) add(id string, top, org uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id] = store.Session{
		ID: id, TopOrganizationID: top, OrganizationID: org,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func (s *catalogLiveSessions) Create(_ context.Context, owner store.Owner, title *string, ctx json.RawMessage) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("catalog-live-created-%d", s.nextID)
	session := store.Session{
		ID: id, TopOrganizationID: owner.TopOrganizationID, OrganizationID: owner.OrganizationID,
		Title: title, Context: ctx, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.byID[id] = session
	return session, nil
}

func (s *catalogLiveSessions) GetByID(_ context.Context, owner store.Owner, id string) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byID[id]
	if !ok || session.TopOrganizationID != owner.TopOrganizationID || session.OrganizationID != owner.OrganizationID {
		return store.Session{}, sql.ErrNoRows
	}
	return session, nil
}

func (s *catalogLiveSessions) BumpUpdatedAtAndIncCount(_ context.Context, owner store.Owner, id string, delta int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byID[id]
	if !ok || session.TopOrganizationID != owner.TopOrganizationID || session.OrganizationID != owner.OrganizationID {
		return sql.ErrNoRows
	}
	session.UpdatedAt = time.Now()
	session.MessageCount += delta
	s.byID[id] = session
	return nil
}

func (s *catalogLiveSessions) UpdateContext(_ context.Context, owner store.Owner, id string, ctx json.RawMessage, expected int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byID[id]
	if !ok || session.TopOrganizationID != owner.TopOrganizationID ||
		session.OrganizationID != owner.OrganizationID || session.ContextVersion != expected {
		return 0, store.ErrStaleWrite
	}
	session.Context = append(json.RawMessage(nil), ctx...)
	session.ContextVersion++
	s.byID[id] = session
	return session.ContextVersion, nil
}

func (s *catalogLiveSessions) ListByOwner(_ context.Context, owner store.Owner, limit int) ([]store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Session, 0, limit)
	for _, session := range s.byID {
		if session.TopOrganizationID == owner.TopOrganizationID && session.OrganizationID == owner.OrganizationID {
			out = append(out, session)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *catalogLiveSessions) SetTitleIfEmpty(_ context.Context, owner store.Owner, id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byID[id]
	if ok && session.TopOrganizationID == owner.TopOrganizationID &&
		session.OrganizationID == owner.OrganizationID && session.Title == nil {
		session.Title = &title
		s.byID[id] = session
	}
	return nil
}
