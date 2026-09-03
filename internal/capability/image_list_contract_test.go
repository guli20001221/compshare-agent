package capability

import (
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestImageListContractHasOnlySourceAndFreeQuery(t *testing.T) {
	reg := NewReadCapability(imageListReadSpec())
	properties := reg.Schema()["properties"].(map[string]any)
	require.Len(t, properties, 2)
	require.Contains(t, properties, "source")
	require.Contains(t, properties, "query")

	for _, query := range []string{"vLLM", ""} {
		request, err := reg.Decode(map[string]any{"source": "community", "query": query})
		require.NoError(t, err)
		require.NoError(t, ValidateCurrentTurnGrounding(request, "推荐一个大模型推理镜像"),
			"catalog search words are model-owned, not user-literal identity fields")
	}
	for _, field := range []string{"mode", "semantic_queries"} {
		_, err := reg.Decode(map[string]any{field: nil})
		require.ErrorContains(t, err, field)
	}
}

func TestImageListQueryMapsToUpstreamWithoutRefiltering(t *testing.T) {
	flat := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-provider", "Name": "Provider Match", "ImageType": "App"},
	}}
	grouped := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "Provider Match", "Author": "creator", "Data": []any{
			map[string]any{"CompShareImageId": "img-provider", "Name": "Provider Match", "VersionName": "v1"},
		}},
	}}
	for _, tc := range []struct {
		name   string
		source platform.ImageSource
		action string
		field  string
		raw    map[string]any
	}{
		{"default platform", "", platformImageAction, "Name", flat},
		{"platform", platform.ImageSourcePlatform, platformImageAction, "Name", flat},
		{"community groups", platform.ImageSourceCommunity, communityImageAction, "FuzzySearch", grouped},
		{"community flat response", platform.ImageSourceCommunity, communityImageAction, "FuzzySearch", flat},
	} {
		for _, query := range []string{"  creator  ", ""} {
			t.Run(tc.name+"/"+query, func(t *testing.T) {
				exec := &fakeReadExec{result: tc.raw}
				result := runImageList(t, exec, ImageListRequest{Source: tc.source, Query: query})
				require.Equal(t, platform.ReadStatusHandled, result.Status)
				require.Len(t, exec.calls, 1)
				require.Equal(t, tc.action, exec.calls[0].action)
				want := map[string]any{"Limit": 100, "Offset": 0}
				if query != "" {
					want[tc.field] = "creator"
				}
				if tc.source == platform.ImageSourceCommunity {
					want["ExcludeReadme"] = true
				}
				require.Equal(t, want, exec.calls[0].args)
				require.NotNil(t, result.Envelope)
				require.Len(t, result.Envelope.Subjects, 1)
				require.Equal(t, "image:img-provider", result.Envelope.Subjects[0].ID,
					"the real upstream candidate survives without a second local name filter")
			})
		}
	}
}

func TestTenantImageQueriesStayOnTheirScopedLists(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-torch", "Name": "PyTorch 2.9", "Status": "Available"},
		map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04", "Status": "Available"},
	}}
	for _, tc := range []struct {
		source platform.ImageSource
		action string
	}{
		{platform.ImageSourceCustom, customImageAction},
		{platform.ImageSourceShared, sharedImageAction},
	} {
		t.Run(string(tc.source), func(t *testing.T) {
			exec := &fakeReadExec{result: raw}
			result := runImageList(t, exec, ImageListRequest{Source: tc.source, Query: "PyTorch"})
			require.Equal(t, platform.ReadStatusHandled, result.Status)
			require.Len(t, exec.calls, 1)
			require.Equal(t, tc.action, exec.calls[0].action)
			require.Equal(t, map[string]any{"Limit": 100, "Offset": 0}, exec.calls[0].args,
				"these APIs have no Name/FuzzySearch parameter; filtering must not change tenant scope")
			require.Contains(t, result.Reply, "PyTorch")
			require.NotContains(t, result.Reply, "Ubuntu")
		})
	}
}
