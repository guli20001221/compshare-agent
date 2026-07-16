package engine

import openai "github.com/sashabaranov/go-openai"

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
