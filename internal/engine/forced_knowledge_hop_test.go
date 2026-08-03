package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withForcedKnowledgeHop flips the experiment arm for one test and restores it,
// so an ordering change can never leave the flag on for the rest of the package.
func withForcedKnowledgeHop(t *testing.T, enabled bool) {
	t.Helper()
	previous := forcedKnowledgeHopEnabled
	SetForcedKnowledgeHopEnabled(enabled)
	t.Cleanup(func() { SetForcedKnowledgeHopEnabled(previous) })
}

// complaintTurnEngine is a first-turn engine: no prior conversation, and a model
// that answers directly without ever calling a tool. That is precisely the
// production shape the arm targets — 好贵 measured 0/5 searches while the same
// topic phrased as a question measured 5/5.
func complaintTurnEngine(t *testing.T, responses ...llm.ChatResponse) (*Engine, *mockLLM, *scriptedKnowledgeRetriever) {
	t.Helper()
	client := &mockLLM{responses: responses}
	retriever := &scriptedKnowledgeRetriever{results: twoHitResults()}
	eng := NewWithDeps(client, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.InitWithContext("用户当前没有实例。")
	return eng, client, retriever
}

func directAnswer(text string) llm.ChatResponse { return llm.ChatResponse{Content: text} }

// TestForcedKnowledgeHopIsInertByDefault is the gate that matters most: the arm
// ships off, so a turn must be indistinguishable from one taken before this code
// existed — no retrieval, no injected messages, no per-turn knowledge state.
func TestForcedKnowledgeHopIsInertByDefault(t *testing.T) {
	require.False(t, ForcedKnowledgeHopEnabled(), "the Go-package default must stay off")

	eng, client, retriever := complaintTurnEngine(t, directAnswer("这个价格确实不低……"))
	_, err := eng.Chat(context.Background(), "Coding Plan 套餐好贵", noopStep)
	require.NoError(t, err)

	assert.Empty(t, retriever.calls, "the default arm must not retrieve on its own")
	assert.False(t, eng.knowledgeQAAgentLoopThisTurn)
	require.Len(t, client.calls, 1)
	assert.False(t, requestCarriesForcedHop(client.calls[0]),
		"no synthetic tool observation may reach the model with the arm off")
}

// TestForcedKnowledgeHopRetrievesOnAComplaintTheAgentWouldNotSearch is the
// point of the arm. The model here never calls a tool — it answers the complaint
// at face value, exactly as production does on 好贵 — and evidence must still
// reach it, in its FIRST call, because after that call there is no second chance:
// a no-tool response ends the turn.
func TestForcedKnowledgeHopRetrievesOnAComplaintTheAgentWouldNotSearch(t *testing.T) {
	withForcedKnowledgeHop(t, true)

	eng, client, retriever := complaintTurnEngine(t,
		// First response is consumed by the query planner, which the arm also
		// enables on first turns; the complaint is restated as a searchable
		// question before it reaches the retriever.
		directAnswer(`{"answer_question":"Coding Plan 套餐怎么收费","search_queries":["Coding Plan 套餐价格档位"]}`),
		directAnswer("Coding Plan 有以下档位……"),
	)
	_, err := eng.Chat(context.Background(), "Coding Plan 套餐好贵", noopStep)
	require.NoError(t, err)

	require.Len(t, retriever.calls, 2, "the engine must retrieve without being asked: the expansion, then the raw form")
	assert.Equal(t, "Coding Plan 套餐价格档位", retriever.calls[0].question,
		"the arm's whole value is that the complaint is rewritten before retrieval")
	assert.Equal(t, "Coding Plan 套餐好贵", retriever.calls[1].question,
		"the user's own words are retained as the last query, so expansion can only add recall")
	assert.True(t, eng.knowledgeQAAgentLoopThisTurn,
		"the citation finalizer must run on a turn where the model can see chunk ids")
	assert.NotEmpty(t, eng.searchKnowledgeLedgerThisTurn.Items, "evidence must land in the per-turn ledger")

	require.Len(t, client.calls, 2, "one planner call, then the Agent's first call")
	assert.True(t, requestCarriesForcedHop(client.calls[1]),
		"evidence must be in front of the Agent on its first call, since that call can end the turn")
}

// TestForcedKnowledgeHopBudgetsLikeAnAgentSearch guards the arm against paying
// for itself twice. The forced hop is a real SearchKnowledge call, so it must
// consume the per-turn call budget like one, and an Agent that then re-issues
// the identical query must get the reused observation rather than a second
// retrieval — otherwise the arm silently halves the multi-hop budget on every
// turn where the model would have searched anyway.
func TestForcedKnowledgeHopBudgetsLikeAnAgentSearch(t *testing.T) {
	withForcedKnowledgeHop(t, true)

	eng, _, retriever := complaintTurnEngine(t,
		directAnswer(`{"answer_question":"Coding Plan 套餐怎么收费","search_queries":["Coding Plan 套餐价格档位"]}`),
		llm.ChatResponse{ToolCalls: []openai.ToolCall{toolCall("again", "SearchKnowledge", `{"query":"Coding Plan 套餐好贵"}`)}},
		directAnswer("Coding Plan 有以下档位……"),
	)
	_, err := eng.Chat(context.Background(), "Coding Plan 套餐好贵", noopStep)
	require.NoError(t, err)

	assert.Equal(t, 1, eng.searchKnowledgeCallsThisTurn, "the forced hop costs exactly one call unit")
	// Two retrievals is the forced hop's own fan-out (expansion + the retained
	// raw form). What this guards is that the Agent's identical re-issue adds
	// NOTHING on top of it.
	assert.Len(t, retriever.calls, 2,
		"re-issuing the forced query must reuse the observation, not spend another retrieval")
}

// TestForcedKnowledgeHopSkipsWhenTheModelCannotSearch covers the two states
// where an injected observation would be a lie about the runtime: no retriever
// wired (the observation would read as "we looked and found nothing" when we
// never looked) and an empty question. Both must leave the turn untouched.
func TestForcedKnowledgeHopSkipsWhenTheModelCannotSearch(t *testing.T) {
	withForcedKnowledgeHop(t, true)

	t.Run("no retriever wired", func(t *testing.T) {
		client := &mockLLM{responses: []llm.ChatResponse{directAnswer("……")}}
		eng := NewWithDeps(client, &mockExecutor{}, nil)
		eng.InitWithContext("用户当前没有实例。")

		_, err := eng.Chat(context.Background(), "Coding Plan 套餐好贵", noopStep)
		require.NoError(t, err)

		assert.False(t, eng.knowledgeQAAgentLoopThisTurn)
		require.Len(t, client.calls, 1)
		assert.False(t, requestCarriesForcedHop(client.calls[0]))
	})

	t.Run("blank question", func(t *testing.T) {
		eng, _, retriever := complaintTurnEngine(t, directAnswer("……"))
		eng.runForcedKnowledgeHop(context.Background(), "   ", noopStep)

		assert.Empty(t, retriever.calls)
		assert.Empty(t, eng.messages[len(eng.messages)-1].ToolCalls)
	})
}

// TestForcedKnowledgeHopKeepsTheTranscriptWellFormed pins the API invariant the
// injection could break: every tool message must answer a tool_call that
// precedes it with a matching id. A synthetic observation with no assistant
// tool_call in front of it is a 400 from the provider, on every turn, for every
// user — the arm would not fail an eval, it would fail the product.
func TestForcedKnowledgeHopKeepsTheTranscriptWellFormed(t *testing.T) {
	withForcedKnowledgeHop(t, true)

	eng, client, _ := complaintTurnEngine(t,
		directAnswer(`{"answer_question":"Coding Plan 套餐怎么收费","search_queries":["Coding Plan 套餐价格档位"]}`),
		directAnswer("Coding Plan 有以下档位……"),
	)
	_, err := eng.Chat(context.Background(), "Coding Plan 套餐好贵", noopStep)
	require.NoError(t, err)

	require.Len(t, client.calls, 2)
	assertToolMessagesAreAnswered(t, client.calls[1].Messages)
}

// TestForcedKnowledgeHopObservationDeclaresItsProvenance covers the
// contamination guard. The Agent is handed evidence it did not request; on an
// action turn ("确认关机") an off-topic hit must not read as a hint about what the
// turn is about. The observation therefore has to say who searched and that the
// result is ignorable — and it must stay valid JSON while doing so.
func TestForcedKnowledgeHopObservationDeclaresItsProvenance(t *testing.T) {
	annotated := annotateForcedKnowledgeHop(
		searchKnowledgeResultJSON(knowledge.EvidenceLedger{Query: "关机后还收什么"}, true, ""))

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(annotated), &payload),
		"the observation must remain parseable JSON")
	assert.Equal(t, true, payload["auto_retrieved"])
	note, _ := payload["note"].(string)
	assert.Contains(t, note, "系统在你决策前自动执行")
	assert.Contains(t, note, "不要因此改变话题或改变要执行的操作")
}

// TestAnnotateForcedKnowledgeHopShipsUnparseableResultsUnchanged: the note is a
// nicety, the evidence is the payload. If a result ever fails to decode, the arm
// must degrade to "evidence without the note", never to "no evidence".
func TestAnnotateForcedKnowledgeHopShipsUnparseableResultsUnchanged(t *testing.T) {
	assert.Equal(t, "not json", annotateForcedKnowledgeHop("not json"))
}

// TestEmptyForcedHopSaysItFailedRatherThanInvitingARetry is the safety net for
// the residual failures the query expansion does not catch. An empty ledger and
// a full one used to carry the same note, whose only nod to failure was the
// optional "需要更准确的证据时…再检索一次" — and that invitation is not taken: of the
// ten turns that ended with an empty ledger, nine had searched exactly once.
//
// The empty note has three jobs, and dropping any one of them makes it harmful
// rather than merely useless: it must ask for another search, it must not let
// the agent report the absence to the user (an empty retrieval here is a query
// failure far more often than a corpus gap), and it must keep the contamination
// guard so a 关机 / 确认 turn is not pulled into a documentation detour.
func TestEmptyForcedHopSaysItFailedRatherThanInvitingARetry(t *testing.T) {
	noteFor := func(t *testing.T, result string) string {
		t.Helper()
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(annotateForcedKnowledgeHop(result)), &payload))
		note, _ := payload["note"].(string)
		require.NotEmpty(t, note)
		return note
	}

	t.Run("empty evidence reports the search failed", func(t *testing.T) {
		note := noteFor(t, searchKnowledgeResultJSON(
			knowledge.EvidenceLedger{Query: "扩容之后要重启吗"}, true, ""))

		assert.Contains(t, note, "结果为空", "the agent must be told the search came back with nothing")
		assert.Contains(t, note, "再检索一次", "and told to search again")
		assert.Contains(t, note, "不能当作知识库里没有",
			"an empty retrieval is a query failure far more often than a corpus gap")
		assert.Contains(t, note, "不要因此改变话题或改变要执行的操作",
			"the contamination guard applies to an action turn whether or not evidence came back")
	})

	t.Run("evidence that survived keeps the original note", func(t *testing.T) {
		ledger := knowledge.EvidenceLedger{
			Query: "扩容之后要重启吗",
			Items: []knowledge.EvidenceItem{{ChunkID: "chunk-1", Title: "扩容", Summary: "evidence"}},
		}
		note := noteFor(t, searchKnowledgeResultJSON(ledger, false, ""))

		assert.Equal(t, forcedKnowledgeHopNote, note)
		assert.NotContains(t, note, "结果为空",
			"a turn holding evidence must not be told the search failed")
	})

	t.Run("the failure signal reaches the model", func(t *testing.T) {
		withForcedKnowledgeHop(t, true)

		// No scripted results: the retriever returns an empty hit set, which is
		// the shape the floor produces when it drops every hit.
		client := &mockLLM{responses: []llm.ChatResponse{
			directAnswer(`{"answer_question":"扩容之后要重启吗","search_queries":["云盘扩容是否需要重启实例"]}`),
			directAnswer("扩容后是否需要重启……"),
		}}
		eng := NewWithDeps(client, &mockExecutor{}, nil)
		eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{})
		eng.InitWithContext("用户当前没有实例。")

		_, err := eng.Chat(context.Background(), "扩容之后要重启吗", noopStep)
		require.NoError(t, err)

		require.Len(t, client.calls, 2, "one planner call, then the Agent's first call")
		var observation string
		for _, msg := range client.calls[1].Messages {
			if msg.Role == openai.ChatMessageRoleTool && msg.ToolCallID == forcedKnowledgeHopCallID {
				observation = msg.Content
			}
		}
		require.NotEmpty(t, observation, "the empty hop must still be observable to the Agent")
		assert.Contains(t, observation, "结果为空",
			"the note has to survive the annotate/marshal round trip into the request")

		result, ok := tools.ParseAgentToolResult(observation)
		require.True(t, ok, "a synthetic SearchKnowledge result must use the same contract as an Agent-issued tool call")
		assert.Equal(t, tools.AgentToolStatusFailed, result.Status)
		assert.Equal(t, "NO_CITABLE_EVIDENCE", result.Error.Code)
		assert.Equal(t, tools.AgentToolNextAnswerWithLimits, result.NextStep)
		data, ok := result.Data.(map[string]any)
		require.True(t, ok)
		assert.Contains(t, data["note"], "再检索一次")
	})
}

// TestFirstTurnRequeryIsGatedOnTheForcedHopArm pins the second half of the
// change. planKnowledgeQuery skipped first turns because there were no
// references to resolve; the arm needs it there for a different job (normalising
// a complaint into a query). Off the arm, a first turn must still spend no LLM
// call — otherwise every unrelated turn pays for a rewrite nothing asked for.
func TestFirstTurnRequeryIsGatedOnTheForcedHopArm(t *testing.T) {
	newFirstTurnEngine := func() (*Engine, *mockLLM) {
		client := &mockLLM{responses: []llm.ChatResponse{
			directAnswer(`{"answer_question":"Coding Plan 套餐怎么收费","search_queries":["Coding Plan 套餐价格档位"]}`),
		}}
		eng := NewWithDeps(client, &mockExecutor{}, nil)
		eng.turnContextViewThisTurn = TurnContextView{CurrentQuestion: "Coding Plan 套餐好贵"}
		eng.turnContextViewReady = true
		return eng, client
	}

	t.Run("off", func(t *testing.T) {
		eng, client := newFirstTurnEngine()
		plan := eng.planKnowledgeQuery(context.Background(), "Coding Plan 套餐好贵")

		assert.Empty(t, client.calls, "a first turn with nothing to resolve must not pay for a planner call")
		assert.Equal(t, []string{"Coding Plan 套餐好贵"}, plan.SearchQueries)
	})

	t.Run("on", func(t *testing.T) {
		withForcedKnowledgeHop(t, true)
		eng, client := newFirstTurnEngine()
		plan := eng.planKnowledgeQuery(context.Background(), "Coding Plan 套餐好贵")

		require.Len(t, client.calls, 1, "the arm searches regardless, so the rewrite is worth its call")
		assert.Equal(t, "Coding Plan 套餐怎么收费", plan.AnswerQuestion)
		assert.Equal(t, []string{"Coding Plan 套餐价格档位"}, plan.SearchQueries)
	})
}

// TestForcedHopQueryIsExpandedNotJustDereferenced pins the fix for the failure
// this arm turned out to have. The forced hop searches on words the user never
// aimed at retrieval, and the default planner — whose job is de-referencing —
// correctly finds nothing to resolve in an already-grammatical question and
// echoes it back. Measured on real traffic: the raw form retrieved hits that the
// relevance floor then dropped in full, and 9 of 10 empty-evidence turns never
// searched again, so the turn answered with no evidence at all. Re-running those
// questions with expanded queries reached usable evidence from the SAME index.
//
// So the forced hop must be planned by the EXPANDER, and an Agent-issued search
// must not be — the Agent writes its own retrieval queries and measurement says
// it writes better ones than either the raw words or a restatement of them.
func TestForcedHopQueryIsExpandedNotJustDereferenced(t *testing.T) {
	newTurn := func(reply string) (*Engine, *mockLLM) {
		client := &mockLLM{responses: []llm.ChatResponse{directAnswer(reply)}}
		eng := NewWithDeps(client, &mockExecutor{}, nil)
		eng.turnContextViewThisTurn = TurnContextView{CurrentQuestion: "配置创建后是否可以修改？"}
		eng.turnContextViewReady = true
		return eng, client
	}
	systemPromptOf := func(t *testing.T, req llm.ChatRequest) string {
		t.Helper()
		require.NotEmpty(t, req.Messages)
		return req.Messages[0].Content
	}
	const expansions = `{"answer_question":"实例创建后能否修改配置",` +
		`"search_queries":["算力实例创建后修改 CPU 内存 GPU 配置 是否需要关机","GPU 实例变配 规格调整 条件"]}`

	t.Run("forced hop expands and keeps the user's words last", func(t *testing.T) {
		withForcedKnowledgeHop(t, true)
		eng, client := newTurn(expansions)
		eng.forcedHopSearchInFlight = true

		plan := eng.planKnowledgeQuery(context.Background(), "配置创建后是否可以修改？")

		require.Len(t, client.calls, 1)
		assert.Equal(t, knowledgeQueryExpanderPrompt, systemPromptOf(t, client.calls[0]),
			"the forced hop's query needs expansion, not de-referencing")
		assert.Equal(t, []string{
			"算力实例创建后修改 CPU 内存 GPU 配置 是否需要关机",
			"GPU 实例变配 规格调整 条件",
			"配置创建后是否可以修改？",
		}, plan.SearchQueries,
			"expansions first (they get the budget), the user's own words retained last")
		assert.Equal(t, "实例创建后能否修改配置", plan.AnswerQuestion,
			"answer_question stays the question the answer is verified against")
	})

	t.Run("an Agent-issued search keeps the de-referencing planner", func(t *testing.T) {
		withForcedKnowledgeHop(t, true)
		eng, client := newTurn(expansions)
		// Not in flight: this is the Agent asking, mid-turn.
		eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{User: "上一轮", Assistant: "上一轮回答"}}

		plan := eng.planKnowledgeQuery(context.Background(), "实例变配要关机吗")

		require.Len(t, client.calls, 1)
		assert.Equal(t, knowledgeQueryPlannerPrompt, systemPromptOf(t, client.calls[0]),
			"expansion must not leak onto queries the Agent wrote itself")
		assert.NotContains(t, plan.SearchQueries, "实例变配要关机吗",
			"only the forced hop pins the caller's raw text into the query set")
	})

	t.Run("the user's words survive a maximally wide expansion", func(t *testing.T) {
		withForcedKnowledgeHop(t, true)
		eng, _ := newTurn(`{"answer_question":"实例创建后能否修改配置",` +
			`"search_queries":["扩写一","扩写二","扩写三","扩写四","扩写五"]}`)
		eng.forcedHopSearchInFlight = true

		plan := eng.planKnowledgeQuery(context.Background(), "配置创建后是否可以修改？")

		require.Len(t, plan.SearchQueries, maxForcedHopPlanQueries)
		assert.Equal(t, "配置创建后是否可以修改？", plan.SearchQueries[len(plan.SearchQueries)-1],
			"a reply that fills every slot must not push the raw form out — that is the no-recall-loss guarantee")
	})
}

// requestCarriesForcedHop reports whether the engine's injected SearchKnowledge
// observation reached the model in this request.
func requestCarriesForcedHop(req llm.ChatRequest) bool {
	for _, msg := range req.Messages {
		if msg.Role == openai.ChatMessageRoleTool && msg.ToolCallID == forcedKnowledgeHopCallID {
			return true
		}
	}
	return false
}

// assertToolMessagesAreAnswered checks the provider-level invariant: each tool
// message names a tool_call id emitted by an assistant message before it.
func assertToolMessagesAreAnswered(t *testing.T, msgs []openai.ChatCompletionMessage) {
	t.Helper()
	pending := map[string]bool{}
	for i, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			pending[tc.ID] = true
		}
		if msg.Role != openai.ChatMessageRoleTool {
			continue
		}
		require.Truef(t, pending[msg.ToolCallID],
			"message %d is a tool result for %q with no preceding tool_call; transcript:\n%s",
			i, msg.ToolCallID, describeTranscript(msgs))
	}
}

func describeTranscript(msgs []openai.ChatCompletionMessage) string {
	var b strings.Builder
	for i, msg := range msgs {
		b.WriteString(msg.Role)
		if msg.ToolCallID != "" {
			b.WriteString(" (answers " + msg.ToolCallID + ")")
		}
		for _, tc := range msg.ToolCalls {
			b.WriteString(" [calls " + tc.Function.Name + "/" + tc.ID + "]")
		}
		if i < len(msgs)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
