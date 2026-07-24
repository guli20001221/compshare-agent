//go:build live

// Shows what the CURRENT production agent actually answers, on deepseek-v4-pro,
// for the nine cases the manual audit labelled source_document_gap. The point is
// not a metric — it is to read the real replies and see, per case, whether the
// agent:
//
//   - reasonably infers an answer the docs imply but do not state outright,
//   - grounds a partial answer on what the corpus does have,
//   - honestly says it lacks the specific documentation, or
//   - invents a service or policy the platform does not have.
//
// The last one is the failure mode that makes a "no public doc" exit dangerous,
// so the replies are the evidence for whether that exit is safe to build and how
// it must be scoped.
//
// Faithfulness / caveats, stated so the replies are not over-read:
//   - Model is deepseek-v4-pro (agent tier), overridable via COMPSHARE_PRO_MODEL.
//     It is NOT in the capability matrix, so it runs on safe-default caps: no
//     forced first hop, no json_object planner. The agent therefore reaches
//     SearchKnowledge via ordinary auto tool choice; searchKnowledgeCallsThisTurn
//     is recorded per case, and a 0 there means the agent chose not to retrieve,
//     which is itself worth seeing.
//   - Retrieval is production qwen3_rrf over the merged platform+external index,
//     WITH the qwen3-reranker-8b stage (rerankedProductionRetriever). The reranker
//     is load-bearing here, not cosmetic: without it qwen3_rrf emits RRF-fusion
//     scores (~0.03) that the engine's isWeakEvidence 0.5 floor rejects on every
//     query, emptying the ledger and starving the agent (floor_reranker probe).
//   - Context is "用户当前没有实例", the no-instance shape, so the agent cannot
//     lean on a live instance API and must answer from knowledge — which is the
//     condition under which the "no doc" exit would fire.
//
// Real customer questions are never committed. Queries are read from
// COMPSHARE_REAL_QUERY_CORPUS; full replies go to COMPSHARE_PROBE_OUT (outside
// the repo). Nothing here is written into the tree.
//
//	go test ./internal/engine -tags live -run TestLiveProAnswersSourceGaps -v -timeout 40m
package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/embedding"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

func proModelName() string {
	if m := strings.TrimSpace(os.Getenv("COMPSHARE_PRO_MODEL")); m != "" {
		return m
	}
	return "deepseek-v4-pro"
}

// answerLLMConfig returns the llm config for the ANSWER model only. The
// retriever's embedder + reranker are always built from cfg's own key (the
// qwen3 embed/rerank stack is authorized ONLY under the platform key — a
// gpt-5.6-terra key 400s on qwen3-embedding-8b / qwen3-reranker-8b, verified
// live 2026-07-24). ModelVerse authorizes models per key, so a cross-vendor
// answer-model comparison MUST keep retrieval on cfg's key and swap only the
// answer client:
//
//	COMPSHARE_ANSWER_API_KEY set  -> answer runs on that key + COMPSHARE_ANSWER_MODEL
//	                                 (default gpt-5.6-terra), same base_url as cfg.
//	COMPSHARE_ANSWER_API_KEY unset -> answer runs on cfg's key + proModelName()
//	                                 (deepseek-v4-pro) — the pre-existing behavior.
//
// The key is never committed: it is read from env at run time only.
func answerLLMConfig(cfg *config.Config) config.LLMConfig {
	ac := cfg.Agent.LLM // value copy; retriever keeps cfg.Agent.LLM untouched
	if k := strings.TrimSpace(os.Getenv("COMPSHARE_ANSWER_API_KEY")); k != "" {
		ac.APIKey = k
		ac.Model = "gpt-5.6-terra"
		if m := strings.TrimSpace(os.Getenv("COMPSHARE_ANSWER_MODEL")); m != "" {
			ac.Model = m
		}
		if b := strings.TrimSpace(os.Getenv("COMPSHARE_ANSWER_BASE_URL")); b != "" {
			ac.BaseURL = b
		}
		return ac
	}
	ac.Model = proModelName()
	return ac
}

// retrievalKey returns the key for the qwen3 embed/rerank stack: the separate
// agent.retrieval.api_key when set (the two-key production layout, where the
// answer key may be a gpt-5.6-terra key NOT authorized for qwen3), else
// agent.llm.api_key (single-key mode). Mirrors production's
// modelverseAPIKeyFromEnv precedence (MODELVERSE_API_KEY before LLM_API_KEY).
func retrievalKey(cfg *config.Config) string {
	if k := strings.TrimSpace(cfg.Agent.Retrieval.APIKey); k != "" {
		return k
	}
	return cfg.Agent.LLM.APIKey
}

type proAnswer struct {
	CaseID   string `json:"case_id"`
	Category string `json:"category"`
	Query    string `json:"query"`
	AuditNote string `json:"audit_note"`
	Searches int    `json:"searches"`
	Queries  int    `json:"queries"`
	Answer   string `json:"answer"`
	Err      string `json:"err,omitempty"`
}

func TestLiveProAnswersSourceGaps(t *testing.T) {
	cfg := loadLiveConfig(t)
	answerCfg := answerLLMConfig(cfg) // answer model may run on a separate (terra) key

	// Reachability smoke first: the answer model may live on the Anthropic-compat
	// endpoint rather than this /v1 one, and an unreachable model looks exactly
	// like a bad answer once it is buried in an agent loop. Fail loud, early.
	smoke, err := llm.NewClient(answerCfg).Chat(context.Background(), llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "回复两个字：在吗"}},
	})
	if err != nil {
		t.Fatalf("model %q not reachable on %s: %v", answerCfg.Model, answerCfg.BaseURL, err)
	}
	t.Logf("answer-model=%s reachable; smoke reply=%q", answerCfg.Model, strings.TrimSpace(firstNonEmptyText(smoke.Content, "(empty)")))

	questions := loadQuestionText(t)
	gaps := loadSourceGapCases(t, questions)
	if len(gaps) == 0 {
		t.Fatalf("no source_document_gap case joined a question")
	}
	corpus, sidecar := mergedProductionIndex(t)
	// MUST include the reranker: without it qwen3_rrf emits RRF-fusion scores
	// (~0.03) that the engine's isWeakEvidence 0.5 floor rejects on every query,
	// starving the agent of evidence (see floor_reranker_probe). This mirrors
	// production, which wires the reranker for qwen3_rrf (cmd/trace.go).
	retriever := rerankedProductionRetriever(t, cfg, corpus, sidecar)

	results := make([]proAnswer, len(gaps))
	// Serial, not concurrent: pro is slow and the point is to read the replies,
	// not to race them; serial also keeps one hung call from being ambiguous.
	for i, g := range gaps {
		eng := NewWithDeps(llm.NewClient(answerCfg), &mockExecutor{}, nil)
		eng.SetKnowledgeRetriever(retriever)
		eng.InitWithContext("用户当前没有实例。")
		reply, cerr := eng.Chat(context.Background(), g.Query, noopStep)
		r := proAnswer{
			CaseID:    g.CaseID,
			Category:  g.Category,
			Query:     g.Query,
			AuditNote: g.AuditNote,
			Searches:  eng.searchKnowledgeCallsThisTurn,
			Queries:   eng.searchKnowledgeQueriesThisTurn,
			Answer:    strings.TrimSpace(reply),
		}
		if cerr != nil {
			r.Err = cerr.Error()
		}
		results[i] = r
		t.Logf("\n================ %s [%s] ================\n问：%s\n检索：calls=%d queries=%d\n答：\n%s",
			g.CaseID, g.Category, g.Query, r.Searches, r.Queries, truncateRunes(r.Answer, 1200))
		if r.Err != "" {
			t.Logf("  ERR: %s", r.Err)
		}
		if r.Searches == 0 {
			t.Logf("  ⚠ 本条 agent 未调用 SearchKnowledge（0 检索）——答案是纯生成，注意是否凭空")
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

type sourceGapCase struct {
	CaseID    string
	Category  string
	Query     string
	AuditNote string
}

// loadSourceGapCases takes the nine case ids the audit labelled
// source_document_gap, and joins the question text from the uncommitted corpus.
func loadSourceGapCases(t *testing.T, questions map[string]string) []sourceGapCase {
	t.Helper()
	path := filepath.Join("..", "..", "eval", "rag_v2_real_chat_gap_audit_2026-07-15.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var file struct {
		Cases []struct {
			CaseID         string `json:"case_id"`
			Classification string `json:"classification"`
			Note           string `json:"note"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse audit: %v", err)
	}
	var out []sourceGapCase
	for _, c := range file.Cases {
		if c.Classification != "source_document_gap" {
			continue
		}
		q := strings.TrimSpace(questions[c.CaseID])
		if q == "" {
			t.Fatalf("source gap %s has no question text; corpus and audit disagree", c.CaseID)
		}
		category := ""
		out = append(out, sourceGapCase{CaseID: c.CaseID, Category: category, Query: q, AuditNote: c.Note})
	}
	return out
}

// loadQuestionText reads case_id -> query from the uncommitted corpus.
func loadQuestionText(t *testing.T) map[string]string {
	t.Helper()
	path := os.Getenv("COMPSHARE_REAL_QUERY_CORPUS")
	if path == "" {
		t.Skip("COMPSHARE_REAL_QUERY_CORPUS not set; real questions are never committed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	out := map[string]string{}
	for _, line := range splitJSONLines(raw) {
		var one struct {
			CaseID string `json:"case_id"`
			Query  string `json:"query"`
		}
		if err := json.Unmarshal(line, &one); err != nil {
			t.Fatalf("corpus line: %v", err)
		}
		out[one.CaseID] = one.Query
	}
	return out
}

// productionAnswerRetriever builds a qwen3_rrf retriever over the merged index
// WITHOUT the reranker. NOT floor-faithful: its RRF-fusion Score (~0.03) fails the
// engine's 0.5 isWeakEvidence floor, so it must not drive an engine turn that
// applies the floor — use rerankedProductionRetriever for that. Fine for the
// rank-only 3-way classifier (which never floors) and as the no-reranker arm of
// the floor_reranker probe. TopK is the modest window the agent actually reads.
func productionAnswerRetriever(t *testing.T, cfg *config.Config, corpus knowledge.Corpus, sidecar knowledge.EmbeddingSidecar) *knowledge.Retriever {
	t.Helper()
	embedModel := "qwen3-embedding-8b"
	embedClient, err := embedding.NewClient(embedding.ClientOptions{
		BaseURL: cfg.Agent.LLM.BaseURL,
		APIKey:  retrievalKey(cfg),
		Model:   embedModel,
	})
	if err != nil {
		t.Fatalf("embedding client: %v", err)
	}
	return knowledge.NewRetriever(corpus, knowledge.RetrieverOptions{
		TopK:             8,
		Mode:             knowledge.RetrievalModeQwen3RRF,
		EmbeddingSidecar: &sidecar,
		Embedder:         embedClient,
		EmbeddingModel:   embedModel,
		Now:              realCorpusRecallNow,
	})
}
