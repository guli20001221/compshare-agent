//go:build live

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// TestLiveJudgeModelIsReachable checks whether the judge used by the previous
// RAG evaluation can still be called with the credentials this deployment
// carries, before an A/B is designed around it.
//
// A judge that turns out to be unreachable after the arms have run means either
// re-running both arms or silently swapping in a different judge — and swapping
// the judge mid-comparison makes the two arms unscoreable against each other.
func TestLiveJudgeModelIsReachable(t *testing.T) {
	cfg := loadLiveConfig(t)
	for _, model := range []string{
		"doubao-seed-2-1-turbo-260628", // judge used by rag_v2_real_chat_retrieval_2026-07-15
		cfg.Agent.LLM.Model,            // control: the model every other live probe uses
	} {
		llmCfg := cfg.Agent.LLM
		llmCfg.Model = model
		resp, err := llm.NewClient(llmCfg).Chat(context.Background(), llm.ChatRequest{
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: "Reply with the single word: ok"},
			},
		})
		switch {
		case err != nil:
			t.Logf("model %-30s UNREACHABLE: %v", model, err)
		case resp == nil || strings.TrimSpace(resp.Content) == "":
			t.Logf("model %-30s reachable but returned empty content", model)
		default:
			t.Logf("model %-30s OK: %q", model, strings.TrimSpace(resp.Content))
		}
	}
}
