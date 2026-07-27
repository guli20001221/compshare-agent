package engine

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

const (
	maxKnowledgePlanQueries = 3
	// maxForcedHopPlanQueries is the wider cap for the forced first hop: the raw
	// user words plus up to three expansions. That is the exact shape the
	// reachability probe measured — over 24 real questions the production stack
	// had failed on, raw+3 expansions reached usable evidence on 23-24 of them
	// from the same index, and re-judging the reached chunks moved 6 from
	// "retrieval found nothing" to "the corpus fully answers this". Shipping a
	// narrower fan-out than the one that was measured would be shipping an
	// unmeasured recipe. Four still leaves half of maxRetrievalQueriesPerTurn for
	// the Agent's own follow-ups.
	maxForcedHopPlanQueries       = 4
	maxKnowledgePlanQuestionRunes = 600
	maxKnowledgePlanQueryRunes    = 400
)

// knowledgeQueryPlannerTemperature pins this call's sampling. It is a var only
// because ChatRequest.Temperature is a pointer (nil = provider default, so every
// other caller is untouched); nothing mutates it.
var knowledgeQueryPlannerTemperature float32 = 0

const knowledgeQueryPlannerPrompt = `你是知识检索问题整理器。仅输出 JSON，不回答问题。
把当前用户问题结合必要的对话历史整理成：
{"answer_question":"用户现在真正要解决的、脱离历史也能理解的完整问题","search_queries":["用于检索的完整问题"]}
要求：
- 当前问题优先；用户已经换话题时不要继承旧主题。
- 消解“它、这个、那关机后、浏览器里呢”等指代，保留产品、环境、计费方式和限制条件。
- 不把助手过去的回答当成事实，不添加对话中没有的条件。
- search_queries 为 1 到 3 条；简单问题只写 1 条，确有多个独立方面时才拆分。`

// knowledgeQueryExpanderPrompt is the forced-hop variant. The default prompt's
// job is de-referencing ("消解 它/这个"), and it does that job correctly — which is
// exactly why the forced hop needed something else. Measured on the flip cases:
// for an already-grammatical question the planner found nothing to resolve and
// echoed it back verbatim (三轮 resolved 全等于原话), the raw words retrieved 8
// hits that the relevance floor then dropped in full, and the turn ended with an
// empty ledger unless the Agent happened to search again — which it did not do
// in 9 of the 10 empty turns.
//
// The failing input is not an ambiguous question, it is an UNDER-SPECIFIED one:
// no subject, no product, colloquial wording that the documentation never uses.
// So this prompt asks for expansion, not resolution, and asks for several
// queries — the "简单问题只写 1 条" rule in the default prompt is the wrong
// instinct here, since one bad query is precisely the failure mode.
//
// answer_question stays faithful. It is the stable question answer verification
// and the grounded-citation check key on, so widening it would move the target
// the answer is checked against; only search_queries expand.
const knowledgeQueryExpanderPrompt = `你是知识检索查询扩写器。仅输出 JSON，不回答问题。
这句话是系统在用户没有明确提问时自动拿去检索的原话，往往省略主语、用口语说法，直接拿去检索会检不到平台文档。
{"answer_question":"用户真正要解决的问题","search_queries":["检索查询1","检索查询2","检索查询3"]}
要求：
- answer_question 忠实于用户原话，只做指代消解和主语补全，不扩写、不加入用户没说的条件。
- search_queries 写 2 到 3 条互不相同的检索查询：补全省略的主语和使用场景（GPU 云实例、镜像、计费、存储、网络、控制台等），把口语化表述换成平台文档里会出现的说法，铺开同义词和相关术语。
- 每条都要能独立拿去检索，不要只是复述原句。
- 当前问题优先；用户已经换话题时不要继承旧主题，不把助手过去的回答当成事实。`

type knowledgeQueryPlan struct {
	AnswerQuestion string   `json:"answer_question"`
	SearchQueries  []string `json:"search_queries"`
}

type knowledgeQueryPlanInput struct {
	Conversation  []ConversationPair `json:"conversation"`
	Current       string             `json:"current_question"`
	ProposedQuery string             `json:"agent_proposed_query"`
}

// planKnowledgeQuery adds one bounded contextualization call only for a knowledge
// search that has prior conversation to resolve. It is not a second answer agent:
// the call can only return a standalone answer target plus retrieval queries.
// Any transport, parse, or validation failure falls back to the Agent-proposed
// query, so retrieval availability never depends on the planner.
func (e *Engine) planKnowledgeQuery(ctx context.Context, proposed string) knowledgeQueryPlan {
	fallback := fallbackKnowledgeQueryPlan(proposed)
	if e == nil || e.llmClient == nil || strings.TrimSpace(proposed) == "" || !e.turnContextViewReady {
		return fallback
	}
	// A first turn has no references to resolve, which is why this call was
	// skipped there — the planner's whole stated job was contextualization.
	//
	// The forced-hop arm gives it a second job. That arm searches on the user's
	// raw words, and the raw words are exactly the input the arm exists to
	// handle: a complaint or a bare statement ("Coding Plan 套餐好贵"), which the
	// Agent never turned into a query at all. Restating it is a rewrite, not a
	// judgement, and it is the only stage that can do so before retrieval runs.
	//
	// Gated on the same flag rather than enabled outright: on a first turn with
	// nothing to resolve, this call earns its latency and tokens only when the
	// engine is committing to a retrieval regardless.
	if len(e.turnContextViewThisTurn.RecentConversation) == 0 && !forcedKnowledgeHopEnabled {
		return fallback
	}

	input := knowledgeQueryPlanInput{
		Conversation:  append([]ConversationPair(nil), e.turnContextViewThisTurn.RecentConversation...),
		Current:       strings.TrimSpace(e.turnContextViewThisTurn.CurrentQuestion),
		ProposedQuery: strings.TrimSpace(proposed),
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fallback
	}
	// The forced hop searches on words the user never aimed at retrieval, so it
	// gets the expander; every Agent-issued search keeps the de-referencing
	// planner it was written for.
	systemPrompt := knowledgeQueryPlannerPrompt
	maxQueries := maxKnowledgePlanQueries
	if e.forcedHopSearchInFlight {
		systemPrompt = knowledgeQueryExpanderPrompt
		// One slot is reserved for the user's own words, which are appended
		// below. Validating at the full cap first would let a four-expansion
		// reply fill every slot and push the raw form out — the one query whose
		// presence makes this change unable to lose recall.
		maxQueries = maxForcedHopPlanQueries - 1
	}
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: string(payload)},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
		// Pinned sampling. This call resolves references and restates one
		// question as retrieval queries — a rewrite, not a judgement, so the
		// same input should not produce a different retrieval set run to run.
		//
		// It still does. A/A over 50 real 2026-06-26..07-09 questions (same arm
		// twice, deterministic BM25 retrieval so only this call varied):
		//
		//	provider default : chunk-set flip 56%, mean Jaccard 0.708, count flip 16%
		//	pinned to 0      : chunk-set flip 50%, mean Jaccard 0.754, count flip 10%
		//
		// So the pin removes one confound and little else — the residual is not
		// sampling temperature. Anything that compares retrieval arms case by
		// case is unreadable at a 50% self-flip rate; such a comparison needs
		// repeated runs per case, or a metric that does not key on chunk-set
		// identity. Keeping the pin because a rewrite should not be sampled, NOT
		// because it made this call reproducible.
		Temperature: &knowledgeQueryPlannerTemperature,
	})
	if err != nil || resp == nil {
		return fallback
	}
	e.emitTokenUsage(resp.Usage)

	var planned knowledgeQueryPlan
	if !parseFirstJSONObject(resp.Content, &planned) {
		return fallback
	}
	plan := validateKnowledgeQueryPlan(planned, fallback, maxQueries)
	if e.forcedHopSearchInFlight {
		// Keep the user's own words as one of the queries. Expansion is an LLM
		// rewrite and can drift off what was asked; retaining the raw form makes
		// the forced hop a superset of what it retrieves today, so the change can
		// add recall but not subtract it.
		//
		// LAST, not first. Retrievals are charged against
		// maxRetrievalQueriesPerTurn in order, so whichever query sits at the
		// front is the one guaranteed to run — and the raw form is the one
		// measured to floor-drop in full. Priority belongs to the expansions.
		plan.SearchQueries = appendQuery(plan.SearchQueries, strings.TrimSpace(proposed), maxForcedHopPlanQueries)
	}
	return plan
}

// appendQuery adds query at the end unless it is already present, respecting the
// cap.
func appendQuery(queries []string, query string, maxQueries int) []string {
	if query == "" || len(queries) >= maxQueries {
		return queries
	}
	key := strings.ToLower(query)
	for _, q := range queries {
		if strings.ToLower(strings.TrimSpace(q)) == key {
			return queries
		}
	}
	return append(queries, query)
}

func fallbackKnowledgeQueryPlan(query string) knowledgeQueryPlan {
	query = strings.TrimSpace(query)
	if query == "" {
		return knowledgeQueryPlan{}
	}
	return knowledgeQueryPlan{AnswerQuestion: query, SearchQueries: []string{query}}
}

func validateKnowledgeQueryPlan(planned, fallback knowledgeQueryPlan, maxQueries int) knowledgeQueryPlan {
	if maxQueries <= 0 {
		maxQueries = maxKnowledgePlanQueries
	}
	answer := truncateRunes(strings.TrimSpace(planned.AnswerQuestion), maxKnowledgePlanQuestionRunes)
	if answer == "" {
		answer = fallback.AnswerQuestion
	}

	queries := make([]string, 0, maxQueries)
	seen := map[string]struct{}{}
	for _, raw := range planned.SearchQueries {
		query := truncateRunes(strings.TrimSpace(raw), maxKnowledgePlanQueryRunes)
		if query == "" {
			continue
		}
		key := strings.ToLower(query)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		queries = append(queries, query)
		if len(queries) == maxQueries {
			break
		}
	}
	if len(queries) == 0 {
		queries = []string{answer}
	}
	return knowledgeQueryPlan{AnswerQuestion: answer, SearchQueries: queries}
}
