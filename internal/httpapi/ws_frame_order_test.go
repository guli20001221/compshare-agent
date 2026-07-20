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
	"github.com/compshare-agent/internal/turncoord"
	"github.com/gin-gonic/gin"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The forced-first-decision create chain must surface the confirmation CARD
// before any answer prose — a turn that streamed answer text before the card
// would bury it. These two tests are the deterministic frame-ordering gate the
// lead asked for: they exercise the REAL WS handler (HandleWS via httptest) in
// BOTH transport modes and assert the same invariant each mode expresses
// differently:
//
//   - durable (real deploy path after the durable_turns cutover): the projected
//     `confirmation` frame must precede the terminal `done` frame, and durable
//     carries the answer text in the done frame's Content — it emits NO per-token
//     `token` frame, so the assertion is idx(confirmation) < idx(done).
//   - legacy (current deploy default, durable_turns:false): the `confirmation`
//     frame must precede the FIRST streamed `token` frame.
//
// Neither test enables the global COMPSHARE_ENABLE_MUTATING_TOOLS (forbidden per
// CLAUDE.md): the durable gate injects a controllable coordinator event stream,
// and the legacy gate uses the per-engine SetMutatingToolsEnabled lever plus a
// scripted LLM that drives the real forced-first-decision → resolver →
// confirmation round-trip.

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

// TestWSDurable_ConfirmationFramePrecedesDoneFrame is the durable half of the
// frame-order gate. A create turn's coordinator stream projects
// step → interaction.requested(confirmation) → turn.committed(done); the WS
// projection MUST preserve that order so the card is never emitted after the
// terminal answer. Durable carries the answer text in the done frame's Content,
// so there is no `token` frame to race — the assertion is
// idx(confirmation) < idx(done).
func TestWSDurable_ConfirmationFramePrecedesDoneFrame(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		events: []turncoord.Event{
			{
				TurnID: "turn-durable-1", Seq: 1, Type: "turn.running", Provisional: true,
				Payload: json.RawMessage(`{}`),
			},
			{
				TurnID: "turn-durable-1", Seq: 2, Type: "turn.step", Provisional: true,
				Payload: json.RawMessage(`{"action":"RequestCreateInstance","message":"proposing create","type":"tool"}`),
			},
			{
				TurnID: "turn-durable-1", Seq: 3, Type: "interaction.requested", Provisional: true,
				Payload: json.RawMessage(`{"interaction_key":"confirmation/0","kind":"confirmation","payload":{"action":"CreateInstanceWorkflow","summary":"create 4090","form":{}}}`),
			},
			{
				TurnID: "turn-durable-1", Seq: 4, Type: "turn.committed", Provisional: false,
				Payload: json.RawMessage(`{"content":"已为你准备好创建卡片，请确认。","message_id":"assistant-1","committed":true}`),
			},
		},
	}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"client-create","Message":"帮我创建一台4090","Features":["confirm_form_v1","guided_create_v1"]}`,
	)))

	frames := readFrameList(t, conn)
	names := eventNames(frames)

	confirmationIdx := indexOfEvent(frames, "confirmation")
	doneIdx := indexOfEvent(frames, "done")
	stepIdx := indexOfEvent(frames, "step")

	require.NotEqual(t, -1, confirmationIdx, "a create turn must project a confirmation frame; got %v", names)
	require.NotEqual(t, -1, doneIdx, "the turn must terminate with a done frame; got %v", names)
	assert.Less(t, confirmationIdx, doneIdx,
		"the confirmation card MUST precede the terminal done frame (order=%v)", names)
	if stepIdx != -1 {
		assert.Less(t, stepIdx, confirmationIdx,
			"a tool/step frame precedes the confirmation it produced (order=%v)", names)
	}
	assert.Equal(t, -1, indexOfEvent(frames, "token"),
		"durable mode carries the answer in done.Content and emits no per-token frame (order=%v)", names)

	// The confirmation frame carries the card identity the client confirms against;
	// the done frame carries the answer text. Both must survive the projection.
	assert.Equal(t, "confirmation/0", frames[confirmationIdx]["ConfirmationId"])
	assert.Equal(t, "CreateInstanceWorkflow", frames[confirmationIdx]["Action"])
	assert.Equal(t, "已为你准备好创建卡片，请确认。", frames[doneIdx]["Content"])
}

// mutatingProposalLLM scripts the two model turns the legacy gate needs: the
// forced-first-decision probe (call #1) returns exactly one Request* proposal
// tool call; every later call (the answer round after the card is rejected)
// returns plain text with no tool calls. It does not stream, so chatStream emits
// the reply as a single `token` frame.
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
// frame-order gate. It drives a per-engine mutating engine with forced
// first-decision on through the REAL chatStream WS handler: the scripted LLM
// proposes a write, the resolver verifies the instance and chatStream emits a
// `confirmation` frame while blocking in WaitForConfirmation, the test client
// rejects it, and the answer round then streams a `token` frame. The card MUST
// precede the first answer token. START-on-Stopped is used because it reaches a
// plain confirmation via ConfirmFunc (create may route to an intake form via a
// different callback); the create card's own content ordering is covered by the
// durable gate above and the live N=5.
func TestWSLegacy_ConfirmationFramePrecedesTokenFrame(t *testing.T) {
	// forced_first_decision is process-global (boot-only). Set + restore; this
	// test must not run in parallel with anything reading the flag.
	prevForced := engine.ForcedFirstDecisionEnabled()
	engine.SetForcedFirstDecisionEnabled(true)
	t.Cleanup(func() { engine.SetForcedFirstDecisionEnabled(prevForced) })

	llmFake := &mutatingProposalLLM{
		proposalTool: "RequestStartInstance",
		proposalArgs: `{"UHostId":"uhost-1"}`,
		reply:        "已取消开机操作，还需要我做别的吗？",
	}
	eng := engine.NewWithDeps(llmFake, tools.ToolExecutor(startResolvingExecutor{}), denyConfirm)
	eng.SetMutatingToolsEnabled(true)
	eng.RehydrateHistory(nil)

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
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
	h.SetConfirmFormEnabled(true)
	h.SetGuidedCreateEnabled(true)

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
