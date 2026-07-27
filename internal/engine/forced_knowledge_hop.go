package engine

import (
	"context"
	"encoding/json"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// Forced first-hop retrieval is the experiment arm for "the Agent's decision to
// search is the broken part, so stop asking it to make one".
//
// What the measurement says (2026-07-24, terra, N=5 per cell, real machine, via
// cmd/live_tool_probe_test.go):
//
//	Coding Plan 套餐好贵            searched 0/5
//	优云智算的 Coding Plan 套餐好贵  searched 0/5
//	Coding Plan 套餐怎么收费        searched 5/5
//	…为什么那么贵                   searched 5/5
//	你们的 GPU 实例好贵             searched 0/5
//	你们的 GPU 实例为什么那么贵     searched 5/5
//
// Same topic, same corpus, same tool schema, naming the platform or not — only
// the speech act differs. A complaint or a bare statement is answered at face
// value (empathy plus generic advice, no evidence); the interrogative form
// retrieves every time. That is backwards for the user: someone who says 好贵 is
// exactly the person who needs the real price table in front of them.
//
// It is NOT a retrieval failure. Over the same 15 coding-plan turns: never
// searched 7, searched → ground truth retrieved → kept → cited 7, ground truth
// missing from a search 1 (of 8 searches), relevance floor dropped everything 0,
// synthesis failure 0. Everything downstream of "did we search at all" works.
//
// Nor is it reachable from the prompt. Three separate edits aimed at it — topic
// enumeration, an abstract closed-form criterion, and a SearchKnowledge
// description split — all landed inside the ±10pp A/A noise floor (same config
// run three times gave 25/28/23 searches over the same 40 questions). And there
// is no ex-ante turn classifier left to hang a rule on: P6 deleted the intent
// router, and knowledgeQAAgentLoopThisTurn is set only where SearchKnowledge
// executes, which makes it a post-hoc marker rather than a signal.
//
// So the engine performs one retrieval itself, before the Agent's first model
// call of the turn, and injects the result as an ordinary tool observation. The
// Agent keeps every other decision: the turn policy already tells it that
// retrieval results are supplementary observations which cannot override
// context, so irrelevant evidence is ignorable, and it may still search again
// with a better query on its own budget.
//
// Deliberately NOT object tool_choice. Forcing through the API 400s on any model
// whose supportsObjectToolChoice capability is false, and it would still leave
// the model choosing the query — which is the decision being taken away.
//
// Scope is every turn, not only the first. First turns are 44% of production
// traffic (2917 user messages / 1293 sessions), and the statement-form failures
// seen in the sample — 对订单有疑惑 / 还是产生reconnect这个现象 / 服务器开机时显示当前
// 资源不足 — arrive mid-conversation.
//
// Default OFF and boot-only, like gapDrivenRetrievalEnabled: the Go-package
// default stays off so unit tests are unaffected, and turning it on is an
// eval-gated decision, not a code change.
var forcedKnowledgeHopEnabled bool

// SetForcedKnowledgeHopEnabled freezes the flag at boot. Never call it per turn:
// half a session retrieving on its own words and half not would make one
// conversation's evidence come from two different policies.
func SetForcedKnowledgeHopEnabled(enabled bool) {
	forcedKnowledgeHopEnabled = enabled
}

// ForcedKnowledgeHopEnabled reports the frozen setting.
func ForcedKnowledgeHopEnabled() bool {
	return forcedKnowledgeHopEnabled
}

// forcedKnowledgeHopCallID labels the synthetic tool call so a transcript never
// leaves the reader guessing which SearchKnowledge the Agent asked for and which
// one the engine took on its behalf.
const forcedKnowledgeHopCallID = "forced_knowledge_hop"

// forcedKnowledgeHopNote travels with the observation. The Agent is being handed
// evidence it did not ask for, so the observation has to say who searched and on
// what — otherwise an off-topic hit reads as a hint about what this turn is
// about, and a mutating turn ("关机", "确认") could be pulled into a documentation
// answer. Retrieval results are supplementary observations; that contract lives
// in the turn policy and this line only names the unusual provenance.
const forcedKnowledgeHopNote = "本次检索由系统在你决策前自动执行，检索词由用户本轮原话整理而来，不代表本轮一定需要文档证据。" +
	"与用户当前意图无关时忽略这些证据，不要因此改变话题或改变要执行的操作；需要更准确的证据时，用更具体的问题再检索一次。"

// forcedKnowledgeHopEmptyNote replaces the note above when the automatic search
// came back with nothing. The distinction matters because the two states read
// identically to the model otherwise — an empty ledger with a note that ends in
// "需要更准确的证据时…再检索一次" is an invitation, and the measurement says the
// invitation is not taken: of the ten turns that ended with an empty ledger,
// nine had searched exactly once.
//
// Two things it must not say. It must not say the knowledge base lacks the
// answer: an empty result here is a query failure far more often than a corpus
// gap — rewriting the same 24 failed questions into explicit queries reached
// usable evidence on 23 of them, and six turned out to be fully documented. And
// it must not order an unconditional re-search, because this observation also
// lands on turns that never needed documentation at all; a 关机 or 确认 turn
// would spend a retrieval on nothing. So the imperative is conditional on the
// turn actually needing platform documentation, and the escape stays first.
const forcedKnowledgeHopEmptyNote = "本次检索由系统在你决策前自动执行，检索词由用户本轮原话整理而来，结果为空。" +
	"本轮不需要平台文档证据时，忽略这条观察正常继续，不要因此改变话题或改变要执行的操作。" +
	"本轮需要平台文档证据时，这个空结果只说明这次的检索词没检到，不能当作知识库里没有、也不能据此告诉用户没有相关文档或资料；" +
	"请补全用户省略的主语和场景（实例、镜像、计费、存储、网络、控制台等），换成平台文档里会出现的说法，再检索一次。"

// runForcedKnowledgeHop retrieves once on the user's own words and appends the
// result to history as an ordinary assistant tool_call / tool result pair, so the
// Agent's first model call of the turn already has evidence in front of it.
//
// It returns without doing anything when the arm is off, when no retriever is
// wired, when there is no question text, or when SearchKnowledge is absent from
// this turn's tool window — injecting an observation for a capability the model
// cannot see would describe a tool that does not exist for it, and the follow-up
// hop the observation invites would be unavailable.
func (e *Engine) runForcedKnowledgeHop(ctx context.Context, userMsg string, onStep func(StepEvent)) {
	if e == nil || !forcedKnowledgeHopEnabled || e.knowledgeRetriever == nil {
		return
	}
	// The raw user message, not llmUserMsg: screenshot context is untrusted
	// reference text and is far longer than a retrieval query should be.
	query := truncateRunes(strings.TrimSpace(userMsg), maxKnowledgePlanQueryRunes)
	if query == "" {
		return
	}
	// Same two gates the real window is built from (engine.go), so this check sees
	// exactly the tool list the model will see. The in-instance lane only adds
	// DiagnoseInstanceInternals, but reading the window through a different set of
	// gates than the one that produces it is how the two silently drift apart.
	if !toolListContainsFunction(centralAgentToolWindow(e.mutatingToolsEnabled, e.instanceOps != nil), "SearchKnowledge") {
		return
	}

	args := map[string]any{"query": query}
	arguments, err := json.Marshal(args)
	if err != nil {
		// Marshal before retrieving: a search whose observation cannot be
		// appended has spent budget and shown the user a tool step for nothing.
		return
	}

	e.knowledgeQAAgentLoopThisTurn = true
	// Scoped to this one call: the query planner expands only the words the user
	// never aimed at retrieval. Cleared immediately so an Agent-issued search
	// later in the same turn is planned normally.
	e.forcedHopSearchInFlight = true
	result := annotateForcedKnowledgeHop(e.executeSearchKnowledge(ctx, args, onStep))
	e.forcedHopSearchInFlight = false

	// Register under the key executeTool would have computed for the same call,
	// so an Agent that re-issues this exact query gets the reused-observation
	// reply instead of spending a second retrieval on it. decodeToolArgsForProgress
	// is a plain unmarshal into map[string]any and toolProgressCallKey marshals it
	// back with sorted keys, so a model call carrying {"query":"<same text>"}
	// digests identically to this map.
	if e.toolResultsByCallThisTurn == nil {
		e.toolResultsByCallThisTurn = map[string]string{}
	}
	e.toolResultsByCallThisTurn[toolProgressCallKey("SearchKnowledge", args)] = result

	e.messages = append(e.messages,
		openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{
				ID:       forcedKnowledgeHopCallID,
				Type:     openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: string(arguments)},
			}},
		},
		openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			Content:    result,
			ToolCallID: forcedKnowledgeHopCallID,
		},
	)
}

// annotateForcedKnowledgeHop adds the provenance note to a SearchKnowledge
// observation this engine produced. It decodes and re-encodes rather than
// splicing strings so a malformed result can never be turned into invalid JSON;
// on any decode failure the observation ships unchanged, since evidence without
// the note is still better than no evidence.
func annotateForcedKnowledgeHop(result string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(result), &payload); err != nil || payload == nil {
		return result
	}
	payload["auto_retrieved"] = true
	// searchKnowledgeResultJSON sets "empty" only when the ledger has no items,
	// and omits the key otherwise, so its presence is the signal. Reading the key
	// rather than re-deriving emptiness keeps the two in step if the ledger shape
	// changes.
	if empty, _ := payload["empty"].(bool); empty {
		payload["note"] = forcedKnowledgeHopEmptyNote
	} else {
		payload["note"] = forcedKnowledgeHopNote
	}
	annotated, err := json.Marshal(payload)
	if err != nil {
		return result
	}
	return string(annotated)
}
