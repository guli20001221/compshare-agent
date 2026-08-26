package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A create/write turn must surface the confirmation card before answer prose;
// otherwise streamed text can bury the decision the user must make. The test
// below drives the real WS handler and ReAct proposal → resolver → confirmation
// path.

// indexOfEvent returns the position of the first frame whose "event" == name,
// or -1 when absent.
func indexOfEvent(frames []map[string]any, name string) int {
	for i, f := range frames {
		if ev, _ := f["event"].(string); ev == name {
			return i
		}
	}
	return -1
}

// eventNames projects the ordered "event" discriminators for readable failures.
func eventNames(frames []map[string]any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		ev, _ := f["event"].(string)
		out = append(out, ev)
	}
	return out
}

// mutatingProposalLLM scripts the two model turns the gate needs: the
// first ReAct round (call #1) returns exactly one Request* proposal tool call;
// every later call (the answer round after the card is rejected) returns plain
// text with no tool calls. It does not stream, so chatStream emits the reply as a
// single `token` frame.
type mutatingProposalLLM struct {
	mu           sync.Mutex
	calls        int
	proposalTool string
	proposalArgs string
	reply        string
}

func (m *mutatingProposalLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{{
			ID:       "call-1",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: m.proposalTool, Arguments: m.proposalArgs},
		}}}, nil
	}
	return &llm.ChatResponse{Content: m.reply}, nil
}

// startResolvingExecutor returns a real, Stopped instance for the START
// resolver's existence check so the proposal reaches ReadyForConfirmation (the
// same executor shape TestP7StartTargetRecordingOffline uses). Every mutating
// action would be a no-op here because the confirmation is rejected before
// execution.
type startResolvingExecutor struct{}

func (startResolvingExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"UHostSet": []any{map[string]any{
			"UHostId": "uhost-1", "Name": "host", "State": "Stopped",
			"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ChargeType": "Postpay", "GpuType": "4090",
		}}}, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{map[string]any{
			"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "IsPod": false,
		}}}, nil
	default:
		return map[string]any{"RetCode": float64(0)}, nil
	}
}

func readOneFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	var f map[string]any
	require.NoError(t, json.Unmarshal(raw, &f))
	return f
}

// TestWSLegacy_ConfirmationFramePrecedesTokenFrame is the legacy half of the
// frame-order gate. It drives a per-engine mutating engine through the REAL
// chatStream WS handler: the scripted LLM's first round proposes a write, the
// resolver verifies the instance and chatStream emits a `confirmation` frame
// while blocking in WaitForConfirmation, the test client rejects it, and the
// answer round then streams a `token` frame. The card MUST precede the first
// answer token. START-on-Stopped is used because it reaches a plain confirmation
// via ConfirmFunc (create may route to an intake form via a different callback);
// the create card's own content ordering is covered by the confirmation gate above and
// the live N=5.
func TestWSLegacy_ConfirmationFramePrecedesTokenFrame(t *testing.T) {
	llmFake := &mutatingProposalLLM{
		proposalTool: "RequestStartInstance",
		proposalArgs: `{"UHostId":"uhost-1","StartMode":"normal"}`,
		reply:        "已取消开机操作，还需要我做别的吗？",
	}
	eng := engine.NewWithDeps(llmFake, tools.ToolExecutor(startResolvingExecutor{}), denyConfirm)
	eng.SetMutatingToolsEnabled(true)
	eng.RehydrateHistory(nil)

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-1": {
				ID: "sess-1", TopOrganizationID: 1, OrganizationID: 2,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			},
		}},
		&recordingMessages{},
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", h.HandleWS)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","SessionId":"sess-1","Message":"帮我开机 uhost-1","Features":["confirm_form_v1","guided_create_v1"]}`,
	)))

	// Phase 1: read up to and including the confirmation frame. The turn is
	// blocked in WaitForConfirmation, so no token can have been emitted yet.
	var frames []map[string]any
	var confirmationID string
	for {
		f := readOneFrame(t, ctx, conn)
		frames = append(frames, f)
		switch f["event"] {
		case "confirmation":
			confirmationID, _ = f["ConfirmationId"].(string)
		case "done", "error":
			t.Fatalf("turn ended before emitting a confirmation frame: %v (%v)", eventNames(frames), f)
		}
		if confirmationID != "" {
			break
		}
	}
	require.NotEmpty(t, confirmationID, "confirmation frame must carry a ConfirmationId")

	// Reject the card on the same socket — the read loop routes it to the broker,
	// unblocking the turn, which then streams the answer.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(
		`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmationID+`","Confirmed":false}`,
	)))

	// Phase 2: read the remaining frames through the terminal done/error.
	for {
		f := readOneFrame(t, ctx, conn)
		frames = append(frames, f)
		if f["event"] == "done" || f["event"] == "error" {
			break
		}
	}

	names := eventNames(frames)
	confIdx := indexOfEvent(frames, "confirmation")
	tokenIdx := indexOfEvent(frames, "token")
	require.NotEqual(t, -1, confIdx, "a confirmation frame is required; got %v", names)
	require.NotEqual(t, -1, tokenIdx, "a streamed answer token must follow the rejected card; got %v", names)
	assert.Less(t, confIdx, tokenIdx,
		"the confirmation card MUST precede the first answer token (order=%v)", names)
}
