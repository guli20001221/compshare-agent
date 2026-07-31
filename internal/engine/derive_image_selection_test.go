package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/workflow"
)

// TestDeriveImageSelectionClassifiesProvenance pins the one classification the whole
// guided image fix turns on: an image id the USER named settles the flow; an id the
// Agent proposed while the user named nothing is only a suggestion. Keyed on both the
// id and the name because a bare user NAME (whose id the Agent then resolved) is still
// the user naming the image, not the Agent guessing.
func TestDeriveImageSelectionClassifiesProvenance(t *testing.T) {
	assert.Equal(t, workflow.ImageSelectionUserPinned,
		deriveImageSelection(map[string]actionresolver.ResolvedSlot{
			"CompShareImageId": {Value: "img-1", Source: actionresolver.SourceUserExplicit},
		}),
		"an id the user's own text named is user-pinned")

	assert.Equal(t, workflow.ImageSelectionUserPinned,
		deriveImageSelection(map[string]actionresolver.ResolvedSlot{
			"ImageName": {Value: "PyTorch", Source: actionresolver.SourceUserExplicit},
		}),
		"a bare name copied by the user is user-pinned but still needs the picker to resolve a version")

	assert.Equal(t, workflow.ImageSelectionSuggested,
		deriveImageSelection(map[string]actionresolver.ResolvedSlot{
			"CompShareImageId": {Value: "img-1", Source: actionresolver.SourceAgentInference},
			"ImageName":        {Value: "PyTorch", Source: actionresolver.SourceUserExplicit},
		}),
		"an Agent-chosen concrete version stays suggested even when it is related to a user name")

	assert.Equal(t, workflow.ImageSelectionSuggested,
		deriveImageSelection(map[string]actionresolver.ResolvedSlot{
			"CompShareImageId": {Value: "img-1", Source: actionresolver.SourceAgentInference},
		}),
		"an id the Agent proposed, with the user naming nothing, is a suggestion — the bug's shape")

	assert.Equal(t, workflow.ImageSelectionSuggested,
		deriveImageSelection(map[string]actionresolver.ResolvedSlot{
			"ImageName": {Value: "FaceFusion", Source: actionresolver.SourceAgentInference},
		}),
		"an Agent-inferred name is also only a suggestion, never user settlement")

	assert.Equal(t, workflow.ImageSelectionUnset,
		deriveImageSelection(map[string]actionresolver.ResolvedSlot{}),
		"no image id or name is Unset")

	assert.Equal(t, workflow.ImageSelectionUnset,
		deriveImageSelection(map[string]actionresolver.ResolvedSlot{
			"CompShareImageId": {Value: "", Source: actionresolver.SourceAgentInference},
		}),
		"an empty id is not a suggestion")
}

func TestImageSourceUserPinnedRequiresUserProvenance(t *testing.T) {
	assert.True(t, imageSourceUserPinned(map[string]actionresolver.ResolvedSlot{
		"ImageSource": {Value: "community", Source: actionresolver.SourceUserExplicit},
	}))
	assert.False(t, imageSourceUserPinned(map[string]actionresolver.ResolvedSlot{
		"ImageSource": {Value: "platform", Source: actionresolver.SourceAgentInference},
	}), "an Agent/default source is a search starting point, not a user choice")
	assert.False(t, imageSourceUserPinned(map[string]actionresolver.ResolvedSlot{}))
}
