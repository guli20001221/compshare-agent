package httpapi

import (
	"strings"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/compshare-agent/internal/security"
	"github.com/gin-gonic/gin"
)

// defaultSessionListLimit / maxSessionListLimit bound the history-sidebar page
// size. The sidebar shows ~10 recent conversations; callers may ask for fewer or
// more but are clamped to [1, max] rather than rejected — unlike GetSession's
// strict 1–100 validation — because the sidebar is a UI auto-call that should
// not fail on a stray Limit.
const (
	defaultSessionListLimit = 10
	maxSessionListLimit     = 50
)

// sessionTitleMaxRunes caps an auto-derived title length. Counted in runes so a
// CJK title like "4090现在有连存吗?" survives intact while long prompts are cut.
const sessionTitleMaxRunes = 30

// sessionListItem is one history-sidebar row.
type sessionListItem struct {
	SessionID    string    `json:"SessionId"`
	Title        *string   `json:"Title"`
	MessageCount int       `json:"MessageCount"`
	Pinned       bool      `json:"Pinned"`
	CreatedAt    time.Time `json:"CreatedAt"`
	UpdatedAt    time.Time `json:"UpdatedAt"`
}

// listSessionsData is the Data payload for a successful ListCSAgentSessions
// response: the owner's most-recently-active sessions first.
type listSessionsData struct {
	Sessions []sessionListItem `json:"Sessions"`
}

// handleListSessions returns the owner's recent sessions for the history
// sidebar. Owner-scoped via base.Owner; soft-deleted rows are excluded by the
// store. Optional: Limit (1–50, default 10). No SessionId required. An empty
// result serializes as "Sessions":[] (not null) to drive the empty state.
func (h *Handlers) handleListSessions(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
	limit := raw.Get("Limit").MustInt(defaultSessionListLimit)
	if limit < 1 {
		limit = defaultSessionListLimit
	}
	if limit > maxSessionListLimit {
		limit = maxSessionListLimit
	}

	sessions, err := h.sessions.ListByOwner(c.Request.Context(), base.Owner, limit)
	if err != nil {
		return nil, err
	}

	items := make([]sessionListItem, 0, len(sessions))
	for _, sess := range sessions {
		items = append(items, sessionListItem{
			SessionID:    sess.ID,
			Title:        redactSessionTitle(sess.Title),
			MessageCount: sess.MessageCount,
			Pinned:       sess.Pinned,
			CreatedAt:    sess.CreatedAt,
			UpdatedAt:    sess.UpdatedAt,
		})
	}
	return listSessionsData{Sessions: items}, nil
}

// redactSessionTitle applies the user-conversation persistence boundary both
// before a caller-supplied title is stored and when any title is projected.
// The read-side application also covers rows created by older binaries.
func redactSessionTitle(title *string) *string {
	if title == nil {
		return nil
	}
	redacted := security.RedactUserConversationText(*title)
	return &redacted
}

// deriveSessionTitle builds a history-row title from the user's first message:
// credential-redacted by the exact user-message persistence boundary, whitespace
// collapsed to single spaces, and truncated to sessionTitleMaxRunes runes with
// an ellipsis. Returns "" for empty/whitespace input, in which case the caller
// skips the title write. It is called from the chat write path (prepareChat) on
// every turn but only fills the title when the row's title is still NULL.
func deriveSessionTitle(msg string) string {
	t := strings.TrimSpace(security.RedactUserConversationText(msg))
	if t == "" {
		return ""
	}
	t = strings.Join(strings.Fields(t), " ")
	r := []rune(t)
	if len(r) > sessionTitleMaxRunes {
		t = string(r[:sessionTitleMaxRunes]) + "…"
	}
	return t
}
