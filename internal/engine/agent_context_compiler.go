package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
)

// These source labels remain readable in existing session context.
const (
	selectionSourceAccountSingle = "account_registry_single"
	selectionSourcePendingCard   = "pending_selection"
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

// safeToolConversationText redacts JSON values before encoding them. Applying a
// credential regexp to serialized JSON can consume an escape rather than the
// secret it precedes, both breaking the JSON and retaining the credential.
func safeToolConversationText(value string) string {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return safeConversationText(value)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return safeConversationText(value)
	}
	redacted := security.RedactForLLM(decoded)
	if reflect.DeepEqual(decoded, redacted) {
		return value
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return safeConversationText(value)
	}
	return string(encoded)
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
	case openai.ChatMessageRoleTool:
		return safeToolConversationText(value)
	}
	return safeConversationText(value)
}

func historyConversationText(role, value string) string {
	return canonicalConversationText(role, value)
}

// isLiveSelectionHint includes only recorded execution context and recognized
// source labels. These are referents for the Agent, not write authorization.
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
func renderAgentContextCard(view AgentContext) string {
	var lines []string
	lines = append(lines, "【本轮执行上下文（仅用于目标指代）】")
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
