package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
)

// A read capability's rendered Reply reaches the model through exactly one of
// two channels, and which one is decided by whether the capability built an
// envelope — not by its Presentation. That is easy to get backwards while
// auditing renderers, and getting it backwards costs real time: three of the
// five browse capabilities (gpu_specs, image_list, zone_catalog) build an
// envelope, so their Handled reply text is consumed by NOBODY. Editing it
// changes nothing the user or the model ever sees.
//
// The other two (model_repository, image_tag_catalog) build no envelope, and
// contextEnvelopeForPlainDirectReply wraps their reply into a synthetic
// contextual_direct_reply envelope, so their reply text IS load-bearing.
//
// Nothing is lost either way — this pins which is which so the next person
// auditing a browse renderer knows before editing it, not after.

const browseRenderedText = "机型=4090, 性能=83, 显存=24GB, 状态=Normal, 最大卡数=8"

func observationFor(t *testing.T, result capability.ReadResult) (string, ReadCapabilityObservation) {
	t.Helper()
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	raw := eng.buildReadObservation("SomeAction", "some_capability", result, true, noopStep)
	var obs ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(raw), &obs))
	return raw, obs
}

func TestBrowseReplyIsDroppedWhenTheCapabilityBuiltItsOwnEnvelope(t *testing.T) {
	env := envelope.Envelope{Kind: envelope.KindGPUSpecsQuery, Subjects: []envelope.Subject{{ID: "gpu:4090", Name: "4090"}}}
	raw, obs := observationFor(t, capability.ReadResult{
		Status:       platform.ReadStatusHandled,
		Reply:        browseRenderedText,
		Presentation: capability.ReadPresentationBrowse,
		Envelope:     &env,
	})

	assert.NotContains(t, raw, browseRenderedText,
		"a browse renderer that has an envelope writes text nothing consumes")
	assert.Empty(t, obs.RenderRef, "browse is a menu the Agent curates; the server never staples it in front")
	assert.NotNil(t, obs.Envelope, "the exact ids and counts still travel — the Agent writes them itself")
}

func TestBrowseReplySurvivesWhenTheCapabilityHasNoEnvelope(t *testing.T) {
	raw, obs := observationFor(t, capability.ReadResult{
		Status:       platform.ReadStatusHandled,
		Reply:        browseRenderedText,
		Presentation: capability.ReadPresentationBrowse,
		ToolAction:   "DescribeModelRepositoryModels",
	})

	require.NotNil(t, obs.Envelope, "a Handled reply with no envelope is wrapped, never dropped")
	assert.Equal(t, envelope.KindContextualDirectReply, obs.Envelope.Kind)
	assert.Contains(t, raw, browseRenderedText,
		"model_repository / image_tag_catalog build no envelope, so their render IS the evidence")
}

// The contrast that makes the rule readable: the same result classified exact
// reaches the USER verbatim, through a render_ref the Agent may insert.
func TestExactReplyReachesTheUserThroughRenderRef(t *testing.T) {
	env := envelope.Envelope{Kind: envelope.KindGPUSpecsQuery}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	raw := eng.buildReadObservation("SomeAction", "some_capability", capability.ReadResult{
		Status:       platform.ReadStatusHandled,
		Reply:        browseRenderedText,
		Presentation: capability.ReadPresentationExact,
		Envelope:     &env,
	}, true, noopStep)

	assert.Contains(t, raw, "render_ref")
	require.Len(t, eng.readResponseEvidenceThisTurn, 1)
	assert.True(t, strings.Contains(eng.readResponseEvidenceThisTurn[0].Reply, "4090"))
}
