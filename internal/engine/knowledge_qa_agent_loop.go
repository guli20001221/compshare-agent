package engine

import openai "github.com/sashabaranov/go-openai"

// knowledgeQAAgentLoopSearchNote makes retrieval a context-dependent decision.
// A follow-up may be answered directly when the visible conversation already
// contains enough information. If retrieval is needed, the same agent that sees
// the conversation owns the standalone query. A first-turn question still gets a
// forced SearchKnowledge hop in engine.go because there is no prior answer to reuse.
const knowledgeQAAgentLoopSearchNote = "本轮已进入知识问答路径；请遵守基础提示中的知识来源与检索规则。"

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
