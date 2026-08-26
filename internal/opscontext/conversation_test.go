package opscontext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConversationAnchorReturnsExactNewSuffix(t *testing.T) {
	first := []ConversationMessage{
		{Role: ConversationRoleUser, Content: "第一轮问题"},
		{Role: ConversationRoleAssistant, Content: "第一轮回答"},
		{Role: ConversationRoleUser, Content: "直接按上面的来"},
	}
	anchor := ConversationAnchor(first)
	require.Len(t, anchor, 64)

	next := append(append([]ConversationMessage(nil), first...),
		ConversationMessage{Role: ConversationRoleAssistant, Content: "已按原参数提交"},
		ConversationMessage{Role: ConversationRoleUser, Content: "继续检查结果"},
	)
	delta, ok := ConversationAfterAnchor(next, anchor)
	require.True(t, ok)
	require.Equal(t, next[len(first):], delta)

	// Returned slices cannot mutate the producer's canonical snapshot.
	delta[0].Content = "changed"
	require.Equal(t, "已按原参数提交", next[len(first)].Content)
}

func TestConversationAnchorDistinguishesRepeatedTextByPosition(t *testing.T) {
	first := []ConversationMessage{
		{Role: ConversationRoleUser, Content: "继续"},
		{Role: ConversationRoleAssistant, Content: "已检查"},
		{Role: ConversationRoleUser, Content: "继续"},
	}
	anchor := ConversationAnchor(first)
	next := append(append([]ConversationMessage(nil), first...),
		ConversationMessage{Role: ConversationRoleAssistant, Content: "第二次检查完成"},
		ConversationMessage{Role: ConversationRoleUser, Content: "继续"},
	)
	delta, ok := ConversationAfterAnchor(next, anchor)
	require.True(t, ok)
	require.Equal(t, next[3:], delta, "a repeated user string must not match the earlier position")
}

func TestConversationAnchorMissRequiresFreshSnapshot(t *testing.T) {
	history := []ConversationMessage{{Role: ConversationRoleUser, Content: "new bounded tail"}}
	_, ok := ConversationAfterAnchor(history, ConversationAnchor([]ConversationMessage{{Role: ConversationRoleUser, Content: "dropped prefix"}}))
	require.False(t, ok)
	_, ok = ConversationAfterAnchor(history, "not-a-digest")
	require.False(t, ok)
}
