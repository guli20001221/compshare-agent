package engine

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

const (
	maxKnowledgePlanQueries       = 3
	maxKnowledgePlanQuestionRunes = 600
	maxKnowledgePlanQueryRunes    = 400
)

// knowledgeQueryPlannerTemperature pins this call's sampling. It is a var only
// because ChatRequest.Temperature is a pointer (nil = provider default, so every
// other caller is untouched); nothing mutates it.
var knowledgeQueryPlannerTemperature float32 = 0

// The planner expands elliptical conversation into a standalone query while
// preserving exact error codes, ports, model names and commands. Both forms are
// searched because the reranker benefits from complete questions while precise
// tokens can be damaged by blanket rewriting.
const knowledgeQueryPlannerPrompt = `你是知识检索问题整理器。仅输出 JSON，不回答问题。
把当前用户问题结合必要的对话历史整理成：
{"answer_question":"用户现在真正要解决的、脱离历史也能理解的完整问题","search_queries":["用于检索的完整问题"]}
要求：
- 当前问题优先；用户已经换话题时不要继承旧主题。
- 消解“它、这个、那关机后、浏览器里呢”等指代，保留产品、资源形态（Pod/虚机）、运行形态（平台托管/Guest 自建）、区域、计费/存储条件、作用域/所有者（控制面/Guest/应用/管理器）和限制条件。
- conversation 按实际角色保留对话和工具观察；从与当前对象对应的工具结果保留已核实条件，不把助手过去的回答或工具调用参数当成已核实事实，不添加对话中没有的条件。
- search_queries 为 1 到 2 条；简单问题只写 1 条，确有多个独立方面时才拆分。
- 至少一条写成完整书面问句：补全省略的主语和对象，点明这是 GPU 云平台场景（实例、镜像、计费、存储、网络、控制台等），用平台文档里会出现的说法，不要只写关键词。
- 错误码、端口号、型号、命令名、金额等精确写法必须原样保留，不得改写、翻译或省略。`

type knowledgeQueryPlan struct {
	AnswerQuestion string   `json:"answer_question"`
	SearchQueries  []string `json:"search_queries"`
}

type knowledgeQueryPlanInput struct {
	Conversation  []openai.ChatCompletionMessage `json:"conversation"`
	Current       string                         `json:"current_question"`
	ProposedQuery string                         `json:"agent_proposed_query"`
}

// planKnowledgeQuery adds one bounded contextualization call for each turn's first
// knowledge search. It is not a second answer agent: the call can only return a
// standalone answer target plus retrieval queries.
// Any transport, parse, or validation failure falls back to the Agent-proposed
// query, so retrieval availability never depends on the planner.
func (e *Engine) planKnowledgeQuery(ctx context.Context, proposed string) knowledgeQueryPlan {
	fallback := fallbackKnowledgeQueryPlan(proposed)
	if e == nil || e.llmClient == nil || strings.TrimSpace(proposed) == "" || !e.turnContextViewReady {
		return fallback
	}
	input := knowledgeQueryPlanInput{
		Conversation:  e.knowledgeQueryConversation(),
		Current:       strings.TrimSpace(e.turnContextViewThisTurn.CurrentQuestion),
		ProposedQuery: strings.TrimSpace(proposed),
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fallback
	}
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: knowledgeQueryPlannerPrompt},
			{Role: openai.ChatMessageRoleUser, Content: string(payload)},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
		// Rewriting is a deterministic transformation, not a creative answer.
		Temperature: &knowledgeQueryPlannerTemperature,
	})
	if err != nil || resp == nil {
		return fallback
	}
	e.emitTokenUsage(resp.Usage)
	if resp.OutputIncomplete() {
		// A length-stopped (or otherwise non-normal) response can still parse as
		// valid JSON — a truncated generation is not reliably invalid JSON, it can
		// be cut off right after the closing brace — so this cannot rely on
		// parseFirstJSONObject failing below. Fall back to the Agent's own query
		// rather than risk a rewrite built on a cut-off answer_question. Usage is
		// still emitted above: the call was paid for either way.
		return fallback
	}

	var planned knowledgeQueryPlan
	if !parseFirstJSONObject(resp.Content, &planned) {
		return fallback
	}
	// Validated one slot short so the Agent's own query cannot be pushed out by a
	// full-length reply. The total stays at maxKnowledgePlanQueries, so this costs
	// no extra retrieval against maxRetrievalQueriesPerTurn.
	plan := validateKnowledgeQueryPlan(planned, fallback, maxKnowledgePlanQueries-1)
	return withAgentQueryAnchor(plan, strings.TrimSpace(proposed))
}

// knowledgeQueryConversation uses the same role-aware history entrance as the
// central agent. Serializing ConversationPair directly duplicated the prose
// endpoints around a nested Transcript, and omitted every observation made in
// the current turn. A query must not lose a condition just because the tool read
// happened this turn or the assistant did not repeat it in its final answer.
//
// The planner gets data, not the central agent's policy or another fact cache.
// Its live tool traffic crosses the existing canonical redaction, bounding and
// pairing boundary before being included; the pending SearchKnowledge call has
// no result yet and is dropped by that projector. This does not capture, persist
// or mutate the active transcript.
func (e *Engine) knowledgeQueryConversation() []openai.ChatCompletionMessage {
	messages := e.messages
	if start := currentTurnStart(messages); start >= 0 {
		live := messages[start:]
		current := []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: canonicalConversationText(openai.ChatMessageRoleUser, live[0].Content),
		}}
		if !oversizedRawTurn(live) {
			if transcript := buildTranscriptV1(live); transcript != nil {
				current = ProjectTranscript(transcript)
			}
		}
		messages = append(append([]openai.ChatCompletionMessage(nil), messages[:start]...), current...)
	}
	assembled := messagesFromAgentContext(messages, e.turnContextViewThisTurn, e.turnContextViewReady)
	conversation := make([]openai.ChatCompletionMessage, 0, len(assembled))
	for _, message := range assembled {
		if message.Role != openai.ChatMessageRoleSystem {
			conversation = append(conversation, message)
		}
	}
	return trimAssembledRequest(conversation, maxAssembledRequestRunes)
}

// withAgentQueryAnchor retains the model's precise query alongside the rewrite.
// It is appended last so the bounded retrieval budget prioritizes the standalone
// form; weak queries contribute no evidence rather than lowering other results.
func withAgentQueryAnchor(plan knowledgeQueryPlan, proposed string) knowledgeQueryPlan {
	if proposed == "" || len(plan.SearchQueries) >= maxKnowledgePlanQueries {
		return plan
	}
	key := strings.ToLower(proposed)
	for _, existing := range plan.SearchQueries {
		if strings.ToLower(strings.TrimSpace(existing)) == key {
			return plan
		}
	}
	plan.SearchQueries = append(append([]string(nil), plan.SearchQueries...), proposed)
	return plan
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
