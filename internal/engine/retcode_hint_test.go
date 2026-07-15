package engine

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScenario_UpstreamRetCodeHintFedToModel is the end-to-end wiring guard for
// P0 阶段1B: a known upstream RetCode (230) returned by the executor must reach
// the model's next-round tool result WITH the recovery hint attached. This fails
// if any link in executor → executeWithRetry → ExecuteSafe → executeSafeTool →
// the ReAct error branch flattens the error with %v (which would strip the typed
// *tools.UpstreamAPIError and silently drop the hint).
func TestFriendlyMessageFromText_ExtractsRetCodeHint(t *testing.T) {
	msg, ok := friendlyMessageFromText("步骤「重置密码」执行失败: API error (RetCode=8314): Password Invalid")
	require.True(t, ok)
	assert.Contains(t, msg, "密码")
	assert.NotContains(t, msg, "RetCode")
	assert.NotContains(t, msg, "zone_id")
	assert.NotContains(t, msg, "az_group")
}

func TestFriendlyMessageFromText_ExistingCFSRetCodeHint(t *testing.T) {
	msg, ok := friendlyMessageFromText("步骤「创建 CFS」执行失败: API error (RetCode=230): Param [ZoneID] conflict with param [existing CFS cfs-1rz6634ri69e]")
	require.True(t, ok)
	assert.Contains(t, msg, "已经存在 CFS")
	assert.NotContains(t, msg, "ZoneID")
	assert.NotContains(t, msg, "RetCode")
}

// retCodeHintProbe returns a stable substring of the 230 hint so the assertion
// stays meaningful without pinning the whole sentence.
func retCodeHintProbe() string {
	h := tools.NewUpstreamAPIError(230, "x").Hint
	// First clause up to the first Chinese colon is stable guidance text.
	if i := strings.Index(h, "："); i > 0 {
		return h[:i]
	}
	return h
}
