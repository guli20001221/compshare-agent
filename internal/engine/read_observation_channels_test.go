package engine

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
)

// A handled read has one model-facing path: its evidence envelope. The rendered
// Reply is used only to build a synthetic evidence envelope when the capability
// did not make one. There is no second server-rendered answer path.
const agentRenderedText = "机型=4090, 性能=83, 显存=24GB, 状态=Normal, 最大卡数=8"

func observationFor(t *testing.T, result capability.ReadResult) (string, ReadCapabilityObservation, *Engine) {
	t.Helper()
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	raw := eng.buildReadObservation("SomeAction", "some_capability", result, true, noopStep)
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(raw), &observation))
	return raw, observation, eng
}

func TestReplyWithEnvelopeIsNotDuplicatedIntoTheObservation(t *testing.T) {
	env := envelope.Envelope{Kind: envelope.KindGPUSpecsQuery, Subjects: []envelope.Subject{{ID: "gpu:4090", Name: "4090"}}}
	raw, observation, eng := observationFor(t, capability.ReadResult{
		Status:   platform.ReadStatusHandled,
		Reply:    agentRenderedText,
		Envelope: &env,
	})

	assert.NotContains(t, raw, agentRenderedText,
		"the model reads structured evidence, not a second prewritten answer")
	assert.NotNil(t, observation.Envelope)
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)
	assert.Empty(t, eng.sensitiveRepliesThisTurn)
}

func TestReplyWithoutEnvelopeIsWrappedAsEvidence(t *testing.T) {
	raw, observation, eng := observationFor(t, capability.ReadResult{
		Status:     platform.ReadStatusHandled,
		Reply:      agentRenderedText,
		ToolAction: "DescribeModelRepositoryModels",
	})

	require.NotNil(t, observation.Envelope, "a handled reply with no envelope is wrapped, never dropped")
	assert.Equal(t, envelope.KindContextualDirectReply, observation.Envelope.Kind)
	assert.Contains(t, raw, agentRenderedText)
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)
}

func TestHandledReadNeverCreatesARenderReference(t *testing.T) {
	env := envelope.Envelope{Kind: envelope.KindGPUSpecsQuery}
	raw, _, eng := observationFor(t, capability.ReadResult{
		Status:   platform.ReadStatusHandled,
		Reply:    agentRenderedText,
		Envelope: &env,
	})

	assert.NotContains(t, raw, "render_ref")
	assert.NotContains(t, raw, "render_contract")
	assert.NotContains(t, raw, "{{READ_OBSERVATION_")
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)
}
