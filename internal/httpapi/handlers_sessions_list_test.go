package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newListTestHandlers builds a Handlers backed by the supplied mockSessions.
// cfg is unused by the list path, so a zero config suffices.
func newListTestHandlers(ms *mockSessions) *Handlers {
	return NewHandlers(
		&config.Config{},
		ms,
		&mockMessages{},
		mockFeedback{},
		nil,
		nil,
	)
}

// TestDispatchListSessions_OwnerScopedAndOrdered asserts the two business
// contracts of the history sidebar: a user only ever sees their OWN sessions
// (no cross-tenant leak), and they come back most-recently-active first.
func TestDispatchListSessions_OwnerScopedAndOrdered(t *testing.T) {
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	older := "older"
	newer := "newer"
	ms := &mockSessions{byID: map[string]store.Session{
		"s-old":   {ID: "s-old", TopOrganizationID: 1, OrganizationID: 2, Title: &older, UpdatedAt: t0},
		"s-new":   {ID: "s-new", TopOrganizationID: 1, OrganizationID: 2, Title: &newer, UpdatedAt: t0.Add(time.Hour)},
		"s-other": {ID: "s-other", TopOrganizationID: 9, OrganizationID: 9, UpdatedAt: t0.Add(2 * time.Hour)},
	}}
	h := newListTestHandlers(ms)

	rec := performGateway(h, `{"Action":"ListCSAgentSessions","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"RetCode":0`)
	// Cross-tenant isolation: the other owner's session must never appear,
	// even though it is the most-recently-active row overall.
	assert.NotContains(t, body, "s-other")
	assert.Contains(t, body, "s-old")
	assert.Contains(t, body, "s-new")
	// Most-recently-active first: s-new (updated_at later) precedes s-old.
	assert.Less(t, strings.Index(body, "s-new"), strings.Index(body, "s-old"))
}

func TestDispatchListSessionsRedactsHistoricalAuthorizationTitle(t *testing.T) {
	const secret = "historical-list-title-secret-0123456789"
	title := "Authorization: Bearer " + secret
	ms := &mockSessions{byID: map[string]store.Session{
		"s-secret": {
			ID: "s-secret", TopOrganizationID: 1, OrganizationID: 2,
			Title: &title, UpdatedAt: time.Now(),
		},
	}}
	h := newListTestHandlers(ms)
	rec := performGateway(h, `{"Action":"ListCSAgentSessions","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), secret)
	assert.Contains(t, rec.Body.String(), "Authorization")
}

// TestListSessionsLimitClamp asserts the handler clamps Limit to [1, max] with a
// default, rather than rejecting out-of-range values like GetSession does — the
// sidebar is a UI auto-call that should not fail on a stray Limit.
func TestListSessionsLimitClamp(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"absent->default", `{"Action":"ListCSAgentSessions","top_organization_id":1,"organization_id":2}`, defaultSessionListLimit},
		{"zero->default", `{"Action":"ListCSAgentSessions","Limit":0,"top_organization_id":1,"organization_id":2}`, defaultSessionListLimit},
		{"negative->default", `{"Action":"ListCSAgentSessions","Limit":-5,"top_organization_id":1,"organization_id":2}`, defaultSessionListLimit},
		{"over-max->clamped", `{"Action":"ListCSAgentSessions","Limit":999,"top_organization_id":1,"organization_id":2}`, maxSessionListLimit},
		{"in-range->kept", `{"Action":"ListCSAgentSessions","Limit":3,"top_organization_id":1,"organization_id":2}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := &mockSessions{byID: map[string]store.Session{}}
			h := newListTestHandlers(ms)
			rec := performGateway(h, tc.body)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.want, ms.lastListLimit)
		})
	}
}

// TestListSessionsEmptyState asserts an owner with no sessions serializes as
// "Sessions":[] (not null), which is the contract the frontend relies on to
// render the "暂无历史对话" empty state instead of choking on null.
func TestListSessionsEmptyState(t *testing.T) {
	ms := &mockSessions{byID: map[string]store.Session{}}
	h := newListTestHandlers(ms)

	rec := performGateway(h, `{"Action":"ListCSAgentSessions","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"Sessions":[]`)
	assert.Contains(t, body, `"Action":"ListCSAgentSessions"`)
	assert.Contains(t, body, `"RetCode":0`)
}

// TestDeriveSessionTitle covers the title derivation used by the chat write path
// to label history rows: empty input is skipped, CJK survives intact, whitespace
// collapses, long input truncates by rune, and credentials/PII are redacted so
// the sidebar never shows values the message body itself would have redacted.
func TestDeriveSessionTitle(t *testing.T) {
	t.Run("empty and whitespace yield empty", func(t *testing.T) {
		assert.Equal(t, "", deriveSessionTitle(""))
		assert.Equal(t, "", deriveSessionTitle("   \n\t "))
	})
	t.Run("short CJK preserved verbatim", func(t *testing.T) {
		assert.Equal(t, "4090现在有连存吗?", deriveSessionTitle("4090现在有连存吗?"))
	})
	t.Run("internal whitespace collapses to single spaces", func(t *testing.T) {
		assert.Equal(t, "创建 实例 步骤", deriveSessionTitle("  创建\n实例   步骤  "))
	})
	t.Run("truncates to 30 runes plus ellipsis", func(t *testing.T) {
		got := deriveSessionTitle(strings.Repeat("好", 40))
		assert.Equal(t, strings.Repeat("好", sessionTitleMaxRunes)+"…", got)
		assert.Equal(t, sessionTitleMaxRunes+1, len([]rune(got)))
	})
	t.Run("PII redacted consistent with stored message body", func(t *testing.T) {
		got := deriveSessionTitle("我的手机13800138000")
		assert.NotContains(t, got, "13800138000")
		assert.Contains(t, got, guardrails.PhoneRedacted)
	})
	t.Run("authorization header value never reaches the sidebar", func(t *testing.T) {
		const secret = "title-auth-secret-0123456789"
		got := deriveSessionTitle("Authorization: " + secret)
		assert.NotContains(t, got, secret)
		assert.Contains(t, got, "Authorization")
		assert.Contains(t, got, "[",
			"the rune cap may truncate the marker, but must leave visible evidence of redaction: %q", got)
	})
}

// TestPrepareChat_TitleDerivationGate locks in the chat write-path contract that
// deriveSessionTitle-in-isolation cannot: the prepareChat gate must derive a
// title from the first user message ONLY when the session has none yet, must
// skip a session that already has a title (the perf guard the inline comment
// promises), and must skip when the message derives to empty. Without this the
// gate could be deleted, inverted, or fed persistContent (OCR-prefixed) and CI
// would stay green — the setTitleCallCount spy exists precisely for this.
func TestPrepareChat_TitleDerivationGate(t *testing.T) {
	t.Run("untitled session derives title once", func(t *testing.T) {
		h, sessions, _ := newChatTestHandlers(t, store.Session{
			ID:                "sess-untitled",
			TopOrganizationID: 1,
			OrganizationID:    2,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		})

		sink, _ := dispatchChatTurn(t, h, "sess-untitled", "hi")

		assert.True(t, sink.has("done"))
		assert.Equal(t, 1, sessions.setTitleCallCount, "first turn on a title-less session must derive a title")
		row := sessions.byID["sess-untitled"]
		require.NotNil(t, row.Title, "derived title must be persisted")
		assert.Equal(t, "hi", *row.Title, "title must come from the user message")
	})

	t.Run("already-titled session is left untouched", func(t *testing.T) {
		existing := "client named this"
		h, sessions, _ := newChatTestHandlers(t, store.Session{
			ID:                "sess-titled",
			TopOrganizationID: 1,
			OrganizationID:    2,
			Title:             &existing,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		})

		sink, _ := dispatchChatTurn(t, h, "sess-titled", "hi")

		assert.True(t, sink.has("done"))
		assert.Equal(t, 0, sessions.setTitleCallCount,
			"a session that already has a title must not trigger SetTitleIfEmpty (avoids a guaranteed 0-row UPDATE per turn)")
		assert.Equal(t, "client named this", *sessions.byID["sess-titled"].Title)
	})

	t.Run("whitespace-only first message writes no title", func(t *testing.T) {
		// A whitespace-only message derives to "" so the gate must skip; whether
		// the stream later completes or the input is rejected upstream is
		// orthogonal — the contract is that no junk title row is created.
		h, sessions, _ := newChatTestHandlers(t, store.Session{
			ID:                "sess-ws",
			TopOrganizationID: 1,
			OrganizationID:    2,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		})

		_, _ = dispatchChatTurn(t, h, "sess-ws", "   ")

		assert.Equal(t, 0, sessions.setTitleCallCount,
			"a message that derives to empty must not write a title")
		assert.Nil(t, sessions.byID["sess-ws"].Title)
	})
}
