package engine

import openai "github.com/sashabaranov/go-openai"

// knowledgeQAAgentLoopOn gates the terminal-knowledge_qa → agent-loop migration.
// Default false => byte-identical: a knowledge_qa turn keeps the deterministic
// terminal-RAG route (tryStage2BRetrieval). When on (AND the agentic SearchKnowledge
// tool is enabled AND a retriever is wired), a knowledge_qa turn instead SKIPS the
// terminal route and enters the shared ReAct loop. A first-turn question forces
// SearchKnowledge; a follow-up may reuse sufficient visible conversation or search,
// so platform/external knowledge flows through the same agent loop as
// every other turn — the lead's north star ("没有单独的 rag — rag 作为 tool 供 agent
// 在 loop 里调用").
//
// Deliberately SEPARATE from COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE (default-on, which
// only makes the tool AVAILABLE) and from COMPSHARE_RAG_GROUNDED_VALIDATOR
// (default-off, the route-independent cite/leak validator): this flag changes the
// knowledge_qa ROUTE. It stays default-off until a flag-on A/B eval proves the
// agent-loop answer matches the terminal route at the hard-gate bar (faithfulness
// 0-fab, 100% cite-or-refuse, retrieval-coverage, no mis-route). Flipping it on is a
// separate, eval-gated PR (the migration's Phase 3). Set once at boot from
// COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP (cmd); the Go-package default stays false so the
// engine/tools unit tests are unaffected. Rollback = COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP=0.
var knowledgeQAAgentLoopOn bool

// SetKnowledgeQAAgentLoopEnabled toggles the knowledge_qa agent-loop route.
// Boot-only (reversible by restart), mirroring tools.SetAgenticSearchKnowledgeEnabled
// and SetGroundedAnswerValidatorEnabled.
func SetKnowledgeQAAgentLoopEnabled(v bool) { knowledgeQAAgentLoopOn = v }

// KnowledgeQAAgentLoopEnabled reports whether the knowledge_qa agent-loop route is on.
func KnowledgeQAAgentLoopEnabled() bool { return knowledgeQAAgentLoopOn }

// knowledgeQAAgentLoopSearchNote makes retrieval a context-dependent decision.
// A follow-up may be answered directly when the visible conversation already
// contains enough information. If retrieval is needed, the same agent that sees
// the conversation owns the standalone query. A first-turn question still gets a
// forced SearchKnowledge hop in engine.go because there is no prior answer to reuse.
const knowledgeQAAgentLoopSearchNote = "本轮为知识问答：先阅读当前可见的完整对话。若已有上下文足以直接、准确回答当前问题，可以直接回答，不要为了形式重复检索；若信息不足、需要新事实或需要确认时效，请先把用户此刻真正要解决的问题组织成独立、完整、可检索且不依赖上文的 query，再调用 SearchKnowledge。必须消解指代并保留已有的产品、环境、目标和约束，不得添加对话中没有的事实。没有实际检索时，不得声称知识库未覆盖。"

// knowledgeQASearchCapNote is injected once the per-turn SearchKnowledge cap is hit
// and the tool is withdrawn (see maxSearchKnowledgeCallsPerTurn). It steers the model
// to answer honestly from what it already retrieved — or to plainly say there is no
// specific documentation — instead of fabricating, since the round-0 cited-contract
// gate no longer applies on these later rounds. Imperative + short per the flash
// directive-phrasing guidance.
const knowledgeQASearchCapNote = "已多次检索但未获得更多相关资料。请基于已检索到的信息如实作答；若确实没有相关专项资料，请直接说明\"暂无该主题的专项文档\"，并建议用户查阅优云控制台或官网帮助，不要编造不确定的细节。"

// toolListContainsFunction reports whether a function tool with the given name is
// present in the built request tool list. Used before forcing a tool via ToolChoice
// so an absent tool is never named (object tool_choice on a missing tool 400s with
// "no function named X in tools" — the 400 trap caught during the 2026-06-08 flash
// re-probe). openai.Tool.Function is a *FunctionDefinition; guard the nil.
func toolListContainsFunction(toolDefs []openai.Tool, name string) bool {
	for _, t := range toolDefs {
		if t.Function != nil && t.Function.Name == name {
			return true
		}
	}
	return false
}

// toolListWithoutFunction returns a copy of the tool list with the named function
// tool removed. Used to withdraw SearchKnowledge once the per-turn cap is hit so the
// model can no longer re-search a corpus-gap query into a token-budget overrun.
// Returns a new slice (never mutates the caller's) so the registry stays intact.
func toolListWithoutFunction(toolDefs []openai.Tool, name string) []openai.Tool {
	out := make([]openai.Tool, 0, len(toolDefs))
	for _, t := range toolDefs {
		if t.Function != nil && t.Function.Name == name {
			continue
		}
		out = append(out, t)
	}
	return out
}

// toolCallsContain reports whether the model's response carried a tool call for the
// named function. It detects a mandatory first-hop misfire and distinguishes a
// context-aware direct answer from an actual retrieval.
func toolCallsContain(calls []openai.ToolCall, name string) bool {
	for _, tc := range calls {
		if tc.Function.Name == name {
			return true
		}
	}
	return false
}
