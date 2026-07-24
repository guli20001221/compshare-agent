package llm

import (
	"encoding/json"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPinnedZeroTemperatureSurvivesSerialization guards the whole point of
// ChatRequest.Temperature. go-openai tags the upstream field `omitempty`, so
// assigning a literal 0 drops "temperature" from the request body and the
// provider silently applies its own default — a caller asking for deterministic
// sampling would get the most sampled setting instead, with nothing to observe.
//
// The assertion is on the marshalled body, not on the Go struct, because the
// struct holds 0 either way; only the wire tells the two apart.
func TestPinnedZeroTemperatureSurvivesSerialization(t *testing.T) {
	body, err := json.Marshal(openai.ChatCompletionRequest{
		Model:       "deepseek-v4-flash",
		Temperature: wireTemperature(0),
	})
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(body), `"temperature"`),
		"a pinned temperature of 0 must reach the wire; got %s", body)
}

// TestUnpinnedTemperatureIsAbsentFromTheWire is the other half: callers that
// leave Temperature nil must be byte-identical to before this field existed.
func TestUnpinnedTemperatureIsAbsentFromTheWire(t *testing.T) {
	body, err := json.Marshal(openai.ChatCompletionRequest{Model: "deepseek-v4-flash"})
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(body), `"temperature"`),
		"an unpinned request must not send a temperature; got %s", body)
}

func TestWireTemperaturePassesThroughNonZeroValues(t *testing.T) {
	assert.Equal(t, float32(0.7), wireTemperature(0.7))
	assert.Equal(t, float32(1.5), wireTemperature(1.5))
}
