package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
)

func readObservationOf(t *testing.T, e *Engine, result capability.ReadResult) (string, ReadCapabilityObservation) {
	t.Helper()
	raw := e.buildReadObservation("SomeAction", "some_capability", result, false, func(StepEvent) {})
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(raw), &observation))
	return raw, observation
}

// A catalog is tool evidence, not a server-authored answer. This regression
// closes the original duplicate-list shape: the Agent can curate the list, and
// the gateway never appends the uncurated source text below its Markdown.
func TestBrowseListingIsEvidenceOnly(t *testing.T) {
	e := &Engine{}
	listing := capability.ReadHandled("社区镜像：\n名称=最强AI数字人InfiniteTalk, 部署次数=16969")
	listing.Envelope = &envelope.Envelope{Kind: envelope.KindImageList}

	raw, observation := readObservationOf(t, e, listing)
	assert.NotContains(t, raw, "render_ref")
	assert.NotContains(t, raw, "render_contract")
	assert.NotContains(t, raw, "{{READ_OBSERVATION_")
	assert.Equal(t, platform.ReadStatusHandled, observation.Status)
	assert.NotNil(t, observation.Envelope)
	require.Len(t, e.platformReadEvidenceThisTurn, 1)

	final := e.finalizeResponse(context.Background(), "推荐一个数字人镜像", "推荐 InfiniteTalk。")
	assert.Equal(t, "推荐 InfiniteTalk。", final)
	assert.NotContains(t, final, "部署次数=16969")
}
