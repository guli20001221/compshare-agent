package capability

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pagedInstanceExec struct {
	rows  []map[string]any
	calls []fakeReadExecCall
}

type missingTotalInstanceExec struct {
	rows  []map[string]any
	calls []fakeReadExecCall
}

func (e *missingTotalInstanceExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, fakeReadExecCall{action: action, args: args})
	rows := make([]any, 0, len(e.rows))
	for _, row := range e.rows {
		rows = append(rows, row)
	}
	return map[string]any{"UHostSet": rows}, nil
}

func (e *missingTotalInstanceExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func (e *pagedInstanceExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, fakeReadExecCall{action: action, args: args})
	if ids, ok := args["UHostIds"].([]string); ok && len(ids) > 0 {
		for _, row := range e.rows {
			if stringField(row, "UHostId") == ids[0] {
				return describeFixture(row), nil
			}
		}
		return describeFixture(), nil
	}
	offset, _ := args["Offset"].(int)
	if offset >= len(e.rows) {
		return map[string]any{"TotalCount": float64(len(e.rows)), "UHostSet": []any{}}, nil
	}
	end := offset + describeInstancePageLimit
	if end > len(e.rows) {
		end = len(e.rows)
	}
	page := make([]any, 0, end-offset)
	for _, row := range e.rows[offset:end] {
		page = append(page, row)
	}
	return map[string]any{"TotalCount": float64(len(e.rows)), "UHostSet": page}, nil
}

func (e *pagedInstanceExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func accountWithInstances(count int) []map[string]any {
	rows := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, instanceRowMap(
			fmt.Sprintf("uhost-page-%03d", i),
			fmt.Sprintf("instance-%03d", i),
			"Running",
		))
	}
	return rows
}

func TestDescribeAllAccountInstancesRestoresGlobalCreateTimeOrder(t *testing.T) {
	rows := accountWithInstances(101)
	for i := range rows {
		rows[i]["CreateTime"] = float64(i)
	}
	exec := &pagedInstanceExec{rows: rows}

	raw, err := describeAllAccountInstances(context.Background(), exec)

	require.NoError(t, err)
	all := mapSliceAt(raw, "UHostSet")
	require.Len(t, all, 101)
	first, _ := all[0].(map[string]any)
	last, _ := all[len(all)-1].(map[string]any)
	assert.Equal(t, "uhost-page-100", stringField(first, "UHostId"))
	assert.Equal(t, "uhost-page-000", stringField(last, "UHostId"))
}

func TestDescribeAllAccountInstancesStopsOnAPartialPageWithoutTotalCount(t *testing.T) {
	exec := &missingTotalInstanceExec{rows: accountWithInstances(2)}

	raw, err := describeAllAccountInstances(context.Background(), exec)

	require.NoError(t, err)
	assert.Len(t, exec.calls, 1)
	assert.Len(t, mapSliceAt(raw, "UHostSet"), 2)
}

func TestResourceInfoFetchesEveryDescribePageBeforeDisplayTruncation(t *testing.T) {
	exec := &pagedInstanceExec{rows: accountWithInstances(101)}

	result := runResource(t, exec, nil, ResourceInfoRequest{})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, 0, exec.calls[0].args["Offset"])
	assert.Equal(t, 100, exec.calls[1].args["Offset"])
	assert.Contains(t, result.Reply, "已显示 50/101 台",
		"pagination is internal; the existing model-visible display cap remains unchanged")
}

func TestColdNameResolutionFindsAnInstanceOnALaterDescribePage(t *testing.T) {
	rows := accountWithInstances(101)
	rows[100]["Name"] = "only-on-second-page"
	exec := &pagedInstanceExec{rows: rows}

	result := runResource(t, exec, coldRegistrySnapshot(), ResourceInfoRequest{
		Targets: []platform.TargetRef{{
			Type: platform.TargetRefName, Value: "only-on-second-page", Source: platform.SourceUserText,
		}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 3, "two listing pages followed by one exact point query")
	assert.Equal(t, 0, exec.calls[0].args["Offset"])
	assert.Equal(t, 100, exec.calls[1].args["Offset"])
	assert.Equal(t, []string{"uhost-page-100"}, exec.calls[2].args["UHostIds"])
	assert.Contains(t, result.Reply, "uhost-page-100")
}
