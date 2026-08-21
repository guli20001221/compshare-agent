package engine

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
)

func cloneAgentContext(in AgentContext) AgentContext {
	out := in
	out.RecentConversation = append([]ConversationPair(nil), in.RecentConversation...)
	out.SelectedEntities = cloneEntityHints(in.SelectedEntities)
	return out
}

func cloneEntityHints(in []SelectedEntityHint) []SelectedEntityHint {
	out := make([]SelectedEntityHint, 0, len(in))
	for _, hint := range in {
		hint.Kind = safeContextText(hint.Kind)
		hint.ID = safeContextText(hint.ID)
		hint.Name = safeContextText(hint.Name)
		hint.Source = safeContextText(hint.Source)
		hint.Freshness = safeContextText(hint.Freshness)
		out = append(out, hint)
	}
	return out
}

func safeContextText(value string) string {
	return compactContextText(security.RedactOperationalTokensInText(value))
}

// safeConversationText redacts replayed content without altering whitespace or
// truncating a message. History size is bounded by whole exchanges elsewhere.
func safeConversationText(value string) string {
	return security.RedactOperationalTokensInText(value)
}

// canonicalConversationText is the persistence-aligned form of a conversation
// endpoint. HTTP persists user and
// assistant rows through different redaction boundaries; using those same
// boundaries before the hot transcript is captured keeps hot and cold endpoints
// byte-identical without any fuzzy transcript matching.
//
// This applies only to historical conversation. The current user turn remains
// raw for routing and tool selection, as required by the input boundary.
func canonicalConversationText(role, value string) string {
	switch role {
	case openai.ChatMessageRoleUser:
		return security.RedactUserConversationText(value)
	case openai.ChatMessageRoleAssistant:
		return security.RedactAssistantConversationText(value)
	}
	return safeConversationText(value)
}

func historyConversationText(role, value string) string {
	return canonicalConversationText(role, value)
}

// isLiveSelectionHint reports whether one SelectedEntities row is live selection
// state rather than an arbitrary model-inferred entity.
//
// CompileForTurn assembles SelectedEntities from the account's sole instance,
// the pending selection card's numbered candidates, and the current
// SelectedInstanceID with its provenance (user_selected or observed). They are
// execution state, not a semantic summary of the conversation. A selected
// instance with empty provenance remains visible for understanding but is
// not a write-selection proof (selection_binder has its own stricter allowlist).
//
// Unknown sources are excluded by default.
func isLiveSelectionHint(hint SelectedEntityHint) bool {
	switch hint.Source {
	case selectionSourceAccountSingle, selectionSourcePendingCard,
		SelectedInstanceSourceUser, SelectedInstanceSourceObserved, "":
		return true
	}
	return false
}

// renderAgentContextCard serializes only the context a transcript cannot carry:
// live execution state (current selection and pending selection card). Complete
// prior exchanges live only in the canonical transcript.
//
// Selection filtering remains at this model-facing serialization point rather
// than CompileForTurn. selection_binder and the write-proposal path read
// view.SelectedEntities as a struct; filtering upstream would silently narrow
// execution-side target binding instead of only changing what the model reads.
func renderAgentContextCard(view AgentContext) string {
	var lines []string
	lines = append(lines, "【本轮执行上下文；不授权任何写操作】")
	for _, entity := range view.SelectedEntities {
		if !isLiveSelectionHint(entity) {
			continue
		}
		label := strings.TrimSpace(entity.Name + " " + entity.ID)
		if label != "" {
			ordinal := ""
			if entity.Ordinal > 0 {
				ordinal = fmt.Sprintf("，序号=%d", entity.Ordinal)
			}
			lines = append(lines, fmt.Sprintf("相关对象：%s（类型=%s，来源=%s，新鲜度=%s%s）", safeContextText(label), safeContextText(entity.Kind), safeContextText(entity.Source), safeContextText(entity.Freshness), ordinal))
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}
