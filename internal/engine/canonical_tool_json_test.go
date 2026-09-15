package engine

import (
	"encoding/json"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tool observation is replayed byte-for-byte: no field is renamed, removed or
// re-encoded, so number spelling, key order and whitespace survive a restart and
// the model reads on the next turn exactly what it read on this one.
func TestCanonicalToolJSONIsReplayedVerbatim(t *testing.T) {
	for _, raw := range []string{
		`{"body":"credential.PrivateKey = \"synthetic-private-value\"\n继续阅读文档","nested":[{"Password":"synthetic-password","id":9007199254740993}],"amount":1.2300e+12}`,
		" {\n  \"id\" : 9007199254740993, \"body\":\"alice@example.com 13800138000 <docs>\", \"project\":\"12345678-1234-1234-1234-1234567890ab\"\n}\n",
		`[null,true,1.2300e+12,{"content":"C:\\models\\example"}]`,
		"plain text Password=" + "synthetic-password and TCP 8188",
		`{"body":"token=synthetic-token"} trailing prose`,
	} {
		assert.Equal(t, raw, canonicalConversationText(openai.ChatMessageRoleTool, raw))
	}
}

func TestCanonicalToolJSONHotColdReplayKeepsArgumentsAndCompletedObservation(t *testing.T) {
	const question = "查 TCP 8188 的开放方法，只读"
	const args = `{"query":"credential.PrivateKey = \"synthetic-arg-text\"","id":9007199254740993,"AccessKey":"synthetic-arg-key"}`
	const observation = `{"action":"SearchKnowledge","data":{"body":"credential.PrivateKey = \"synthetic-result-text\"\nTCP 8188","AccessKey":"synthetic-access-key","id":9007199254740993}}`
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
					assert.Equal(t, args, call.Function.Arguments, "the recorded call carries the arguments the model wrote")
				}
				if msg.Role == openai.ChatMessageRoleTool && msg.ToolCallID == "read-only-call" {
					results++
					require.True(t, json.Valid([]byte(msg.Content)))
					assert.Equal(t, observation, msg.Content, "the recorded observation is the one the model read")
				}
			}
			assert.Equal(t, 1, calls, "the recorded call must remain visible exactly once, not disappear after broken-argument rejection")
			assert.Equal(t, 1, results, "the paired completed observation must remain visible exactly once")
		})
	}
}
