package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
)

// These source labels remain readable in existing session context.
const (
	selectionSourceAccountSingle = "account_registry_single"
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
		hint.Kind = compactContextText(hint.Kind)
		hint.ID = compactContextText(hint.ID)
		hint.Name = compactContextText(hint.Name)
		hint.Source = compactContextText(hint.Source)
		hint.Freshness = compactContextText(hint.Freshness)
		out = append(out, hint)
	}
	return out
}

// safeToolConversationText applies the field-name redaction to a JSON
// observation before it is replayed. Anything that is not one JSON document is
// replayed unchanged.
func safeToolConversationText(value string) string {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return value
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return value
	}
	redacted := security.RedactForLLM(decoded)
	if reflect.DeepEqual(decoded, redacted) {
		return value
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return value
	}
	return string(encoded)
}

// canonicalConversationText is the persistence-aligned form of a conversation
// endpoint. HTTP persists assistant rows through the same boundary; using it
// before the hot transcript is captured keeps hot and cold endpoints
// byte-identical without any fuzzy transcript matching. User text is persisted
// and replayed exactly as typed.
func canonicalConversationText(role, value string) string {
	switch role {
	case openai.ChatMessageRoleAssistant:
		return security.PersistedAssistantText(value)
	case openai.ChatMessageRoleTool:
		return safeToolConversationText(value)
	}
	return value
}

func historyConversationText(role, value string) string {
	return canonicalConversationText(role, value)
}

// isLiveSelectionHint includes only recorded execution context and recognized
// source labels. These are referents for the Agent, not write authorization.
func isLiveSelectionHint(hint SelectedEntityHint) bool {
	switch hint.Source {
	case selectionSourceAccountSingle,
		SelectedInstanceSourceUser, SelectedInstanceSourceObserved, "":
		return true
	}
	return false
}

// renderAgentContextCard serializes only the context a transcript cannot carry:
// live execution state (turn time and the current instance referent). Complete
// prior exchanges live only in the canonical transcript.
func renderAgentContextCard(view AgentContext) string {
	var lines []string
	lines = append(lines, "【本轮执行上下文】")
	if view.BuiltAtUnix > 0 {
		local := time.Unix(view.BuiltAtUnix, 0).In(time.FixedZone("Asia/Shanghai", 8*60*60))
		lines = append(lines, "本轮开始时间："+local.Format("2006-01-02 15:04:05 -07:00")+"（Asia/Shanghai）")
	}
	for _, entity := range view.SelectedEntities {
		if !isLiveSelectionHint(entity) {
			continue
		}
		label := strings.TrimSpace(entity.Name + " " + entity.ID)
		if label != "" {
			lines = append(lines, fmt.Sprintf("相关对象：%s（类型=%s，来源=%s，新鲜度=%s）", compactContextText(label), compactContextText(entity.Kind), compactContextText(entity.Source), compactContextText(entity.Freshness)))
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}
