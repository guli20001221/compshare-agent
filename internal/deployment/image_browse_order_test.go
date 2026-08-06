package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// browseCatalog mirrors what the community browse returns: upstream was asked to
// sort by CreatedCount descending, so entry order IS the popularity order. The
// most-deployed image deliberately has the OLDEST publication date — that is the
// case the two orderings disagree on, and the live one.
func browseCatalog() *ImageCatalogSnapshot {
	return NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "img-popular", Name: "最强AI数字人InfiniteTalk-图片和视频数字人", Source: "community", PubTime: 100},
		{ID: "img-newer-1", Name: "超强AI语音克隆 VoxCPM", Source: "community", PubTime: 900},
		{ID: "img-newer-2", Name: "LTX-2.3视频生成合集", Source: "community", PubTime: 800},
	})
}

func rankedIDs(sels []ImageSelection) []string {
	out := make([]string, 0, len(sels))
	for _, s := range sels {
		out = append(out, s.ID)
	}
	return out
}

// TestBrowseKeepsTheCatalogOrderWhenTheRequestSaysNothing is the reported symptom.
//
// A create that named no image showed a picker of the ten NEWEST community images
// and not one of the image the assistant had just recommended by name and id.
// communityImageBrowseArgs asks upstream for the top rows by CreatedCount
// descending; ranking then gave every row a score of 0 and re-sorted the ties by
// PubTime, discarding the popularity order it had just requested. The most
// deployed image on the platform (16.8k deploys, latest version months old) fell
// below images with a fraction of the usage and a newer date.
//
// With nothing in the request to rank BY, the catalog's own order is the only real
// signal, and it is the one we asked for.
func TestBrowseKeepsTheCatalogOrderWhenTheRequestSaysNothing(t *testing.T) {
	ranked := RankImages(browseCatalog(), ImageRequest{Source: "community"})
	require.Len(t, ranked, 3)
	assert.Equal(t, []string{"img-popular", "img-newer-1", "img-newer-2"}, rankedIDs(ranked),
		"an unqualified browse must present the order upstream was asked for, not re-sort it by date")
}

// TestBrowseStillPrefersTheNamedImage: the ordering change must not weaken the
// case that already worked. A named request outranks catalog order outright.
func TestBrowseStillPrefersTheNamedImage(t *testing.T) {
	ranked := RankImages(browseCatalog(), ImageRequest{Source: "community", Name: "LTX-2.3"})
	require.NotEmpty(t, ranked)
	assert.Equal(t, "img-newer-2", ranked[0].ID,
		"a name the user gave must still win over the catalog's own order")
}

// TestRecencyStillBreaksTiesAmongRealMatches keeps the behaviour recency was for.
// When the request DID discriminate and several entries match equally well, the
// newest of them is the right pick — that is a preference the user expressed,
// unlike a browse where they expressed none.
func TestRecencyStillBreaksTiesAmongRealMatches(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "img-old", Name: "InfiniteTalk", Source: "community", PubTime: 100},
		{ID: "img-new", Name: "InfiniteTalk", Source: "community", PubTime: 900},
	})
	ranked := RankImages(snap, ImageRequest{Source: "community", Name: "InfiniteTalk"})
	require.Len(t, ranked, 2)
	assert.Equal(t, "img-new", ranked[0].ID,
		"among equally-good matches for a name the user gave, the newest still leads")
}

func TestDirectImageNameMatchDoesNotPromotePickerNearMatchesToIdentity(t *testing.T) {
	faceFusion := ImageCatalogEntry{
		Name:        "FaceFusion 3.5.1 / 3.6.1 全模型离线版",
		VersionName: "v3.6",
	}

	assert.True(t, DirectImageNameMatch(faceFusion, "FaceFusion"))
	assert.True(t, DirectImageNameMatch(faceFusion, "v3.6"),
		"用户复制卡片上的具体版本也应直接匹配")
	assert.False(t, DirectImageNameMatch(
		ImageCatalogEntry{Name: "SVC-Fusion_api_rvc", VersionName: "v1.6"},
		"FaceFusion",
	), "共享 Fusion 片段只够进入候选列表，不能证明是同一个镜像")
	assert.False(t, DirectImageNameMatch(
		ImageCatalogEntry{Name: "ComfyUI 5.1"},
		"FaceFusion",
	))
	assert.True(t, DirectImageNameMatch(
		ImageCatalogEntry{Name: "最强AI数字人InfiniteTalk-图片和视频数字人"},
		"最强 AI 数字人 InfiniteTalk",
	), "用户对同一推荐的空格化简称不能推翻已验证的精确镜像 ID")
}
