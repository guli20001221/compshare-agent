package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalToolJSONRedactsInsideValuesWithoutBreakingEscapes(t *testing.T) {
	const raw = `{"body":"credential.PrivateKey = \"synthetic-private-value\"\n继续阅读文档","nested":[{"Password":"synthetic-password","id":9007199254740993}],"amount":1.2300e+12}`

	got := canonicalConversationText(openai.ChatMessageRoleTool, raw)
	require.True(t, json.Valid([]byte(got)), "credential redaction must not turn a valid tool observation into broken JSON")
	assert.NotContains(t, got, "synthetic-private-value")
	assert.NotContains(t, got, "synthetic-password")
	assert.Contains(t, got, "9007199254740993", "large numeric IDs must not pass through float64")
	assert.Contains(t, got, "1.2300e+12", "JSON number spelling must survive required re-encoding")
	var decoded struct {
		Body string `json:"body"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &decoded))
	assert.Equal(t, "credential.PrivateKey = \"[REDACTED]\"\n继续阅读文档", decoded.Body)
	assert.Equal(t, got, canonicalConversationText(openai.ChatMessageRoleTool, got), "replay redaction must be idempotent")
}

func TestCanonicalToolJSONPreservesOrdinaryTextWhenNoCredentialChanges(t *testing.T) {
	for _, raw := range []string{
		" {\n  \"id\" : 9007199254740993, \"body\":\"alice@example.com 13800138000 <docs>\", \"project\":\"12345678-1234-1234-1234-1234567890ab\"\n}\n",
		`[null,true,1.2300e+12,{"content":"C:\\models\\example"}]`,
	} {
		assert.Equal(t, raw, canonicalConversationText(openai.ChatMessageRoleTool, raw), "credential-free JSON must retain formatting and ordinary facts")
	}
	for _, raw := range []string{
		"plain text Password=" + "synthetic-password and TCP 8188",
		`{"body":"token=synthetic-token"} trailing prose`,
	} {
		assert.Equal(t, security.RedactOperationalTokensInText(raw), canonicalConversationText(openai.ChatMessageRoleTool, raw), "non-JSON observations keep the existing text credential boundary")
	}
}

func TestCanonicalToolJSONHotColdReplayKeepsSafeArgumentsAndCompletedObservation(t *testing.T) {
	const question = "查 TCP 8188 的开放方法，只读"
	const args = `{"query":"credential.PrivateKey = \"synthetic-arg-secret\"","id":9007199254740993}`
	const observation = `{"action":"SearchKnowledge","data":{"body":"credential.PrivateKey = \"synthetic-result-secret\"\nTCP 8188","AccessKey":"synthetic-access-key","id":9007199254740993}}`
	for _, answer := range []string{"", "已查到 TCP 8188 的说明。"} {
		t.Run(map[bool]string{true: "interrupted", false: "completed"}[answer == ""], func(t *testing.T) {
			hot, metadata, stats := runHotTurn([]openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: question},
				{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("read-only-call", "SearchKnowledge", args)}},
				{Role: openai.ChatMessageRoleTool, ToolCallID: "read-only-call", Content: observation},
				{Role: openai.ChatMessageRoleAssistant, Content: answer},
			})
			require.True(t, stats.Attempted)
			require.NotEmpty(t, metadata)
			for _, secret := range []string{"synthetic-arg-secret", "synthetic-result-secret", "synthetic-access-key"} {
				assert.NotContains(t, string(metadata), secret)
			}

			cold := rebuildCold(question, answer, metadata)
			hotMessages := assembleNextTurn(hot, "继续刚才 TCP 8188 的问题，不执行任何命令")
			coldMessages := assembleNextTurn(cold, "继续刚才 TCP 8188 的问题，不执行任何命令")
			require.Equal(t, hotMessages, coldMessages, "hot replay and persisted cold replay must retain the same completed read-only tool work")
			assertToolCallPairsValid(t, hotMessages)
			calls, results := 0, 0
			for _, msg := range hotMessages {
				for _, call := range msg.ToolCalls {
					if call.ID != "read-only-call" {
						continue
					}
					calls++
					require.True(t, json.Valid([]byte(call.Function.Arguments)))
					assert.Contains(t, call.Function.Arguments, "9007199254740993")
					assert.NotContains(t, call.Function.Arguments, "synthetic-arg-secret")
				}
				if msg.Role == openai.ChatMessageRoleTool && msg.ToolCallID == "read-only-call" {
					results++
					require.True(t, json.Valid([]byte(msg.Content)))
					assert.Contains(t, msg.Content, "TCP 8188")
					assert.Contains(t, msg.Content, "9007199254740993")
					assert.False(t, strings.Contains(msg.Content, "synthetic-"))
				}
			}
			assert.Equal(t, 1, calls, "the recorded call must remain visible exactly once, not disappear after broken-argument rejection")
			assert.Equal(t, 1, results, "the paired completed observation must remain visible exactly once")
		})
	}
}
