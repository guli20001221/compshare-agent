//go:build live

// Runs an arbitrary list of real user queries through the CURRENT production
// answer stack — gpt-5.6-terra (the merged default) over the reranked qwen3_rrf
// retriever — and dumps the replies for a faithfulness read. Unlike the
// source-gap probe this joins no audit; it just answers whatever queries it is
// given, so it can be pointed at a fresh traffic sample.
//
// Retrieval stays on the platform key (mergedProductionIndex + reranker); the
// answer model runs on answerLLMConfig (terra when COMPSHARE_ANSWER_API_KEY is
// set, else deepseek-v4-pro). Context is "用户当前没有实例", the no-instance shape,
// so the agent must answer from knowledge.
//
// Real customer questions are never committed. The query list is read from
// COMPSHARE_QUERY_LIST (jsonl: {case_id, query}); full replies go to
// COMPSHARE_PROBE_OUT, outside the repo. Nothing is written into the tree.
//
//	go test ./internal/engine -tags live -run TestLiveAnswerQueryList -v -timeout 60m
package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

type queryListCase struct {
	CaseID    string `json:"case_id"`
	Query     string `json:"query"`
	CreatedAt string `json:"created_at,omitempty"`
}

type queryListAnswer struct {
	CaseID   string `json:"case_id"`
	Query    string `json:"query"`
	Searches int    `json:"searches"`
	Answer   string `json:"answer"`
	Err      string `json:"err,omitempty"`
}

func loadQueryList(t *testing.T) []queryListCase {
	t.Helper()
	path := os.Getenv("COMPSHARE_QUERY_LIST")
	if path == "" {
		t.Skip("COMPSHARE_QUERY_LIST not set; real questions are never committed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read query list: %v", err)
	}
	var out []queryListCase
	for _, line := range splitJSONLines(raw) {
		var one queryListCase
		if err := json.Unmarshal(line, &one); err != nil {
			t.Fatalf("query list line: %v", err)
		}
		if strings.TrimSpace(one.Query) == "" {
			continue
		}
		out = append(out, one)
	}
	return out
}

func TestLiveAnswerQueryList(t *testing.T) {
	cfg := loadLiveConfig(t)
	answerCfg := answerLLMConfig(cfg)

	smoke, err := llm.NewClient(answerCfg).Chat(context.Background(), llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "回复两个字：在吗"}},
	})
	if err != nil {
		t.Fatalf("answer model %q not reachable on %s: %v", answerCfg.Model, answerCfg.BaseURL, err)
	}
	t.Logf("answer-model=%s reachable; smoke=%q", answerCfg.Model, strings.TrimSpace(firstNonEmptyText(smoke.Content, "(empty)")))

	cases := loadQueryList(t)
	if len(cases) == 0 {
		t.Fatalf("query list is empty")
	}
	corpus, sidecar := mergedProductionIndex(t)
	retriever := rerankedProductionRetriever(t, cfg, corpus, sidecar)
	t.Logf("cases=%d  merged index=%d chunks", len(cases), len(corpus.Chunks))

	results := make([]queryListAnswer, len(cases))
	for i, c := range cases {
		eng := NewWithDeps(llm.NewClient(answerCfg), &mockExecutor{}, nil)
		eng.SetKnowledgeRetriever(retriever)
		eng.InitWithContext("用户当前没有实例。")
		reply, cerr := eng.Chat(context.Background(), c.Query, noopStep)
		r := queryListAnswer{
			CaseID:   c.CaseID,
			Query:    c.Query,
			Searches: eng.searchKnowledgeCallsThisTurn,
			Answer:   strings.TrimSpace(reply),
		}
		if cerr != nil {
			r.Err = cerr.Error()
		}
		results[i] = r
		t.Logf("\n======== %s ========\n问：%s\n检索：calls=%d\n答：\n%s",
			c.CaseID, c.Query, r.Searches, truncateRunes(r.Answer, 900))
		if r.Err != "" {
			t.Logf("  ERR: %s", r.Err)
		}
		if r.Searches == 0 {
			t.Logf("  ⚠ 未调用 SearchKnowledge（纯生成）")
		}
	}

	if out := os.Getenv("COMPSHARE_PROBE_OUT"); out != "" {
		blob, mErr := json.MarshalIndent(results, "", "  ")
		if mErr != nil {
			t.Fatalf("marshal: %v", mErr)
		}
		if wErr := os.WriteFile(out, blob, 0o600); wErr != nil {
			t.Fatalf("write: %v", wErr)
		}
		t.Logf("full replies -> %s", out)
	}
}
