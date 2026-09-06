package observability

// AgentRunUsage is the SDK-reported aggregate for one delegated query, not a
// provider attempt or a cumulative session counter. Anthropic input tokens
// exclude the separately reported cache categories; retaining native fields
// avoids silently applying OpenAI accounting or third-party price assumptions.
// Nil means the SDK omitted a counter; a pointer to zero is an observed zero.
type AgentRunUsage struct {
	InputTokens              *int `json:"input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	NumTurns                 *int `json:"num_turns,omitempty"`
	DurationAPIMS            *int `json:"duration_api_ms,omitempty"`
}
