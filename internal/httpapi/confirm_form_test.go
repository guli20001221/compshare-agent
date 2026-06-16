package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WHY these tests exist: Overrides are user-controlled input headed for
// CreateCompShareInstance. The broker is the transport-side whitelist gate —
// if it ever passes a value the emitted form didn't offer, a crafted frame
// could inject create params. And the no-Form wire shape must stay
// byte-identical so legacy clients are unaffected by the feature existing.

func testGPUForm() *workflow.ConfirmForm {
	return &workflow.ConfirmForm{Version: 1, Fields: []workflow.ConfirmFormField{
		{Key: "GpuType", Label: "GPU 型号", Type: "select", Value: "4090", Editable: true,
			Options: []workflow.ConfirmFormOption{{Value: "4090"}, {Value: "A800"}}},
	}}
}

func TestConfirmBroker_FormOverridesDelivered(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.RegisterWithForm("sess-1", testOwner, testGPUForm())

	require.NoError(t, b.Resolve(id, "sess-1", testOwner,
		ConfirmDecision{Confirmed: true, Overrides: map[string]string{"GpuType": "A800"}}))

	d := WaitForConfirmation(context.Background(), ch, time.Second)
	assert.True(t, d.Confirmed)
	assert.Equal(t, map[string]string{"GpuType": "A800"}, d.Overrides)
}

func TestConfirmBroker_InvalidOverrideKeepsPending(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.RegisterWithForm("sess-1", testOwner, testGPUForm())

	// Value never offered → rejected AND the pending survives so the client
	// can fix and resend within the timeout window.
	err := b.Resolve(id, "sess-1", testOwner,
		ConfirmDecision{Confirmed: true, Overrides: map[string]string{"GpuType": "H100"}})
	require.Error(t, err)

	require.NoError(t, b.Resolve(id, "sess-1", testOwner,
		ConfirmDecision{Confirmed: true, Overrides: map[string]string{"GpuType": "A800"}}))
	d := WaitForConfirmation(context.Background(), ch, time.Second)
	assert.True(t, d.Confirmed)
	assert.Equal(t, "A800", d.Overrides["GpuType"])
}

func TestConfirmBroker_OverridesWithoutFormRejected(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.Register("sess-1", testOwner) // legacy: no form emitted

	err := b.Resolve(id, "sess-1", testOwner,
		ConfirmDecision{Confirmed: true, Overrides: map[string]string{"GpuType": "A800"}})
	assert.ErrorIs(t, err, ErrOverridesNotAllowed)

	// Pending kept; a plain confirm still works.
	require.NoError(t, b.Resolve(id, "sess-1", testOwner, ConfirmDecision{Confirmed: true}))
	assert.True(t, WaitForConfirmation(context.Background(), ch, time.Second).Confirmed)
}

func TestConfirmBroker_DenyIgnoresOverrides(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.RegisterWithForm("sess-1", testOwner, testGPUForm())

	// A denial resolves regardless of override validity — and must not leak
	// the overrides downstream (nothing may act on a denied edit).
	require.NoError(t, b.Resolve(id, "sess-1", testOwner,
		ConfirmDecision{Confirmed: false, Overrides: map[string]string{"GpuType": "H100"}}))
	d := WaitForConfirmation(context.Background(), ch, time.Second)
	assert.False(t, d.Confirmed)
	assert.Nil(t, d.Overrides)
}

func TestConfirmEditsFuncFor_DoubleGate(t *testing.T) {
	sw := &recordingSink{}
	prepIn := &chatPrep{confirmFormOptIn: true}
	prepOut := &chatPrep{confirmFormOptIn: false}

	flagOff := &Handlers{confirmBroker: NewConfirmBroker(), confirmFormEnabled: false}
	flagOn := &Handlers{confirmBroker: NewConfirmBroker(), confirmFormEnabled: true}

	assert.Nil(t, flagOff.confirmEditsFuncFor(context.Background(), sw, "s", testOwner, prepIn),
		"boot flag off must disable the form gate even for opted-in clients")
	assert.Nil(t, flagOn.confirmEditsFuncFor(context.Background(), sw, "s", testOwner, prepOut),
		"a client that did not opt in must never receive a Form")
	assert.NotNil(t, flagOn.confirmEditsFuncFor(context.Background(), sw, "s", testOwner, prepIn))
}

// TestConfirmationEvent_LegacyWireShapeUnchanged pins the no-Form frame bytes:
// adding the feature must not add a key to frames legacy clients receive.
func TestConfirmationEvent_LegacyWireShapeUnchanged(t *testing.T) {
	raw, err := json.Marshal(confirmationEvent{
		ConfirmationID: "c-1",
		Action:         "CreateInstanceWorkflow",
		Summary:        map[string]any{"GpuType": "4090"},
		TimeoutSeconds: 60,
	})
	require.NoError(t, err)
	assert.Equal(t,
		`{"ConfirmationId":"c-1","Action":"CreateInstanceWorkflow","Summary":{"GpuType":"4090"},"TimeoutSeconds":60}`,
		string(raw))

	withForm, err := json.Marshal(confirmationEvent{
		ConfirmationID: "c-2", Action: "CreateInstanceWorkflow", TimeoutSeconds: 120,
		Form: testGPUForm(),
	})
	require.NoError(t, err)
	assert.Contains(t, string(withForm), `"Form":{"Version":1`)
}

func TestGetMeta_FeaturesAdvertisedOnlyWhenEnabled(t *testing.T) {
	srvOff, _, hOff := wsTestHandlers(t, chatLLM{}, denyConfirm)
	_ = srvOff
	dataOff, err := hOff.handleGetMeta(nil, BaseRequest{}, nil)
	require.NoError(t, err)
	assert.Empty(t, dataOff.(metaData).Features, "flag off → no Features advertised (legacy GetMeta shape)")

	hOff.SetConfirmFormEnabled(true)
	dataOn, err := hOff.handleGetMeta(nil, BaseRequest{}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{featureConfirmForm}, dataOn.(metaData).Features)

	hOff.SetGuidedCreateEnabled(true)
	dataGuided, err := hOff.handleGetMeta(nil, BaseRequest{}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{featureConfirmForm, featureGuidedCreate}, dataGuided.(metaData).Features)
}

func TestStringMapFromFrame(t *testing.T) {
	got, err := stringMapFromFrame(map[string]any{"GpuType": "A800"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"GpuType": "A800"}, got)

	got, err = stringMapFromFrame(nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	// A non-string override must reject the frame — silently dropping it would
	// turn "confirm with edits" into "confirm the unedited card".
	_, err = stringMapFromFrame(map[string]any{"GpuType": float64(1)})
	assert.Error(t, err)
}

func TestOverridesFromFrame(t *testing.T) {
	frame, err := simplejson.NewJson([]byte(`{"Action":"ConfirmCSAgentAction"}`))
	require.NoError(t, err)
	got, err := overridesFromFrame(frame)
	require.NoError(t, err)
	assert.Nil(t, got)

	frame, err = simplejson.NewJson([]byte(`{"Overrides":{"GpuType":"A800"}}`))
	require.NoError(t, err)
	got, err = overridesFromFrame(frame)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"GpuType": "A800"}, got)

	frame, err = simplejson.NewJson([]byte(`{"Overrides":[]}`))
	require.NoError(t, err)
	_, err = overridesFromFrame(frame)
	assert.Error(t, err, "present but non-object Overrides must not silently confirm the unedited card")
}

// ---------------------------------------------------------------------------
// WS round trips
// ---------------------------------------------------------------------------

func TestWS_Confirm_OverridesRoundTrip(t *testing.T) {
	srv, _, h := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	confirmID, ch := h.confirmBroker.RegisterWithForm("sess-1", gatewayOwner, testGPUForm())
	result := make(chan ConfirmDecision, 1)
	go func() {
		result <- WaitForConfirmation(context.Background(), ch, 5*time.Second)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmID+`","Confirmed":true,"Overrides":{"GpuType":"A800"}}`)))

	select {
	case d := <-result:
		assert.True(t, d.Confirmed)
		assert.Equal(t, "A800", d.Overrides["GpuType"], "validated overrides must reach the workflow gate")
	case <-time.After(3 * time.Second):
		t.Fatal("override confirm frame did not resolve the waiter")
	}
}

func TestWS_Confirm_InvalidOverride_ErrorFrameAndPendingKept(t *testing.T) {
	srv, _, h := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	confirmID, ch := h.confirmBroker.RegisterWithForm("sess-1", gatewayOwner, testGPUForm())
	result := make(chan ConfirmDecision, 1)
	go func() {
		result <- WaitForConfirmation(context.Background(), ch, 5*time.Second)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// H100 was never offered → error frame back, pending NOT consumed.
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmID+`","Confirmed":true,"Overrides":{"GpuType":"H100"}}`)))

	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	var f map[string]any
	require.NoError(t, json.Unmarshal(data, &f))
	assert.Equal(t, "error", f["event"])
	assert.Equal(t, "InvalidParam", f["Code"])

	// The client fixes the value and resends — the SAME pending resolves.
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmID+`","Confirmed":true,"Overrides":{"GpuType":"A800"}}`)))

	select {
	case d := <-result:
		assert.True(t, d.Confirmed)
		assert.Equal(t, "A800", d.Overrides["GpuType"])
	case <-time.After(3 * time.Second):
		t.Fatal("corrected override frame did not resolve the waiter")
	}
}
