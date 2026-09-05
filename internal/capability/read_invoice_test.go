package capability

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runInvoiceStatus(t *testing.T, exec ReadExecutor) ReadResult {
	t.Helper()
	reg := NewReadCapability(invoiceStatusReadSpec())
	return reg.Run(context.Background(), InvoiceStatusRequest{}, ReadRuntime{Executor: exec})
}

func TestInvoiceStatusCallsTheAccountScopedUpstreamAndProjectsStatusEvidence(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{
		"TotalCount": float64(1),
		"InvoiceSet": []any{map[string]any{
			"InvoiceID":      float64(42),
			"InvoiceAmount":  float64(12345),
			"InvoiceState":   "InvoiceIssued",
			"InvoiceMode":    "PAPER_MODE",
			"InvoiceType":    "Common",
			"RequestTime":    float64(1788220800),
			"InvoiceTitle":   "不应投影的抬头",
			"ReceiveEmail":   "private@example.com",
			"ExpressAddress": "不应投影的地址",
			"InvoiceNo":      []any{"sensitive-number"},
			"DownloadUrl":    "https://example.invalid/private",
		}},
	}}

	result := runInvoiceStatus(t, exec)

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, invoiceStatusAction, result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, invoiceStatusAction, exec.calls[0].action)
	assert.Equal(t, map[string]any{"Limit": 100, "Offset": 0}, exec.calls[0].args, "tenant identity is server-owned; only pagination is forwarded")
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindInvoiceStatus, result.Envelope.Kind)
	require.Len(t, result.Envelope.Subjects, 1)
	assert.Equal(t, envelope.SubjectInvoice, result.Envelope.Subjects[0].Type)
	assert.Contains(t, result.Reply, "开具完成，待邮寄")
	assert.Contains(t, result.Reply, "¥123.45")

	encoded, err := json.Marshal(result.Envelope)
	require.NoError(t, err)
	for _, omitted := range []string{"不应投影的抬头", "private@example.com", "不应投影的地址", "sensitive-number", "example.invalid"} {
		assert.NotContains(t, string(encoded), omitted)
	}
}

func TestInvoiceStatusEmptyAndFailureStayTyped(t *testing.T) {
	empty := runInvoiceStatus(t, &fakeReadExec{result: map[string]any{"TotalCount": float64(0), "InvoiceSet": []any{}}})
	require.Equal(t, platform.ReadStatusEmpty, empty.Status)
	require.NotNil(t, empty.Envelope)
	assert.Equal(t, envelope.KindInvoiceStatus, empty.Envelope.Kind)
	assert.Contains(t, empty.Reply, "没有查询到发票记录")

	failed := runInvoiceStatus(t, errReadExec{err: errors.New("boom")})
	assert.Equal(t, platform.ReadStatusFailureAfterTool, failed.Status)
	assert.Equal(t, invoiceStatusAction, failed.ToolAction)
}

func TestInvoiceStatusDoesNotReportEmptyWhenUpstreamRecordsCannotBeProjected(t *testing.T) {
	for name, upstream := range map[string]map[string]any{
		"row without a usable invoice id": {
			"TotalCount": float64(1),
			"InvoiceSet": []any{map[string]any{"InvoiceID": "not-numeric"}},
		},
		"positive total without projected rows": {
			"TotalCount": float64(2),
			"InvoiceSet": []any{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := runInvoiceStatus(t, &fakeReadExec{result: upstream})

			assert.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
			assert.Equal(t, invoiceStatusAction, result.ToolAction)
			assert.NotContains(t, result.Reply, "没有查询到发票记录")
		})
	}
}

func TestInvoiceStatusIsAConcreteModelVisibleRead(t *testing.T) {
	toolName := ReadToolName(intent.IntentInvoiceStatus)
	reg, ok := RegisteredReadForTool(toolName)
	require.True(t, ok)
	_, request, err := DecodeReadRequest(toolName, map[string]any{})
	require.NoError(t, err)
	require.IsType(t, InvoiceStatusRequest{}, request)
	assert.Contains(t, reg.Schema()["properties"], "offset")
}

func TestInvoiceStatusCanReadPastTheFirstPage(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{
		"TotalCount": float64(102),
		"InvoiceSet": []any{map[string]any{"InvoiceID": float64(101), "InvoiceAmount": float64(12345), "InvoiceState": "Finished"}},
	}}
	result := NewReadCapability(invoiceStatusReadSpec()).Run(context.Background(), InvoiceStatusRequest{Offset: 100}, ReadRuntime{Executor: exec})
	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Equal(t, map[string]any{"Limit": 100, "Offset": 100}, exec.calls[0].args)
	assert.Contains(t, result.Reply, "发票 101")
	assert.Contains(t, result.Reply, "¥123.45")
	assert.Contains(t, result.Reply, "共 102 条")
	assert.Contains(t, result.Reply, "offset=101")
}
