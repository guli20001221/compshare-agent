package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runImageTagCatalog(t *testing.T, exec ReadExecutor, req ImageTagCatalogRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(imageTagCatalogReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec})
}

func TestImageTagCatalogRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, ImageTagCatalogRequest{}.MissingFields())
}

// TestImageTagCatalogHandle_Categorized: a TagIndex + TagsMap payload renders the
// categorized tag listing (parity with intent's TestRenderImageTagCatalog_Categorized).
func TestImageTagCatalogHandle_Categorized(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{
		"TagIndex": []any{"框架", "场景"},
		"TagsMap": map[string]any{
			"框架": []any{"PyTorch", "TensorFlow"},
			"场景": []any{"LLM", "图像生成"},
		},
	}}

	result := runImageTagCatalog(t, exec, ImageTagCatalogRequest{})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "DescribeCompShareImageTags", result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "DescribeCompShareImageTags", exec.calls[0].action)
	for _, want := range []string{"镜像标签分类", "框架: PyTorch、TensorFlow", "场景: LLM、图像生成"} {
		assert.Contains(t, result.Reply, want)
	}
	assert.Nil(t, result.Envelope, "image tag catalog carries no evidence envelope (legacy parity)")
}

// TestImageTagCatalogHandle_FlatFallback: no TagIndex/TagsMap but a flat Tags list
// renders the flat listing (parity with TestRenderImageTagCatalog_FlatFallback).
func TestImageTagCatalogHandle_FlatFallback(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{
		"Tags": []any{"深度学习", "ComfyUI", "Stable Diffusion"},
	}}

	result := runImageTagCatalog(t, exec, ImageTagCatalogRequest{})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	for _, want := range []string{"镜像标签", "深度学习", "ComfyUI"} {
		assert.Contains(t, result.Reply, want)
	}
}

// TestImageTagCatalogHandle_Empty: an empty payload is an explicit not-found reply
// (parity with TestRenderImageTagCatalog_Empty), still a Handled status.
func TestImageTagCatalogHandle_Empty(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{}}

	result := runImageTagCatalog(t, exec, ImageTagCatalogRequest{})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "未获取到镜像标签")
}

// TestImageTagCatalogHandle_UpstreamError: an upstream failure is a generic
// post-tool failure labelled with the capability identity (legacy parity: the
// route handler labelled failures with string(intent)).
func TestImageTagCatalogHandle_UpstreamError(t *testing.T) {
	result := runImageTagCatalog(t, errReadExec{err: errors.New("boom")}, ImageTagCatalogRequest{})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, imageTagCatalogCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}
