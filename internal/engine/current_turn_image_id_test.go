package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This is the production-shaped regression: Chinese prose commonly touches an
// ASCII resource id directly ("用compshareImage-…为我创建"), and the model may
// omit the opaque id even when it correctly selected the create operation. The
// runtime must preserve that one literal user choice, then discover its source
// from live catalogs rather than making the user browse a source card again.
func TestCurrentTurnExactImageIDCompletesOmittedProposalAcrossSources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		id     string
	}{
		{name: "community", source: "community", id: "compshareImage-community-direct"},
		{name: "custom", source: "custom", id: "compshareImage-custom-direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
				switch action {
				case "DescribeAvailableCompShareInstanceTypes":
					return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
				case "DescribeCompShareSupportZone":
					return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
				case "DescribeCompShareImages":
					return map[string]any{"ImageSet": []any{}}, nil
				case "DescribeCommunityImages":
					if tc.source != "community" {
						return map[string]any{"CompshareImageGroup": []any{}}, nil
					}
					return map[string]any{"CompshareImageGroup": []any{map[string]any{
						"ImageName": "社区直达镜像", "GroupId": "group-direct",
						"Data": []any{map[string]any{"CompShareImageId": tc.id, "Name": "v1", "Status": "Available"}},
					}}}, nil
				case "DescribeCompShareCustomImages":
					if tc.source != "custom" {
						return map[string]any{"TotalCount": float64(0), "ImageSet": []any{}}, nil
					}
					return map[string]any{"TotalCount": float64(1), "ImageSet": []any{
						map[string]any{"CompShareImageId": tc.id, "Name": "自制直达镜像", "Status": "Available"},
					}}, nil
				default:
					return map[string]any{"RetCode": float64(0)}, nil
				}
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.lastUserMsg = "用" + tc.id + "为我创建一台 4090"
			eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
				eng, eng.lastUserMsg, "turn-direct-"+tc.source, time.Now(),
			)
			eng.turnContextViewReady = true

			// Intentionally omit CompShareImageId to model the exact failure mode.
			resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
				"turn_id":   "turn-direct-" + tc.source,
				"operation": "CreateInstanceWorkflow",
				"slots":     []any{map[string]any{"name": "GpuType", "value": "4090"}},
			})

			require.NoError(t, err)
			require.Equal(t, tc.id, resolved.action.Arguments["CompShareImageId"])
			require.Equal(t, tc.source, resolved.action.Arguments["ImageSource"])
			require.Equal(t, actionresolver.SourceUserExplicit,
				resolved.action.Provenance["CompShareImageId"].Source)
			require.Equal(t, actionresolver.SourceVerifiedContext,
				resolved.action.Provenance["ImageSource"].Source)
			require.Equal(t, workflow.ImageSelectionUserPinned, resolved.referenceData.ImageSelection)
			require.NotNil(t, resolved.referenceData.ImageCatalog)
			_, found := resolved.referenceData.ImageCatalog.ByID(tc.id)
			require.True(t, found)
			assert.Contains(t, executor.calls, "Describe"+map[string]string{
				"community": "CommunityImages",
				"custom":    "CompShareCustomImages",
			}[tc.source])
		})
	}
}

func TestImageIDEvidenceAcceptsChineseAdjacencyButNotASCIISubstrings(t *testing.T) {
	const id = "compshareImage-direct-id"
	text := []rune("请用" + id + "为我创建")
	start, end, ok := uniqueQuoteForCodec(text, []rune(id), actionresolver.CodecImage)
	require.True(t, ok)
	assert.Equal(t, id, string(text[start:end]))
	assert.True(t, containsStandaloneImageID(string(text), id))

	_, _, ok = uniqueQuoteForCodec([]rune("prefix"+id+"suffix"), []rune(id), actionresolver.CodecImage)
	assert.False(t, ok, "a shorter id must not borrow an ASCII identifier's span")
}

func TestCurrentTurnImageIDDoesNotChooseBetweenDifferentIDs(t *testing.T) {
	catalog, err := defaultActionCatalog()
	require.NoError(t, err)
	spec, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)

	proposal := actionresolver.ActionProposal{Operation: "CreateInstanceWorkflow"}
	view := AgentContext{
		TurnID:          "turn-ambiguous-image",
		CurrentQuestion: "比较 compshareImage-first 和 compshareImage-second 后再创建",
	}
	got := completeCurrentTurnImageID(proposal, view, spec)
	assert.Empty(t, got.Slots, "two different current-turn ids require Agent/user disambiguation")
}
