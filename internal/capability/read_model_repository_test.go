package capability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapReadExec returns a per-action result (and optional per-action error),
// recording every call. Used by capabilities whose handler makes more than one
// distinct upstream call — model repository reads tags then models.
type mapReadExec struct {
	results map[string]map[string]any
	errs    map[string]error
	calls   []fakeReadExecCall
}

func (m *mapReadExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, fakeReadExecCall{action: action, args: args})
	if m.errs != nil {
		if err, ok := m.errs[action]; ok {
			return nil, err
		}
	}
	if m.results != nil {
		if r, ok := m.results[action]; ok {
			return r, nil
		}
	}
	return map[string]any{}, nil
}

func (m *mapReadExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return m.Execute(ctx, action, args)
}

func runModelRepository(t *testing.T, exec ReadExecutor, req ModelRepositoryRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(modelRepositoryReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec})
}

// --- args parity (typed query/mode replaces Slots) ------------------------------

// TestModelRepositoryHandle_Empty: no tags and no models is a structured Empty
// read (issue 1); a no-match that still shows the tag vocabulary stays Handled.
func TestModelRepositoryHandle_Empty(t *testing.T) {
	result := runModelRepository(t, &mapReadExec{}, ModelRepositoryRequest{})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Contains(t, result.Reply, "未获取到模型仓库数据")
}

func TestModelRepositoryArgs_MatchesTag(t *testing.T) {
	args := modelRepositoryArgs("LLM", platform.ListModeFiltered, map[string]any{"Tags": []any{"LLM", "图像生成"}})
	require.Equal(t, "LLM", args["tags"])
	_, hasName := args["name"]
	require.False(t, hasName, "tag match should not also set name: %#v", args)
}

func TestModelRepositoryArgs_UsesQuery(t *testing.T) {
	args := modelRepositoryArgs("Qwen", platform.ListModeFiltered, map[string]any{"Tags": []any{"LLM"}})
	require.Equal(t, "qwen", args["name"])
}

// --- render parity (mirrors intent's TestRenderModelRepositoryReply_*) ----------

func TestModelRepositoryRender_ListAll(t *testing.T) {
	modelRaw := map[string]any{"Models": []any{
		map[string]any{"Name": "Qwen2.5-7B", "Path": "/models/qwen", "Tag": "LLM,Qwen", "Size": "15GB", "Deleted": float64(0)},
		map[string]any{"Name": "DeletedModel", "Path": "/models/deleted", "Tag": "LLM", "Size": "1GB", "Deleted": float64(1)},
	}}
	tagRaw := map[string]any{"Tags": []any{"LLM", "图像生成", "LLM"}}
	reply, _ := renderModelRepositoryReply(modelRaw, tagRaw, "", platform.ListModeAll)
	for _, want := range []string{"模型仓库标签", "LLM", "模型仓库列表", "Qwen2.5-7B", "/models/qwen"} {
		assert.Contains(t, reply, want)
	}
	assert.NotContains(t, reply, "LLM、图像生成、LLM", "tags must be de-duplicated")
	assert.NotContains(t, reply, "DeletedModel", "deleted model must not render")
	for _, want := range []string{"无需重新下载", "想部署", "自行拉取"} {
		assert.Contains(t, reply, want, "found listing must bridge to deploy/self-pull follow-ups")
	}
}

func TestModelRepositoryRender_NameFilterNoMatch(t *testing.T) {
	modelRaw := map[string]any{"Models": []any{
		map[string]any{"Name": "Qwen2.5-7B", "Path": "/models/qwen", "Tag": "LLM", "Size": "15GB"},
	}}
	tagRaw := map[string]any{"Tags": []any{"LLM"}}
	reply, _ := renderModelRepositoryReply(modelRaw, tagRaw, "llama", platform.ListModeFiltered)
	assert.Contains(t, reply, "未找到匹配的模型")
	for _, want := range []string{"自行拉取", "ollama pull"} {
		assert.Contains(t, reply, want, "a repo miss must guide the user to self-pull")
	}
	assert.NotContains(t, reply, "无需重新下载", "the pre-download note belongs to a found listing, not a miss")
}

func TestModelRepositoryRender_CapsDefaultOutputAtTen(t *testing.T) {
	models := make([]any, 0, imageModelBrowseDisplayCap+3)
	for i := 0; i < imageModelBrowseDisplayCap+3; i++ {
		models = append(models, map[string]any{"Name": fmt.Sprintf("Qwen-%d", i), "Path": "/m", "Size": "1GB"})
	}
	reply, _ := renderModelRepositoryReply(map[string]any{"Models": models}, map[string]any{}, "", platform.ListModeAll)
	require.Equal(t, imageModelBrowseDisplayCap, strings.Count(reply, "Name=Qwen-"),
		"reply should cap the model list at the browse display cap: %s", reply)
	assert.Contains(t, reply, fmt.Sprintf("共 %d 个", imageModelBrowseDisplayCap+3))
	assert.Contains(t, reply, fmt.Sprintf("已显示前 %d 个", imageModelBrowseDisplayCap))
}

// --- handler wiring: two upstream calls, tags then models -----------------------

func TestModelRepositoryHandle_TwoCallsAndRenders(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeModelRepositoryTags":   {"Tags": []any{"LLM"}},
		"DescribeModelRepositoryModels": {"Models": []any{map[string]any{"Name": "Qwen2.5-7B", "Path": "/models/qwen", "Size": "15GB"}}},
	}}

	result := runModelRepository(t, exec, ModelRepositoryRequest{Mode: platform.ListModeAll})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "DescribeModelRepositoryModels", result.ToolAction)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "DescribeModelRepositoryTags", exec.calls[0].action)
	assert.Equal(t, "DescribeModelRepositoryModels", exec.calls[1].action)
	assert.Contains(t, result.Reply, "Qwen2.5-7B")
	assert.Nil(t, result.Envelope)
}

func TestModelRepositoryHandle_TagUpstreamError(t *testing.T) {
	result := runModelRepository(t, errReadExec{err: errors.New("boom")}, ModelRepositoryRequest{})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, "DescribeModelRepositoryTags", result.ToolAction)
	assert.Equal(t, modelRepositoryCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}

func TestModelRepositoryHandle_ModelUpstreamError(t *testing.T) {
	exec := &mapReadExec{
		results: map[string]map[string]any{"DescribeModelRepositoryTags": {"Tags": []any{"LLM"}}},
		errs:    map[string]error{"DescribeModelRepositoryModels": errors.New("boom")},
	}

	result := runModelRepository(t, exec, ModelRepositoryRequest{})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, "DescribeModelRepositoryModels", result.ToolAction, "the failing step's action is surfaced")
	require.Len(t, exec.calls, 2)
}
