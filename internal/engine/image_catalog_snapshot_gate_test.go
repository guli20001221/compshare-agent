package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageCatalogFetchedOnlyWhenAProposalNamesAnId guards the cost condition that
// came with giving create a CompShareImageId field.
//
// SpecNeedsImageCatalog is a property of the SCHEMA, so it is true for every
// create now — including the overwhelming majority that name no image at all. The
// catalog's only consumer is CodecImage, and a codec runs only on a slot the
// proposal carries; fetching a fully paginated image catalog for a proposal with
// no id would be upstream cost on every create turn producing an answer nothing
// reads. nil here is "not needed", which is distinct from the unavailable snapshot
// a FAILED fetch returns — the resolver refuses on the latter and must never see
// it just because nobody asked for an image.
func TestImageCatalogFetchedOnlyWhenAProposalNamesAnId(t *testing.T) {
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	require.True(t, actionresolver.SpecNeedsImageCatalog(create),
		"premise: the schema alone says create needs the catalog")

	eng := &Engine{}

	assert.Nil(t, eng.imageCatalogSnapshotForSpec(context.Background(), create, "community", ""),
		"a create naming no image must not pay for a catalog fetch")
	assert.Nil(t, eng.imageCatalogSnapshotForSpec(context.Background(), create, "community", "   "),
		"…and blank is not an id")

	// With an id there IS something to verify, so the fetch is attempted. This
	// engine has no executor, so it yields the unavailable snapshot rather than
	// nil — and unavailable is what makes the resolver REFUSE the id instead of
	// passing it through, which is the behavior that must not be skipped.
	snap := eng.imageCatalogSnapshotForSpec(context.Background(), create, "community", "compshareImage-abc")
	require.NotNil(t, snap, "an id must be verified, so the catalog must be fetched")
	assert.False(t, snap.Available(),
		"a fetch that could not run reports unavailable, so the id is refused rather than trusted")

	stop, ok := catalog.Lookup("StopInstanceWorkflow")
	require.True(t, ok)
	assert.Nil(t, eng.imageCatalogSnapshotForSpec(context.Background(), stop, "", "compshareImage-abc"),
		"an operation with no image field never needs the catalog, whatever it was handed")
}

func TestCommunityImagePointQueryParsesLiveGroupedResponse(t *testing.T) {
	const imageID = "compshareImage-1pl06yxr5lvm"
	var gotArgs map[string]any
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		require.Equal(t, "DescribeCommunityImages", action)
		gotArgs = args
		return map[string]any{"CompshareImageGroup": []any{
			map[string]any{
				"GroupId":   "qYl0zvqlo03V",
				"ImageName": "FaceFusion",
				"Status":    "Available",
				"Data": []any{map[string]any{
					"CompShareImageId": imageID,
					"Name":             "FaceFusion",
					"VersionName":      "v3.6",
					"ImageType":        "Community",
					"Status":           "Available",
					"Container":        "True",
				}},
			},
		}}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)

	snap, source := eng.resolveImageCatalogSnapshotForSpec(
		context.Background(), create, "community", imageID, true,
	)

	require.True(t, snap.Available())
	entry, ok := snap.ByID(imageID)
	require.True(t, ok)
	assert.Equal(t, "FaceFusion", entry.Name)
	assert.Equal(t, "community", source)
	assert.Equal(t, imageID, gotArgs["CompShareImageId"])
	assert.NotContains(t, gotArgs, "Offset")
	assert.Equal(t, []string{"DescribeCommunityImages"}, executor.calls,
		"精确社区 ID 只需一次点查，不能分页拉全目录")
}

func TestPlatformImagePointQueryUsesImageSetWhenTotalCountIsZero(t *testing.T) {
	const imageID = "compshareImage-1e3udifakfm9"
	var gotArgs map[string]any
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		require.Equal(t, "DescribeCompShareImages", action)
		gotArgs = args
		return map[string]any{
			"TotalCount": float64(0),
			"ImageSet": []any{map[string]any{
				"CompShareImageId": imageID,
				"Name":             "Windows-nvidia 2022 64位",
				"ImageType":        "System",
				"Status":           "Available",
				"Container":        "False",
			}},
		}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)

	snap, source := eng.resolveImageCatalogSnapshotForSpec(
		context.Background(), create, "platform", imageID, true,
	)

	require.True(t, snap.Available())
	entry, ok := snap.ByID(imageID)
	require.True(t, ok, "平台点查的命中依据是 ImageSet，不是错误的 TotalCount")
	assert.Equal(t, "Windows-nvidia 2022 64位", entry.Name)
	assert.Equal(t, "platform", source)
	assert.Equal(t, imageID, gotArgs["CompShareImageId"])
	assert.Equal(t, []string{"DescribeCompShareImages"}, executor.calls)
}

func TestCustomVerificationUsesTenantListAndSharingUsesScopedExactRead(t *testing.T) {
	tests := []struct {
		source string
		action string
	}{
		{source: "custom", action: "DescribeCompShareCustomImages"},
		{source: "sharing", action: "DescribeCompShareSharingImages"},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			const imageID = "compshareImage-visible-to-this-tenant"
			var gotArgs map[string]any
			executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
				require.Equal(t, tt.action, action)
				gotArgs = args
				return map[string]any{
					"TotalCount": float64(1),
					"ImageSet": []any{map[string]any{
						"CompShareImageId": imageID,
						"Name":             "租户可见镜像",
						"Status":           "Available",
					}},
				}, nil
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)

			snap := eng.imageCatalogSnapshotByID(context.Background(), tt.source, imageID)

			require.True(t, snap.Available())
			entry, ok := snap.ByID(imageID)
			require.True(t, ok)
			assert.Equal(t, tt.source, entry.Source)
			assert.Equal(t, 100, gotArgs["Limit"])
			if tt.source == "custom" {
				assert.NotContains(t, gotArgs, "CompShareImageId")
				assert.Equal(t, 0, gotArgs["Offset"])
			} else {
				assert.Equal(t, imageID, gotArgs["CompShareImageId"])
			}
			assert.Equal(t, []string{tt.action}, executor.calls)
		})
	}
}

func TestUserExplicitImageSourceIsAConstraint(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		require.Equal(t, "DescribeCompShareImages", action,
			"用户明确选择 platform 时不能转去其他来源找同名 ID")
		return nil, tools.NewUpstreamAPIError(230, "Params [CompShareImageId] not available")
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)

	snap, source := eng.resolveImageCatalogSnapshotForSpec(
		context.Background(), create, "platform", "compshareImage-community", true,
	)

	require.True(t, snap.Available())
	assert.Zero(t, snap.Len())
	assert.Empty(t, source)
	assert.Equal(t, []string{"DescribeCompShareImages"}, executor.calls)
}

func TestMissingImageIsInvalidValueRatherThanCatalogOutage(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		require.Equal(t, "DescribeCommunityImages", action)
		return nil, fmt.Errorf("point query: %w",
			tools.NewUpstreamAPIError(8039, "Resource not exist [compshareImage-stale]"))
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)

	snap, source := eng.resolveImageCatalogSnapshotForSpec(
		context.Background(), create, "community", "compshareImage-stale", true,
	)

	require.True(t, snap.Available(), "资源不存在是用户值无效，不是镜像目录故障")
	assert.Zero(t, snap.Len())
	assert.Empty(t, source)
}

func TestMissingImageAcrossAllCandidateSourcesIsNotAnOutage(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			return map[string]any{"TotalCount": float64(0), "ImageSet": []any{}}, nil
		case "DescribeCommunityImages":
			return nil, tools.NewUpstreamAPIError(8039, "Resource not exist [compshareImage-stale]")
		case "DescribeCompShareCustomImages":
			return map[string]any{"TotalCount": float64(0), "ImageSet": []any{}}, nil
		case "DescribeCompShareSharingImages":
			return map[string]any{"TotalCount": float64(0), "ImageSet": []any{}}, nil
		default:
			t.Fatalf("unexpected action %s", action)
			return nil, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)

	snap, source := eng.resolveImageCatalogSnapshotForSpec(
		context.Background(), create, "", "compshareImage-stale", false,
	)

	require.True(t, snap.Available(),
		"所有候选来源都明确回答无此 ID 时，应当拒绝 ID，而不是谎称目录不可用")
	assert.Zero(t, snap.Len())
	assert.Empty(t, source)
	assert.Equal(t, []string{"DescribeCompShareImages", "DescribeCommunityImages", "DescribeCompShareCustomImages", "DescribeCompShareSharingImages"}, executor.calls)
}

func TestCreateExactImageIDCanResolveToSharingWithoutADeclaredSource(t *testing.T) {
	const imageID = "compshareImage-shared-visible"
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSharingImages" {
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId": imageID,
				"Name":             "共享训练环境",
				"Status":           "Available",
			}}}, nil
		}
		if action == "DescribeCommunityImages" {
			return nil, tools.NewUpstreamAPIError(8039, "Resource not exist ["+imageID+"]")
		}
		return map[string]any{"ImageSet": []any{}}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)

	snap, source := eng.resolveImageCatalogSnapshotForSpec(context.Background(), create, "", imageID, false)

	require.True(t, snap.Available())
	entry, ok := snap.ByID(imageID)
	require.True(t, ok)
	assert.Equal(t, "共享训练环境", entry.Name)
	assert.Equal(t, "sharing", entry.Source)
	assert.Equal(t, "sharing", source)
}

func TestImageCatalogDependencyFailureStaysUnavailable(t *testing.T) {
	tests := map[string]error{
		"transport":      errors.New("connection reset"),
		"service":        tools.NewUpstreamAPIError(8433, "service unavailable"),
		"other 230":      tools.NewUpstreamAPIError(230, "Params [organization_id] not available"),
		"disk not found": tools.NewUpstreamAPIError(8351, "can not find disk [disk-1] from uhost[uhost-1]"),
	}
	for name, upstreamErr := range tests {
		t.Run(name, func(t *testing.T) {
			executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
				require.Equal(t, "DescribeCommunityImages", action)
				return nil, upstreamErr
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			catalog, err := actionresolver.BuildCatalog()
			require.NoError(t, err)
			create, ok := catalog.Lookup("CreateInstanceWorkflow")
			require.True(t, ok)

			snap, source := eng.resolveImageCatalogSnapshotForSpec(
				context.Background(), create, "community", "compshareImage-any", true,
			)

			require.False(t, snap.Available(), "真实依赖故障不能伪装成用户镜像 ID 无效")
			assert.Empty(t, source)
		})
	}
}
