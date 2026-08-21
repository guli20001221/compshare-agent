package capability

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnavailableRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, unavailableRequest{}.MissingFields())
}

// TestUnavailableCapability_ReturnsStructuredUnavailable: invoking the capability
// yields a structured Unavailable status + a deterministic reply + alternatives,
// and never touches the executor (no real upstream call).
func TestUnavailableCapability_ReturnsStructuredUnavailable(t *testing.T) {
	exec := &fakeReadExec{}
	reg := NewUnavailableCapability(accountFinanceUnavailableSpec())

	result := reg.Run(context.Background(), unavailableRequest{}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusUnavailable, result.Status)
	assert.Contains(t, result.Reply, "不支持直接查询账号余额")
	assert.NotEmpty(t, result.Alternatives)
	assert.Empty(t, exec.calls, "an unavailable capability never calls upstream")
	assert.Nil(t, result.Envelope, "unavailable answers carry no evidence envelope")
}

// TestUnavailableCapability_InReadCatalog: the account-finance capability is
// exposed as a model-visible read tool whose description declares it unavailable,
// decodes with no parameters, and dispatches to the Unavailable vertical.
func TestUnavailableCapability_InReadCatalog(t *testing.T) {
	toolName := ReadToolPrefix + accountFinanceStatusCapability

	reg, ok := RegisteredReadForTool(toolName)
	require.True(t, ok, "account_finance_status must be a registered typed read tool")
	result := reg.Run(context.Background(), unavailableRequest{}, ReadRuntime{})
	require.Equal(t, platform.ReadStatusUnavailable, result.Status)

	_, request, err := DecodeReadRequest(toolName, map[string]any{})
	require.NoError(t, err)
	require.IsType(t, unavailableRequest{}, request)

	var found bool
	for _, d := range ReadDefinitions() {
		if d.Tool.Function != nil && d.Tool.Function.Name == toolName {
			found = true
			assert.Contains(t, d.Tool.Function.Description, "不支持", "tool description must declare the capability is unavailable")
		}
	}
	require.True(t, found, "account_finance_status must appear in the model-visible read catalog")
}
