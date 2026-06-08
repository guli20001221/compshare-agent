package engine

import openai "github.com/sashabaranov/go-openai"

// knowledgeQAAgentLoopOn gates the terminal-knowledge_qa → agent-loop migration.
// Default false => byte-identical: a knowledge_qa turn keeps the deterministic
// terminal-RAG route (tryStage2BRetrieval). When on (AND the agentic SearchKnowledge
// tool is enabled AND a retriever is wired), a knowledge_qa turn instead SKIPS the
// terminal route and enters the shared ReAct loop with a forced SearchKnowledge
// first hop, so platform/external knowledge flows through the same agent loop as
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

// knowledgeQAAgentLoopSearchNote is the ephemeral system note injected before the
// last user message when forcing the SearchKnowledge first hop on a model that does
// NOT support object tool_choice (so the precise object force is unavailable). It
// mirrors monitorRecallRequiredToolNote: a strong instruction to call SearchKnowledge
// first, paired with tool_choice="required" when that is supported, else advisory.
const knowledgeQAAgentLoopSearchNote = "本轮为知识问答：请先调用 SearchKnowledge 工具检索知识库，再基于检索到的条目作答；不要在未检索的情况下直接回答。"

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
