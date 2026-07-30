package engine

import (
	"context"
	"strconv"
	"strings"
	"testing"

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

// restoredHistory returns only what the assembler replayed from PRIOR turns:
// everything after the leading system/card block, up to the current user message.
//
// Assertions target these messages rather than a rendered concatenation of the
// whole request. A whole-request match is satisfiable by unrelated prompt text
// and breakable by it too — an earlier draft asserted the request contained no
// "…" anywhere, which the system prompt could start failing for reasons that have
// nothing to do with history.
func restoredHistory(msgs []openai.ChatCompletionMessage) []string {
	start := 0
	for start < len(msgs) && msgs[start].Role == openai.ChatMessageRoleSystem {
		start++
	}
	end := start
	for i := len(msgs) - 1; i >= start; i-- {
		if msgs[i].Role == openai.ChatMessageRoleUser {
			end = i
			break
		}
	}
	if end < start {
		end = start
	}
	out := make([]string, 0, end-start)
	for _, m := range msgs[start:end] {
		out = append(out, m.Content)
	}
	return out
}

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

// longUserMessage is the other side of the same defect. Production's second
// largest message class is a whole terminal log pasted with no question, which
// the whitespace collapse alone destroys even below the rune cut.
func longUserMessage() string {
	var b strings.Builder
	b.WriteString("报错日志如下，帮我看下：\n")
	for i := 0; i < 14; i++ {
		b.WriteString("  File \"/root/app/mod_" + strconv.Itoa(i) + ".py\", line " + strconv.Itoa(40+i) + ", in forward\n")
	}
	b.WriteString("RuntimeError: CUDA out of memory. Tried to allocate 20.00 MiB\n")
	return b.String()
}

func runOneTurnWithHistory(t *testing.T, prior []openai.ChatCompletionMessage, question string) []string {
	t.Helper()
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "好的"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.messages = append([]openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
	}, prior...)

	_, err := eng.Chat(context.Background(), question, noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 1)
	return restoredHistory(mock.calls[0].Messages)
}

func TestReplayedExchangeKeepsALongAssistantReplyIntact(t *testing.T) {
	reply := longAssistantReply()
	// Guard against a vacuous test: if the fixture ever shrinks below the old cut
	// point, the assertions below would pass on the truncating code too.
	require.Greater(t, len([]rune(reply)), maxSemanticRunes,
		"fixture must exceed the old 320-rune cut or this test cannot fail")

	history := runOneTurnWithHistory(t, []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "我有哪些实例"},
		{Role: openai.ChatMessageRoleAssistant, Content: reply},
	}, "第一台是什么时候启动的")

	// Byte-exact, not "contains": this fails on truncation AND on whitespace
	// collapse, and cannot be satisfied by prompt text that merely mentions the
	// same words.
	require.Contains(t, history, reply,
		"a long prior reply must be replayed exactly, tail and line structure included")
}

func TestReplayedExchangeKeepsALongUserMessageIntact(t *testing.T) {
	pasted := longUserMessage()
	require.Greater(t, len([]rune(pasted)), maxSemanticRunes,
		"fixture must exceed the old 320-rune cut or this test cannot fail")

	history := runOneTurnWithHistory(t, []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: pasted},
		{Role: openai.ChatMessageRoleAssistant, Content: "显存不足，建议减小 batch size。"},
	}, "那要调到多少")

	require.Contains(t, history, pasted,
		"the user side is squashed by the same call and needs its own gate")
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
	// of the assertion it is supposed to protect.
	require.NotContains(t, security.RedactOperationalTokensInText(secret), secret,
		"fixture must actually trigger redaction or this test proves nothing")

	reply := "已为你签发访问令牌：Authorization: Bearer " + secret + "\n" + longAssistantReply()
	history := runOneTurnWithHistory(t, []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "给我一个令牌"},
		{Role: openai.ChatMessageRoleAssistant, Content: reply},
	}, "刚才那台怎么启动")

	joined := strings.Join(history, "\n")
	require.NotContains(t, joined, secret,
		"dropping the compaction must not drop the redaction with it")
	// Proves we are on the verbatim path, so the assertion above is not passing
	// merely because the whole reply was cut at 320 runes.
	require.Contains(t, joined, "启动时间：2026-07-19 11:35")
}

// --- size budget -----------------------------------------------------------
//
// Replaying verbatim removed the only bound the restored exchanges had: 8 pairs
// x 2 sides x 320 runes was a hard 5,120-rune ceiling regardless of what the user
// pasted. maxReplayedHistoryRunes is the replacement, and it must shed WHOLE
// exchanges — a reply cut mid-table is worse than an absent one, because the
// model cannot tell it is reading a fragment.

func pair(user, assistant string) ConversationPair {
	return ConversationPair{User: user, Assistant: assistant}
}

func TestBudgetDropsWholeOldestExchangesAndNeverTruncates(t *testing.T) {
	pairs := []ConversationPair{
		pair(strings.Repeat("旧", 100), strings.Repeat("旧", 100)),
		pair(strings.Repeat("中", 100), strings.Repeat("中", 100)),
		pair(strings.Repeat("新", 100), strings.Repeat("新", 100)),
	}
	// Room for two exchanges (400 runes) but not three (600).
	kept := budgetReplayedPairs(pairs, 450)

	require.Len(t, kept, 2, "the oldest exchange must be dropped whole")
	require.Equal(t, pairs[1], kept[0])
	require.Equal(t, pairs[2], kept[1])
	for _, k := range kept {
		require.Equal(t, 100, len([]rune(k.User)), "a surviving message must not be shortened")
		require.Equal(t, 100, len([]rune(k.Assistant)), "a surviving message must not be shortened")
	}
}

func TestBudgetKeepsTheNewestExchangeEvenWhenItAloneExceeds(t *testing.T) {
	huge := pair(strings.Repeat("问", 5000), strings.Repeat("答", 5000))
	kept := budgetReplayedPairs([]ConversationPair{
		pair("旧问", "旧答"),
		huge,
	}, 1000)

	require.Equal(t, []ConversationPair{huge}, kept,
		"the immediately preceding exchange is what follow-ups refer to; "+
			"dropping it to respect a budget reintroduces the amnesia being fixed")
}

func TestBudgetLeavesOrdinaryHistoryUntouched(t *testing.T) {
	// p99 of a full-history replay in the production exports is 5,764 runes, so a
	// realistic session must be entirely under budget or the fix is undone for
	// everyone by its own guard.
	pairs := make([]ConversationPair, 0, 20)
	for i := 0; i < 20; i++ {
		pairs = append(pairs, pair("问题"+strconv.Itoa(i), strings.Repeat("答", 255)))
	}
	require.Equal(t, pairs, budgetReplayedPairs(pairs, maxReplayedHistoryRunes),
		"20 median-sized exchanges must still replay complete")
}
