package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testOwner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}

func TestConfirmBroker_ResolveConfirmed(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.Register("sess-1", testOwner)
	require.NotEmpty(t, id)

	go func() {
		time.Sleep(10 * time.Millisecond)
		require.NoError(t, b.Resolve(id, "sess-1", testOwner, ConfirmDecision{Confirmed: true}))
	}()

	result := WaitForConfirmation(context.Background(), ch, 1*time.Second)
	assert.True(t, result.Confirmed)
}

func TestConfirmBroker_ResolveDenied(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.Register("sess-1", testOwner)

	go func() {
		time.Sleep(10 * time.Millisecond)
		require.NoError(t, b.Resolve(id, "sess-1", testOwner, ConfirmDecision{Confirmed: false}))
	}()

	result := WaitForConfirmation(context.Background(), ch, 1*time.Second)
	assert.False(t, result.Confirmed)
}

func TestConfirmBroker_Timeout(t *testing.T) {
	b := NewConfirmBroker()
	_, ch := b.Register("sess-1", testOwner)

	result := WaitForConfirmation(context.Background(), ch, 50*time.Millisecond)
	assert.False(t, result.Confirmed, "timeout should return false")
}

func TestConfirmBroker_ContextCancelled(t *testing.T) {
	b := NewConfirmBroker()
	_, ch := b.Register("sess-1", testOwner)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	result := WaitForConfirmation(ctx, ch, 5*time.Second)
	assert.False(t, result.Confirmed, "cancelled context should return false")
}

func TestConfirmBroker_Cancel(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.Register("sess-1", testOwner)

	b.Cancel(id)

	result := WaitForConfirmation(context.Background(), ch, 50*time.Millisecond)
	assert.False(t, result.Confirmed, "cancelled confirmation should return false")
}

func TestWaitForConfirmationOutcomeDistinguishesTerminalReasons(t *testing.T) {
	confirmed := make(chan ConfirmDecision, 1)
	confirmed <- ConfirmDecision{Confirmed: true}
	close(confirmed)
	decision, reason := WaitForConfirmationOutcome(context.Background(), confirmed, time.Second)
	assert.True(t, decision.Confirmed)
	assert.Equal(t, observability.ConfirmationReasonUserConfirmed, reason)

	denied := make(chan ConfirmDecision, 1)
	denied <- ConfirmDecision{}
	close(denied)
	decision, reason = WaitForConfirmationOutcome(context.Background(), denied, time.Second)
	assert.False(t, decision.Confirmed)
	assert.Equal(t, observability.ConfirmationReasonUserDeclined, reason)

	timeoutCh := make(chan ConfirmDecision)
	_, reason = WaitForConfirmationOutcome(context.Background(), timeoutCh, time.Millisecond)
	assert.Equal(t, observability.ConfirmationReasonTimeout, reason)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, reason = WaitForConfirmationOutcome(ctx, make(chan ConfirmDecision), time.Second)
	assert.Equal(t, observability.ConfirmationReasonClientDisconnect, reason)

	closed := make(chan ConfirmDecision)
	close(closed)
	_, reason = WaitForConfirmationOutcome(context.Background(), closed, time.Second)
	assert.Equal(t, observability.ConfirmationReasonBrokerCancelled, reason)
}

func TestConfirmBroker_DoubleResolve(t *testing.T) {
	b := NewConfirmBroker()
	id, _ := b.Register("sess-1", testOwner)

	require.NoError(t, b.Resolve(id, "sess-1", testOwner, ConfirmDecision{Confirmed: true}))
	err := b.Resolve(id, "sess-1", testOwner, ConfirmDecision{Confirmed: true})
	assert.Error(t, err, "second resolve should fail")
}

// A confirm whose session label differs from the registration's (same owner)
// RESOLVES — the confirmation is bound to its ConfirmationId + owner, not the
// session label. This is required because stale-session recovery can mint a new
// session id mid-turn (the create-flow "[Forbidden] ... session/owner" bug);
// the random ConfirmationId + owner already prevent cross-tenant/guessing.
func TestConfirmBroker_ResolveSessionDriftSameOwnerResolves(t *testing.T) {
	b := NewConfirmBroker()
	id, ch := b.Register("sess-1", testOwner)

	require.NoError(t, b.Resolve(id, "sess-recovered", testOwner, ConfirmDecision{Confirmed: true}),
		"same-owner confirm under a drifted session label must resolve")
	result := WaitForConfirmation(context.Background(), ch, 50*time.Millisecond)
	assert.True(t, result.Confirmed, "decision must be delivered to the registering turn")
}

func TestConfirmBroker_ResolveWrongOwner(t *testing.T) {
	b := NewConfirmBroker()
	id, _ := b.Register("sess-1", testOwner)

	otherOwner := store.Owner{TopOrganizationID: 99, OrganizationID: 99}
	err := b.Resolve(id, "sess-1", otherOwner, ConfirmDecision{Confirmed: true})
	assert.ErrorIs(t, err, ErrConfirmationOwner, "wrong owner should be rejected")
}

func TestConfirmBroker_ResolveUnknownID(t *testing.T) {
	b := NewConfirmBroker()
	err := b.Resolve("nonexistent", "sess-1", testOwner, ConfirmDecision{Confirmed: true})
	assert.Error(t, err)
}

func TestConfirmBroker_CancelUnknownID(t *testing.T) {
	b := NewConfirmBroker()
	b.Cancel("nonexistent") // should not panic
}

func TestSanitizeConfirmArgs_FiltersPasswords(t *testing.T) {
	args := map[string]any{
		"UHostId":  "uhost-xxx",
		"Name":     "my-gpu",
		"Password": "secret123",
		"token":    "bearer-xyz",
		"State":    "Running",
	}
	safe := sanitizeConfirmArgs(args)
	assert.Equal(t, "uhost-xxx", safe["UHostId"])
	assert.Equal(t, "my-gpu", safe["Name"])
	assert.Equal(t, "Running", safe["State"])
	assert.NotContains(t, safe, "Password")
	assert.NotContains(t, safe, "token")
}

func TestSanitizeConfirmArgs_NilInput(t *testing.T) {
	assert.Nil(t, sanitizeConfirmArgs(nil))
}

// TestSanitizeConfirmArgs_KeepsDeployCardFields proves the deploy_model v2 confirm
// card reaches the frontend: the structured create-instance fields (GPU/image/zone/
// FallbackNote) survive sanitization into the SSE confirmation event's Summary —
// they are not secrets and must not be filtered.
func TestSanitizeConfirmArgs_KeepsDeployCardFields(t *testing.T) {
	args := map[string]any{
		"workflow":     "CreateInstanceWorkflow",
		"GpuType":      "4090",
		"Zone":         "cn-sh2-02",
		"image":        "ComfyUI",
		"FallbackNote": "cn-wlcb-01 暂时售罄，已自动切换到 cn-sh2-02 创建。",
		"Password":     "secret123",
	}
	safe := sanitizeConfirmArgs(args)
	assert.Equal(t, "4090", safe["GpuType"])
	assert.Equal(t, "cn-sh2-02", safe["Zone"])
	assert.Equal(t, "ComfyUI", safe["image"])
	assert.Contains(t, safe["FallbackNote"], "cn-sh2-02", "zone fallback note must reach the frontend card")
	assert.NotContains(t, safe, "Password", "secrets still filtered")
}
