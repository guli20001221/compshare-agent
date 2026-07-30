package engine

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// The replayed conversation window is the model's ENTIRE cross-turn memory —
// messagesFromAgentContext drops every other prior-turn message. Until this file
// existed, both sides of every restored pair went through safeContextText, which
// collapses whitespace and cuts at maxSemanticRunes (320). Nothing asserted that:
// `grep -rn 'maxSemanticRunes' internal/engine/*_test.go` was empty, so the
// truncation could be added, removed or retargeted with the suite still green.
//
// Measured cost of the truncation on the 2026-07 production exports (3867
// completed exchanges): 41.8% of assistant replies exceeded 320 runes, so 43% of
// exchanges reached the next turn incomplete.

// longAssistantReply builds a reply whose distinctive tail sits past rune 320 and
// which carries real newlines, i.e. the shape safeContextText destroyed twice over.
// The prefix is a plausible instance table rather than filler so the length is
// representative: production's median assistant reply is 255 runes and p90 is 782.
func longAssistantReply() string {
	var b strings.Builder
	b.WriteString("你共有 3 台实例：\n\n| 实例名称 | 实例ID | 状态 | GPU |\n")
	for i := 0; i < 12; i++ {
		b.WriteString("| host-demo-" + strconv.Itoa(i) + " | uhost-demo" + strconv.Itoa(i) + " | Running | 4090 |\n")
	}
	b.WriteString("\n启动时间：2026-07-19 11:35\n")
	return b.String()
}

func TestReplayedExchangeKeepsALongAssistantReplyIntact(t *testing.T) {
	reply := longAssistantReply()
	// Guard against a vacuous test: if the fixture ever shrinks below the old cut
	// point, the assertions below would pass on the truncating code too.
	require.Greater(t, len([]rune(reply)), maxSemanticRunes,
		"fixture must exceed the old 320-rune cut or this test cannot fail")

	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "好的"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "我有哪些实例"},
		{Role: openai.ChatMessageRoleAssistant, Content: reply},
	}

	_, err := eng.Chat(context.Background(), "第一台是什么时候启动的", noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 1)
	modelInput := renderTestMessages(mock.calls[0].Messages)

	// The tail is what the follow-up question is about, and it is the part the
	// 320-rune cut removed — which is how a reply that DID state the start time
	// became "平台没有记录" on the next turn.
	require.Contains(t, modelInput, "启动时间：2026-07-19 11:35",
		"the end of a long prior reply must survive into the next turn")
	require.NotContains(t, modelInput, "…",
		"no ellipsis: the restored exchange must not be a truncated projection")
	// Line structure carries the table; collapsing it to one line is the other
	// half of the damage and is not covered by the length assertion above.
	require.Contains(t, modelInput, "| host-demo-11 | uhost-demo11 | Running | 4090 |")
}

func TestReplayedExchangeStillRedactsCredentials(t *testing.T) {
	const secret = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIiwibmFtZSI6ImEifQ.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	// Assert the redactor really fires on this fixture. Without this, a test that
	// only checks "secret absent from prompt" passes just as well when the secret
	// was never redacted but merely truncated away, or when the pattern stopped
	// matching — the empty-gate shape this repo keeps rediscovering.
	//
	// This guard calls the SECURITY primitive, deliberately not the engine wrapper
	// under test: pointing it at safeConversationText would make the guard and the
	// subject the same code, so removing the redaction would trip the guard instead
	// of the assertion it is supposed to protect, and the real assertion would never
	// be shown to fire.
	require.NotContains(t, security.RedactOperationalTokensInText(secret), secret,
		"fixture must actually trigger redaction or this test proves nothing")

	// Pad past the old cut point so a truncating implementation could not be what
	// removes the secret: here the secret sits in the first 320 runes and the
	// padding tail sits beyond it.
	reply := "已为你签发访问令牌：Authorization: Bearer " + secret + "\n" + longAssistantReply()

	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "好的"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "给我一个令牌"},
		{Role: openai.ChatMessageRoleAssistant, Content: reply},
	}

	_, err := eng.Chat(context.Background(), "刚才那台怎么启动", noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 1)
	modelInput := renderTestMessages(mock.calls[0].Messages)

	require.NotContains(t, modelInput, secret,
		"dropping the compaction must not drop the redaction with it")
	// Proves we are on the verbatim path, so the assertion above is not passing
	// merely because the whole reply was cut at 320 runes.
	require.Contains(t, modelInput, "启动时间：2026-07-19 11:35")
}

// TestAgentContextPairsCoverTheSessionTurnCap pins the two constants that must
// move together. The restored pair window is the model's whole cross-turn memory,
// so a session permitted to run longer than the window spends turns the model
// cannot see — silently, with no error and no log line.
func TestAgentContextPairsCoverTheSessionTurnCap(t *testing.T) {
	require.GreaterOrEqual(t, maxAgentContextPairs, config.DefaultMaxSessionTurns,
		"raising the turn cap without raising the pair window buys invisible turns")

	// The Go default is only the fallback; production sets the value explicitly,
	// so the shipped file is what actually has to hold.
	raw, err := os.ReadFile("../../deploy/conf/config.yaml")
	require.NoError(t, err)
	match := regexp.MustCompile(`(?m)^\s*max_session_turns:\s*(\d+)`).FindSubmatch(raw)
	require.NotNil(t, match, "max_session_turns not found in the shipped config")
	shipped, err := strconv.Atoi(string(match[1]))
	require.NoError(t, err)
	require.LessOrEqual(t, shipped, maxAgentContextPairs,
		"deploy/conf/config.yaml lets a session run past the model's memory window")
}
