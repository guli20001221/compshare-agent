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

func readObservationOf(t *testing.T, e *Engine, result capability.ReadResult) ReadCapabilityObservation {
	t.Helper()
	raw := e.buildReadObservation("SomeAction", "some_capability", result, false, func(StepEvent) {})
	var obs ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(raw), &obs))
	return obs
}

func handledRead(reply string) capability.ReadResult {
	r := capability.ReadHandled(reply)
	r.Presentation = capability.ReadPresentationExact
	r.Envelope = &envelope.Envelope{Kind: envelope.KindImageList}
	return r
}

// TestABrowseListingGetsNoRenderRef is the fix for a 1.4k-character raw catalog
// dump that opened an answer to "为我推荐一个做数字人的镜像".
//
// A render_ref is a promise the server will paste this exact text into the final
// answer, so precise ids/prices/stock cannot be paraphrased. For a catalog listing
// that promise is wrong twice over: the listing is a menu the model is supposed to
// curate, and the model then restated the same deploy counts and image ids in its
// own prose — so the user read every row twice. The envelope still travels either
// way, so nothing the model needs to be exact about is lost.
func TestABrowseListingGetsNoRenderRef(t *testing.T) {
	e := &Engine{}

	quotable := readObservationOf(t, e, handledRead("精确实例表"))
	require.NotEmpty(t, quotable.RenderRef,
		"premise: an ordinary factual read still gets its verbatim block")
	require.Len(t, e.readResponseEvidenceThisTurn, 1)

	listing := handledRead("社区镜像：\n名称=最强AI数字人InfiniteTalk, 部署次数=16969")
	listing.Presentation = capability.ReadPresentationBrowse
	obs := readObservationOf(t, e, listing)

	assert.Empty(t, obs.RenderRef, "a menu must not be stapled in front of the curated answer")
	assert.Empty(t, obs.RenderContract)
	assert.Len(t, e.readResponseEvidenceThisTurn, 1,
		"and it must not enter the substitution table at all")
	assert.NotNil(t, obs.Envelope,
		"the exact ids and deploy counts still reach the model — it writes them itself")
	assert.Equal(t, platform.ReadStatusHandled, obs.Status)
}

// TestSubstitutionCannotReviveASuppressedListing closes the loop at the gateway:
// even if a model emits a placeholder that looks like one, there is no evidence row
// to substitute, so the raw listing cannot reappear in the final text.
func TestSubstitutionCannotReviveASuppressedListing(t *testing.T) {
	e := &Engine{}
	listing := handledRead("社区镜像：\n名称=最强AI数字人InfiniteTalk, 部署次数=16969")
	listing.Presentation = capability.ReadPresentationBrowse
	readObservationOf(t, e, listing)

	final := substituteReadObservationBlocks("推荐 InfiniteTalk {{READ_OBSERVATION_1}}", e.readResponseEvidenceThisTurn)
	assert.NotContains(t, final, "部署次数=16969")
}
