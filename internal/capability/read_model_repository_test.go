package capability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/deployment"
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

func runModelRepositoryWithZones(t *testing.T, exec ReadExecutor, req ModelRepositoryRequest, zones *deployment.ZoneCatalogSnapshot) ReadResult {
	t.Helper()
	reg := NewReadCapability(modelRepositoryReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec, ZoneCatalog: zones})
}

// --- args parity (typed query/mode replaces Slots) ------------------------------

// TestModelRepositoryHandle_Empty: no tags and no models is a structured Empty
// read (issue 1); a no-match that still shows the tag vocabulary stays Handled.
func TestModelRepositoryHandle_Empty(t *testing.T) {
	result := runModelRepository(t, &mapReadExec{}, ModelRepositoryRequest{})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Contains(t, result.Reply, "未获取到模型仓库数据")
}

func TestModelRepositoryDescriptionDoesNotClaimDeployability(t *testing.T) {
	description := NewReadCapability(modelRepositoryReadSpec()).Tool.Function.Description
	for _, want := range []string{
		"目录记录不等于目标实例已预置",
		"目标可用区副本健康",
		"不是可创建的镜像目录",
		"不能证明平台已支持部署",
	} {
		require.Contains(t, description, want)
	}
}

func TestModelRepositoryPageDoesNotClaimTheWholeCatalogIsEmpty(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		modelRepositoryModelAction: {"TotalCount": float64(101), "Models": []any{
			map[string]any{"Name": "older-model", "Status": "Active", "MissingZoneIDs": []any{float64(5002)}},
		}},
	}}
	args := modelRepositoryArgs(ModelRepositoryRequest{Offset: 100}, nil, 0)
	assert.Equal(t, 100, args["Offset"])
	assert.Equal(t, imageModelBrowseDisplayCap, args["Limit"], "a page must not fetch more candidates than the renderer exposes")
	reply, empty := renderModelRepositoryReply(exec.results[modelRepositoryModelAction], nil,
		ModelRepositoryRequest{Offset: 100, ReplicaStatus: "Missing"}, 5001, "目标区")
	assert.False(t, empty)
	assert.Contains(t, reply, "共 101 个模型")
	assert.Contains(t, reply, "本页未找到")
	assert.NotContains(t, reply, "仓库里暂时没有")
	result := runModelRepository(t, exec, ModelRepositoryRequest{Offset: 100})
	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, 100, exec.calls[1].args["Offset"])
	assert.Contains(t, result.Reply, "older-model")
}

func TestModelRepositoryArgs_MatchesTag(t *testing.T) {
	req := ModelRepositoryRequest{Query: "LLM", Mode: platform.ListModeFiltered}
	args := modelRepositoryArgs(req, map[string]any{"Tags": []any{"LLM", "图像生成"}}, 0)
	require.Equal(t, []string{"LLM"}, args["Tags"])
	_, hasKeyword := args["Keyword"]
	require.False(t, hasKeyword, "derived tag match should not also set Keyword: %#v", args)
}

func TestModelRepositoryArgs_UsesQuery(t *testing.T) {
	req := ModelRepositoryRequest{Query: "Qwen", Mode: platform.ListModeFiltered}
	args := modelRepositoryArgs(req, map[string]any{"Tags": []any{"LLM"}}, 0)
	require.Equal(t, "Qwen", args["Keyword"])
}

func TestModelRepositoryArgs_ForwardsCurrentUpstreamFilters(t *testing.T) {
	req := ModelRepositoryRequest{
		Query: "Qwen", Source: "ModelScope", Tags: []string{"LLM"}, Categories: []string{"NLP"},
		Status: "Active", ReplicaStatus: "Healthy", Mode: platform.ListModeFiltered,
	}
	args := modelRepositoryArgs(req, map[string]any{}, 42)
	assert.Equal(t, "Qwen", args["Keyword"])
	assert.Equal(t, "ModelScope", args["Source"])
	assert.Equal(t, []string{"LLM"}, args["Tags"])
	assert.Equal(t, []string{"NLP"}, args["Categories"])
	assert.Equal(t, "Active", args["Status"])
	assert.NotContains(t, args, "ReplicaStatus", "upstream Healthy is global and must not over-filter a target-zone health query")
	assert.Equal(t, uint32(42), args["ZoneID"])
}

func TestModelRepositoryArgs_IssueStatusUsesGlobalPrefilterWithoutAvailableZoneFilter(t *testing.T) {
	req := ModelRepositoryRequest{ReplicaStatus: "Offline"}
	args := modelRepositoryArgs(req, map[string]any{}, 42)
	assert.Equal(t, "Offline", args["ReplicaStatus"])
	assert.NotContains(t, args, "ZoneID", "ZoneID only matches AvailableZoneIDs upstream and would erase offline results")
}

// --- render parity (mirrors intent's TestRenderModelRepositoryReply_*) ----------

func TestModelRepositoryRender_ListAll(t *testing.T) {
	modelRaw := map[string]any{"Models": []any{
		map[string]any{"Name": "Qwen2.5-7B", "Path": "/models/qwen", "Tag": "LLM,Qwen", "Size": "15GB", "Deleted": float64(0)},
		map[string]any{"Name": "DeletedModel", "Path": "/models/deleted", "Tag": "LLM", "Size": "1GB", "Deleted": float64(1)},
	}}
	tagRaw := map[string]any{"Tags": []any{"LLM", "图像生成", "LLM"}}
	reply, _ := renderModelRepositoryReply(modelRaw, tagRaw, ModelRepositoryRequest{Mode: platform.ListModeAll}, 0, "")
	for _, want := range []string{"模型仓库标签", "LLM", "模型仓库列表", "Qwen2.5-7B", "/models/qwen"} {
		assert.Contains(t, reply, want)
	}
	assert.NotContains(t, reply, "LLM、图像生成、LLM", "tags must be de-duplicated")
	assert.NotContains(t, reply, "DeletedModel", "deleted model must not render")
	for _, want := range []string{"目录记录", "想部署", "自行拉取"} {
		assert.Contains(t, reply, want, "found listing must bridge to deploy/self-pull follow-ups")
	}
	assert.NotContains(t, reply, "无需重新下载")
}

func TestModelRepositoryRender_UpstreamNoMatch(t *testing.T) {
	modelRaw := map[string]any{"Models": []any{}}
	tagRaw := map[string]any{"Tags": []any{"LLM"}}
	reply, _ := renderModelRepositoryReply(modelRaw, tagRaw, ModelRepositoryRequest{Query: "llama", Mode: platform.ListModeFiltered}, 0, "")
	assert.Contains(t, reply, "未找到匹配的模型")
	for _, want := range []string{"自行拉取", "ollama pull"} {
		assert.Contains(t, reply, want, "a repo miss must guide the user to self-pull")
	}
	assert.NotContains(t, reply, "无需重新下载", "the pre-download note belongs to a found listing, not a miss")
}

func TestModelRepositoryPreservesUpstreamTagMatches(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		modelRepositoryTagAction: {"Tags": []any{"LLM"}},
		modelRepositoryModelAction: {"TotalCount": float64(1), "Models": []any{
			map[string]any{"Name": "Qwen3-8B", "Tags": []any{"LLM"}, "Status": "Active"},
		}},
	}}
	result := runModelRepository(t, exec, ModelRepositoryRequest{Query: "LLM模型", Mode: platform.ListModeFiltered})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, []string{"LLM"}, exec.calls[1].args["Tags"])
	assert.NotContains(t, exec.calls[1].args, "Keyword")
	assert.Contains(t, result.Reply, "Qwen3-8B")
	assert.NotContains(t, result.Reply, "未找到匹配")
}

func TestModelRepositoryRender_CapsDefaultOutputAtTen(t *testing.T) {
	models := make([]any, 0, imageModelBrowseDisplayCap+3)
	for i := 0; i < imageModelBrowseDisplayCap+3; i++ {
		models = append(models, map[string]any{"Name": fmt.Sprintf("Qwen-%d", i), "Path": "/m", "Size": "1GB"})
	}
	reply, _ := renderModelRepositoryReply(map[string]any{"Models": models}, map[string]any{}, ModelRepositoryRequest{Mode: platform.ListModeAll}, 0, "")
	require.Equal(t, imageModelBrowseDisplayCap, strings.Count(reply, "Name=Qwen-"),
		"reply should cap the model list at the browse display cap: %s", reply)
	assert.Contains(t, reply, fmt.Sprintf("共 %d 个", imageModelBrowseDisplayCap+3))
	assert.Contains(t, reply, fmt.Sprintf("已显示前 %d 个", imageModelBrowseDisplayCap))
}

func TestModelRepositoryReplicaRequiresActiveHealthyTargetZone(t *testing.T) {
	entry := map[string]any{
		"Name": "Qwen", "Status": "Active",
		"AvailableZoneIDs": []any{float64(42)},
	}
	assert.Contains(t, modelRepositoryReplicaLabel(entry, 42, "测试区"), "副本健康、可直接使用")
	entry["Status"] = "Offline"
	assert.NotContains(t, modelRepositoryReplicaLabel(entry, 42, "测试区"), "可直接使用")
	entry["Status"] = "Active"
	entry["IncompleteZoneIDs"] = []any{float64(42)}
	assert.Contains(t, modelRepositoryReplicaLabel(entry, 42, "测试区"), "副本不完整")
}

func TestModelRepositoryTargetReplicaFilterUsesExactZoneArrays(t *testing.T) {
	models := []map[string]any{
		{"Name": "offline-here", "Status": "Active", "OfflineZoneIDs": []any{float64(42)}},
		{"Name": "offline-elsewhere", "Status": "Active", "OfflineZoneIDs": []any{float64(43)}},
		{"Name": "healthy-here", "Status": "Active", "AvailableZoneIDs": []any{float64(42)}},
	}
	offline := filterModelRepositoryTargetReplica(models, 42, "Offline")
	require.Len(t, offline, 1)
	assert.Equal(t, "offline-here", offline[0]["Name"])
	healthy := filterModelRepositoryTargetReplica(models, 42, "Healthy")
	require.Len(t, healthy, 1)
	assert.Equal(t, "healthy-here", healthy[0]["Name"])
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

func TestModelRepositoryHandle_ResolvesZoneFromLiveCatalog(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeModelRepositoryTags": {},
		"DescribeModelRepositoryModels": {"Models": []any{map[string]any{
			"Name": "Qwen", "Status": "Active", "AvailableZoneIDs": []any{float64(42)},
		}}},
	}}
	zones := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{{
		Placement: deployment.ZonePlacement{Zone: "cn-test-01", ZoneID: 42}, DisplayName: "测试一区",
	}})
	result := runModelRepositoryWithZones(t, exec, ModelRepositoryRequest{Zone: "cn-test-01"}, zones)
	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, uint32(42), exec.calls[1].args["ZoneID"])
	assert.Contains(t, result.Reply, "副本健康、可直接使用")
}

func TestModelRepositoryHandle_UnresolvedZoneReturnsCatalogForCorrection(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		modelRepositoryTagAction: {"Tags": []any{"LLM"}},
	}}
	zones := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-01", ZoneID: 41}, DisplayName: "上海二A"},
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", ZoneID: 42}, DisplayName: "上海二B"},
	})

	result := runModelRepositoryWithZones(t, exec, ModelRepositoryRequest{Zone: "上海"}, zones)

	require.Equal(t, platform.ReadStatusNeedsInput, result.Status)
	require.Equal(t, platform.ReadFallbackValidation, result.FallbackReason)
	require.NotNil(t, result.Envelope)
	assert.Contains(t, result.Reply, "本次未查询模型副本状态")
	assert.Contains(t, result.Reply, "上海二A")
	assert.Contains(t, result.Reply, "上海二B")
	require.Len(t, exec.calls, 1, "an unresolved zone must stop before the model-repository query")
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
