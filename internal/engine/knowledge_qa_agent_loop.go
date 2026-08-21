package engine

import openai "github.com/sashabaranov/go-openai"

// toolListContainsFunction reports whether a function tool with the given name is
// present in the built request tool list. It prevents forced tool_choice from
// naming an absent function. openai.Tool.Function may be nil.
func toolListContainsFunction(toolDefs []openai.Tool, name string) bool {
	for _, t := range toolDefs {
		if t.Function != nil && t.Function.Name == name {
			return true
		}
	}
	return false
}

// toolListWithoutFunction returns a copy without the named function. SearchKnowledge
// is withdrawn after its per-turn cap to prevent repeated empty searches.
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
