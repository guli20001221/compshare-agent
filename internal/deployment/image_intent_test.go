package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func catalogIntentCatalog() *ImageCatalogSnapshot {
	return NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{
			ID: "torch-old", Name: "cuda124_torch280_py311", Source: "platform",
			Status: ImageStatusAvailable, Tags: []string{"pytorch"},
			Software: SoftwareFacts{
				Present: true, Framework: "PyTorch", FrameworkVersionIndex: 280,
			},
		},
		{
			ID: "torch-new", Name: "cuda128_torch291_py312", Source: "platform",
			Status: ImageStatusAvailable, Tags: []string{"pytorch"},
			Software: SoftwareFacts{
				Present: true, Framework: "PyTorch", FrameworkVersionIndex: 291,
			},
		},
		{
			ID: "tf", Name: "cuda128_tf220_py312", Source: "platform",
			Status: ImageStatusAvailable, Tags: []string{"tensorflow"},
			Software: SoftwareFacts{
				Present: true, Framework: "TensorFlow", FrameworkVersionIndex: 220,
			},
		},
	})
}

func TestInferImageCatalogRequestUsesOnlyLiteralLiveCatalogFact(t *testing.T) {
	req, ok := InferImageCatalogRequest(
		catalogIntentCatalog(),
		"在华北一C用最新pytorch为我创建一台4090",
		"platform",
	)

	require.True(t, ok)
	assert.Equal(t, "PyTorch", req.Framework)
	assert.Empty(t, req.Tag, "the same literal framework and tag are one signal, not two ranking votes")
	assert.Equal(t, "platform", req.Source)

	ranked := RankImages(catalogIntentCatalog(), req)
	require.Len(t, ranked, 2)
	assert.Equal(t, "torch-new", ranked[0].ID,
		"the recovered framework narrows the catalog; the ordinary version ladder chooses newest")
	assert.Equal(t, "torch-old", ranked[1].ID)
}

func TestInferImageCatalogRequestDoesNotDoubleCountSameFrameworkAndTag(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{
			ID: "new", Name: "cuda132_torch2130_py312", Source: "platform",
			Status: ImageStatusAvailable, Tags: nil,
			Software: SoftwareFacts{
				Present: true, Framework: "PyTorch", FrameworkVersion: "2.13.0",
			},
		},
		{
			ID: "old-tagged", Name: "cuda126_torch271_py312", Source: "platform",
			Status: ImageStatusAvailable, Tags: []string{"pytorch"},
			Software: SoftwareFacts{
				Present: true, Framework: "PyTorch", FrameworkVersion: "2.7.1",
			},
		},
	})

	req, ok := InferImageCatalogRequest(snap, "用最新 pytorch 创建", "platform")
	require.True(t, ok)
	assert.Equal(t, "PyTorch", req.Framework)
	assert.Empty(t, req.Tag)

	ranked := RankImages(snap, req)
	require.Len(t, ranked, 2)
	assert.Equal(t, "new", ranked[0].ID,
		"an older row carrying the duplicate tag must not outrank the newer structured framework row")
}

func TestInferImageCatalogRequestUsesTagOnRuntimeNamedRowsWithoutSoftwareFacts(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{
			ID: "torch-old", Name: "cuda124_torch280_py311", Source: "platform",
			Status: ImageStatusAvailable, Tags: []string{"pytorch"}, PubTime: 100,
		},
		{
			ID: "torch-new", Name: "cuda128_torch291_py312", Source: "platform",
			Status: ImageStatusAvailable, Tags: []string{"pytorch"}, PubTime: 200,
		},
		{
			ID: "tf", Name: "cuda128_tf220_py312", Source: "platform",
			Status: ImageStatusAvailable, Tags: []string{"tensorflow"}, PubTime: 300,
		},
	})

	req, ok := InferImageCatalogRequest(snap, "用最新 PYTORCH 创建", "platform")
	require.True(t, ok)
	assert.Empty(t, req.Framework, "the catalog carried no software facts; absence stays honest")
	assert.Equal(t, "pytorch", req.Tag)

	ranked := RankImages(snap, req)
	require.Len(t, ranked, 2)
	assert.Equal(t, "torch-new", ranked[0].ID)
	assert.Equal(t, "torch-old", ranked[1].ID)
}

func TestInferImageCatalogRequestDoesNotTreatHardwareAsImageIntent(t *testing.T) {
	_, ok := InferImageCatalogRequest(
		catalogIntentCatalog(),
		"在华北一C为我创建一台4090",
		"platform",
	)
	assert.False(t, ok)
}

func TestInferImageCatalogRequestRefusesTwoFrameworks(t *testing.T) {
	_, ok := InferImageCatalogRequest(
		catalogIntentCatalog(),
		"PyTorch 或 TensorFlow 都可以",
		"platform",
	)
	assert.False(t, ok)
}

func TestInferImageCatalogRequestScopesToQueriedSource(t *testing.T) {
	_, ok := InferImageCatalogRequest(
		catalogIntentCatalog(),
		"最新 PyTorch",
		"community",
	)
	assert.False(t, ok)
}

func TestInferImageCatalogRequestPrefersLongerOverlappingCatalogFact(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "pytorch", Name: "PyTorch image", Source: "platform", Software: SoftwareFacts{Present: true, Framework: "PyTorch"}},
		{ID: "torch", Name: "Torch image", Source: "platform", Software: SoftwareFacts{Present: true, Framework: "Torch"}},
	})

	req, ok := InferImageCatalogRequest(snap, "最新pytorch", "platform")
	require.True(t, ok)
	assert.Equal(t, "PyTorch", req.Framework)
}
