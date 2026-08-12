package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDurableCoordinator struct {
	mu sync.Mutex

	turn       store.Turn
	events     []turncoord.Event
	submits    []turncoord.SubmitInput
	lastSeq    int64
	abortCalls int
	resolved   []fakeResolvedInteraction
	resolveErr error
	// imageIdentityGate emulates the coordinator's persisted request hash at
	// the transport seam so these tests isolate raw-image digest wiring.
	imageIdentityGate bool
	firstImageDigest  string
}

type committedPageMessages struct {
	mockMessages
	committed []store.Message
	total     int
}

func (m *committedPageMessages) ListCommittedBySession(_ context.Context, _ store.Owner, _ string, _ int, _ string) ([]store.Message, string, int, error) {
	return append([]store.Message(nil), m.committed...), "", m.total, nil
}

type fakeResolvedInteraction struct {
	owner  store.Owner
	turnID string
	key    string
	value  turncoord.ConfirmationResponse
}

func (f *fakeDurableCoordinator) Submit(_ context.Context, in turncoord.SubmitInput, _ turncoord.EventSink) (turncoord.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imageIdentityGate && len(f.submits) > 0 && in.ImageDigest != f.firstImageDigest {
		return turncoord.Submission{}, store.ErrIdempotencyConflict
	}
	if len(f.submits) == 0 {
		f.firstImageDigest = in.ImageDigest
	}
	f.submits = append(f.submits, in)
	turn := f.turn
	turn.Owner = in.Owner
	turn.SessionID = in.SessionID
	turn.ClientTurnID = in.ClientTurnID
	if turn.ID == "" {
		turn.ID = "turn-durable-1"
	}
	if turn.Status == "" {
		turn.Status = store.TurnStatusAccepted
	}
	f.turn = turn
	return turncoord.Submission{Turn: turn, Disposition: turncoord.DispositionStarted}, nil
}

type sequenceOCR struct {
	mu      sync.Mutex
	outputs []string
}

func (o *sequenceOCR) Recognize(_ context.Context, _ string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.outputs) == 0 {
		return "", nil
	}
	value := o.outputs[0]
	o.outputs = o.outputs[1:]
	return value, nil
}

func (f *fakeDurableCoordinator) Subscribe(_ context.Context, _ store.Owner, _ string, lastSeq int64, sink turncoord.EventSink) error {
	f.mu.Lock()
	f.lastSeq = lastSeq
	events := append([]turncoord.Event(nil), f.events...)
	f.mu.Unlock()
	for _, event := range events {
		if event.Seq > lastSeq {
			if err := sink(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeDurableCoordinator) GetTurn(_ context.Context, owner store.Owner, turnID string) (store.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.turn.ID != turnID || f.turn.Owner != owner {
		return store.Turn{}, store.ErrTurnNotFound
	}
	return f.turn, nil
}

func (f *fakeDurableCoordinator) FindTurnByClientID(_ context.Context, owner store.Owner, sessionID, clientTurnID string) (store.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.turn.Owner != owner || f.turn.SessionID != sessionID || f.turn.ClientTurnID != clientTurnID {
		return store.Turn{}, store.ErrTurnNotFound
	}
	return f.turn, nil
}

func (f *fakeDurableCoordinator) ResolveInteraction(_ context.Context, owner store.Owner, turnID, key string, value turncoord.ConfirmationResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return f.resolveErr
	}
	f.resolved = append(f.resolved, fakeResolvedInteraction{owner: owner, turnID: turnID, key: key, value: value})
	return nil
}

func (f *fakeDurableCoordinator) AbortTurn(_ context.Context, _ store.Owner, _ string) (store.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortCalls++
	f.turn.Status = store.TurnStatusAborted
	return f.turn, nil
}

func durableWSTestServerWithOCR(t *testing.T, coordinator *fakeDurableCoordinator, recognizer OCRRecognizer) *httptest.Server {
	t.Helper()
	sessions := &mockSessions{byID: map[string]store.Session{
		"sess-1": {
			ID: "sess-1", TopOrganizationID: 1, OrganizationID: 2,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
	}}
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000},
			OCR:  config.OCRConfig{MaxBytes: 1024, Timeout: time.Second},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions, &mockMessages{}, mockFeedback{}, nil, nil,
	)
	h.SetOCRClient(recognizer)
	h.SetTurnCoordinator(coordinator)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", h.HandleWS)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func durableWSTestServer(t *testing.T, coordinator *fakeDurableCoordinator) *httptest.Server {
	return durableWSTestServerWithOCR(t, coordinator, nil)
}

func readDurableFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	var frame map[string]any
	require.NoError(t, json.Unmarshal(raw, &frame))
	return frame
}

func TestWSDurable_LegacyAndV2SendUseTheSameCoordinator(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		events: []turncoord.Event{{
			TurnID: "turn-durable-1", Seq: 1, Type: "turn.committed", Provisional: false,
			Payload: json.RawMessage(`{"content":"saved answer","message_id":"assistant-1","committed":true}`),
		}},
	}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","SessionId":"sess-1","Message":"hi"}`,
	)))
	meta := readDurableFrame(t, conn)
	done := readDurableFrame(t, conn)
	assert.Equal(t, "meta", meta["event"])
	assert.Equal(t, "turn-durable-1", meta["TurnId"])
	assert.Equal(t, "done", done["event"])
	assert.Equal(t, "saved answer", done["Content"])
	assert.Equal(t, float64(1), done["Seq"])
	coordinator.mu.Lock()
	require.Len(t, coordinator.submits, 1)
	assert.Equal(t, "req-ws-1", coordinator.submits[0].ClientTurnID, "legacy request id is the idempotency key")
	coordinator.mu.Unlock()
}

func TestWSDurable_ResumeByClientIDReplaysStrictlyAfterLastSeq(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		turn: store.Turn{
			ID: "turn-2", SessionID: "sess-1", ClientTurnID: "client-2", Owner: gatewayOwner,
			Status: store.TurnStatusCommitted, NextEventSeq: 4,
		},
		events: []turncoord.Event{{
			TurnID: "turn-2", Seq: 3, Type: "turn.committed", Provisional: false,
			Payload: json.RawMessage(`{"content":"replayed","committed":true}`),
		}},
	}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"ResumeCSAgentTurn","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"client-2","LastSeq":3}`,
	)))
	meta := readDurableFrame(t, conn)
	assert.NotContains(t, meta, "Status", "transport metadata must not announce terminal state before the answer event is replayed")
	assert.Equal(t, "committed", meta["ServerStatus"])
	done := readDurableFrame(t, conn)
	assert.Equal(t, "done", done["event"])
	coordinator.mu.Lock()
	assert.Equal(t, int64(2), coordinator.lastSeq, "terminal frame is replayed even if the client lost only its local status transition")
	coordinator.mu.Unlock()
}

func TestWSDurable_ConfirmationIsBoundToTurnAndInteraction(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		events: []turncoord.Event{{
			TurnID: "turn-durable-1", Seq: 1, Type: "interaction.requested", Provisional: true,
			Payload: json.RawMessage(`{"interaction_key":"confirmation/0","kind":"confirmation","payload":{"action":"StopInstance"}}`),
		}},
	}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"client-confirm","Message":"stop it"}`,
	)))
	_ = readDurableFrame(t, conn)
	requested := readDurableFrame(t, conn)
	assert.Equal(t, "confirmation", requested["event"])
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","TurnId":"turn-durable-1","InteractionKey":"confirmation/0","Confirmed":true}`,
	)))
	require.Eventually(t, func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return len(coordinator.resolved) == 1
	}, time.Second, 10*time.Millisecond)
	coordinator.mu.Lock()
	assert.Equal(t, "turn-durable-1", coordinator.resolved[0].turnID)
	assert.Equal(t, "confirmation/0", coordinator.resolved[0].key)
	assert.True(t, coordinator.resolved[0].value.Confirmed)
	coordinator.mu.Unlock()
}

func TestWSDurable_ForwardsClientEditsToDurableValidation(t *testing.T) {
	coordinator := &fakeDurableCoordinator{turn: store.Turn{ID: "turn-1", SessionID: "sess-1", Owner: gatewayOwner}}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","TurnId":"turn-1","InteractionKey":"confirmation/0","Confirmed":true,"Overrides":{"Zone":"cn-bj2-02"}}`,
	)))
	require.Eventually(t, func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return len(coordinator.resolved) == 1
	}, time.Second, 10*time.Millisecond)
	coordinator.mu.Lock()
	assert.Equal(t, map[string]string{"Zone": "cn-bj2-02"}, coordinator.resolved[0].value.Overrides)
	coordinator.mu.Unlock()
}

func TestWSDurable_RejectedEditKeepsInteractionOpenForCorrection(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		turn:       store.Turn{ID: "turn-1", SessionID: "sess-1", Owner: gatewayOwner},
		resolveErr: fmt.Errorf("%w: value is not one of the reviewed options", store.ErrInvalidArgument),
	}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	rejected := `{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","TurnId":"turn-1","InteractionKey":"confirmation/0","Confirmed":true,"Overrides":{"Zone":"invalid"}}`
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(rejected)))

	frame := readDurableFrame(t, conn)
	assert.Equal(t, "interaction_error", frame["event"])
	assert.Equal(t, "InvalidParam", frame["Code"])
	assert.Equal(t, "turn-1", frame["TurnId"])
	assert.Equal(t, "confirmation/0", frame["InteractionKey"])

	// The socket and card remain usable. A corrected submission reaches the
	// same durable interaction instead of forcing a new turn.
	coordinator.mu.Lock()
	coordinator.resolveErr = nil
	coordinator.mu.Unlock()
	corrected := `{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","TurnId":"turn-1","InteractionKey":"confirmation/0","Confirmed":true,"Overrides":{"Zone":"cn-bj2-02"}}`
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(corrected)))
	require.Eventually(t, func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return len(coordinator.resolved) == 1
	}, time.Second, 10*time.Millisecond)
}

func TestWSDurable_BootGateKeepsClientOptInDisabled(t *testing.T) {
	coordinator := &fakeDurableCoordinator{}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"client-1","Message":"hi","Features":["confirm_form_v1"]}`,
	)))
	frame := readDurableFrame(t, conn)
	assert.Equal(t, "meta", frame["event"])
	coordinator.mu.Lock()
	require.Len(t, coordinator.submits, 1)
	assert.False(t, coordinator.submits[0].ConfirmForm)
	assert.False(t, coordinator.submits[0].GuidedCreate)
	coordinator.mu.Unlock()
}

func TestDurableSubmitInput_PropagatesFeatureFlags(t *testing.T) {
	h := newTestHandlers()
	h.SetConfirmFormEnabled(true)
	h.SetGuidedCreateEnabled(true)
	frame, err := simplejson.NewJson([]byte(`{"ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"feature-turn","Message":"create","Features":["confirm_form_v1","guided_create_v1","feishu_public_platform_readonly_v1","feishu_console_handoff_v1"]}`))
	require.NoError(t, err)
	in, apiErr := h.durableSubmitInput(context.Background(), BaseRequest{Owner: gatewayOwner, RequestUUID: "request-feature"}, frame)
	require.Nil(t, apiErr)
	assert.True(t, in.ConfirmForm)
	assert.True(t, in.GuidedCreate)
	assert.True(t, in.PublicPlatformReadOnly)
	assert.True(t, in.FeishuConsoleHandoff)
}

func TestGetMeta_DurableModeAdvertisesEnabledConfirmationFeatures(t *testing.T) {
	h := newTestHandlers()
	h.SetConfirmFormEnabled(true)
	h.SetGuidedCreateEnabled(true)
	h.SetTurnCoordinator(&fakeDurableCoordinator{})
	data, err := h.handleGetMeta(nil, BaseRequest{}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{featureTurnReplay, featureConfirmForm, featureGuidedCreate}, data.(metaData).Features)
}

func TestConfirmPOST_DurableModeUsesPersistentInteraction(t *testing.T) {
	coordinator := &fakeDurableCoordinator{turn: store.Turn{ID: "turn-1", SessionID: "sess-1", Owner: gatewayOwner}}
	h := newTestHandlers()
	h.SetTurnCoordinator(coordinator)
	rec := performGateway(h, `{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","TurnId":"turn-1","ConfirmationId":"confirmation/0","InteractionKey":"confirmation/0","Confirmed":true,"Overrides":{"Zone":"cn-bj2-02"},"top_organization_id":1,"organization_id":2}`)
	require.Equal(t, 200, rec.Code)
	coordinator.mu.Lock()
	require.Len(t, coordinator.resolved, 1)
	assert.Equal(t, "turn-1", coordinator.resolved[0].turnID)
	assert.Equal(t, "confirmation/0", coordinator.resolved[0].key)
	assert.Equal(t, map[string]string{"Zone": "cn-bj2-02"}, coordinator.resolved[0].value.Overrides)
	coordinator.mu.Unlock()
}

func TestWSDurable_WrongSessionCannotResolveAnotherTurnsInteraction(t *testing.T) {
	coordinator := &fakeDurableCoordinator{turn: store.Turn{ID: "turn-1", SessionID: "sess-1", Owner: gatewayOwner}}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"ConfirmCSAgentAction","SessionId":"sess-other","TurnId":"turn-1","InteractionKey":"confirmation/0","Confirmed":true}`,
	)))
	frame := readDurableFrame(t, conn)
	assert.Equal(t, "interaction_error", frame["event"])
	assert.Equal(t, "NotFound", frame["Code"])
	coordinator.mu.Lock()
	assert.Empty(t, coordinator.resolved)
	coordinator.mu.Unlock()
}

func TestConfirmPOST_DurableModeRejectsWrongSessionBinding(t *testing.T) {
	coordinator := &fakeDurableCoordinator{turn: store.Turn{ID: "turn-1", SessionID: "sess-1", Owner: gatewayOwner}}
	h := newTestHandlers()
	h.SetTurnCoordinator(coordinator)
	rec := performGateway(h, `{"Action":"ConfirmCSAgentAction","SessionId":"sess-other","TurnId":"turn-1","ConfirmationId":"confirmation/0","InteractionKey":"confirmation/0","Confirmed":true,"top_organization_id":1,"organization_id":2}`)
	assert.Equal(t, 404, rec.Code)
	coordinator.mu.Lock()
	assert.Empty(t, coordinator.resolved)
	coordinator.mu.Unlock()
}

func TestWSDurable_DisconnectNeverAbortsDurableExecution(t *testing.T) {
	coordinator := &fakeDurableCoordinator{}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"client-detach","Message":"hi"}`,
	)))
	_ = readDurableFrame(t, conn)
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "detach"))
	time.Sleep(50 * time.Millisecond)
	coordinator.mu.Lock()
	assert.Zero(t, coordinator.abortCalls)
	coordinator.mu.Unlock()
}

func TestWSDurable_CancelAbortsOnlyTheNamedDurableTurn(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		turn: store.Turn{
			ID: "turn-cancel", SessionID: "sess-1", ClientTurnID: "client-cancel",
			Owner: gatewayOwner, Status: store.TurnStatusAccepted, NextEventSeq: 2,
		},
		events: []turncoord.Event{{
			TurnID: "turn-cancel", Seq: 1, Type: "turn.failed", Provisional: false,
			Payload: json.RawMessage(`{"reason":"client_aborted","status":"aborted"}`),
		}},
	}
	srv := durableWSTestServer(t, coordinator)
	conn := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"CancelCSAgentTurn","ProtocolVersion":2,"SessionId":"sess-1","TurnId":"turn-cancel","ClientTurnId":"client-cancel"}`,
	)))
	assert.Equal(t, "meta", readDurableFrame(t, conn)["event"])
	terminal := readDurableFrame(t, conn)
	assert.Equal(t, "aborted", terminal["event"])
	assert.Equal(t, "aborted", terminal["Status"])
	coordinator.mu.Lock()
	assert.Equal(t, 1, coordinator.abortCalls)
	coordinator.mu.Unlock()
}

func TestWSDurable_SameImageBytesDifferentOCROutputKeepOneIdentity(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		imageIdentityGate: true,
		events: []turncoord.Event{{
			TurnID: "turn-durable-1", Seq: 1, Type: "turn.committed", Provisional: false,
			Payload: json.RawMessage(`{"content":"ok","committed":true}`),
		}},
	}
	ocrClient := &sequenceOCR{outputs: []string{"first OCR", "second OCR drift"}}
	srv := durableWSTestServerWithOCR(t, coordinator, ocrClient)
	image := "data:image/png;base64,aGVsbG8="
	for i := 0; i < 2; i++ {
		conn := dialWS(t, srv, gatewayHeaders())
		frame := `{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"image-turn","Message":"read this","Image":"` + image + `"}`
		require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(frame)))
		_ = readDurableFrame(t, conn)
		assert.Equal(t, "done", readDurableFrame(t, conn)["event"])
	}
	coordinator.mu.Lock()
	require.Len(t, coordinator.submits, 2)
	assert.NotEmpty(t, coordinator.submits[0].ImageDigest)
	assert.Equal(t, coordinator.submits[0].ImageDigest, coordinator.submits[1].ImageDigest)
	assert.NotEqual(t, coordinator.submits[0].ImageContext, coordinator.submits[1].ImageContext)
	coordinator.mu.Unlock()
}

func TestWSDurable_SameClientTurnIDDifferentImageBytesConflict(t *testing.T) {
	coordinator := &fakeDurableCoordinator{
		imageIdentityGate: true,
		events: []turncoord.Event{{
			TurnID: "turn-durable-1", Seq: 1, Type: "turn.committed", Provisional: false,
			Payload: json.RawMessage(`{"content":"ok","committed":true}`),
		}},
	}
	srv := durableWSTestServerWithOCR(t, coordinator, &sequenceOCR{outputs: []string{"one", "two"}})
	first := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, first.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"same-id","Message":"read","Image":"data:image/png;base64,b25l"}`,
	)))
	_ = readDurableFrame(t, first)
	assert.Equal(t, "done", readDurableFrame(t, first)["event"])

	second := dialWS(t, srv, gatewayHeaders())
	require.NoError(t, second.Write(context.Background(), websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"sess-1","ClientTurnId":"same-id","Message":"read","Image":"data:image/png;base64,dHdv"}`,
	)))
	errorFrame := readDurableFrame(t, second)
	assert.Equal(t, "error", errorFrame["event"])
	assert.Equal(t, "Conflict", errorFrame["Code"])
}

func TestGetSession_DurableModeNeverSilentlyCreatesReplacement(t *testing.T) {
	h := newTestHandlers()
	h.SetTurnCoordinator(&fakeDurableCoordinator{})
	rec := performGateway(h, `{"Action":"GetCSAgentSession","SessionId":"stale-session","top_organization_id":1,"organization_id":2}`)
	assert.Equal(t, 404, rec.Code)
	assert.Contains(t, rec.Body.String(), `"RetCode":226615`)
	assert.NotContains(t, rec.Body.String(), `"SessionId":"sess-new"`)
}

func TestGetSession_DurableModeReturnsOnlyCommittedPairs(t *testing.T) {
	now := time.Now()
	sessions := &mockSessions{byID: map[string]store.Session{
		"sess-1": {ID: "sess-1", TopOrganizationID: 1, OrganizationID: 2, MessageCount: 99, CreatedAt: now, UpdatedAt: now},
	}}
	messages := &committedPageMessages{
		committed: []store.Message{
			{ID: "user-ok", SessionID: "sess-1", Role: "user", Content: "saved question", Status: "ok", CreatedAt: now},
			{ID: "assistant-ok", SessionID: "sess-1", Role: "assistant", Content: "saved answer", Status: "ok", CreatedAt: now},
		},
		total: 2,
	}
	h := NewHandlers(&config.Config{}, sessions, messages, mockFeedback{}, nil, nil)
	h.SetTurnCoordinator(&fakeDurableCoordinator{})
	rec := performGateway(h, `{"Action":"GetCSAgentSession","SessionId":"sess-1","top_organization_id":1,"organization_id":2}`)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"MessageCount":2`)
	assert.Contains(t, rec.Body.String(), `"saved question"`)
	assert.Contains(t, rec.Body.String(), `"saved answer"`)
	assert.NotContains(t, rec.Body.String(), `"MessageCount":99`)
}
