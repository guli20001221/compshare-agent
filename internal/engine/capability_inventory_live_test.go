//go:build live

package engine

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

// TestDumpCapabilityInventory writes the exact tool window the production model
// sees, plus the mutating workflow registry. This is the code-grounded answer to
// "can the agent actually do X", used to label the 53 replay cases before any of
// them is scored: a question the agent has no capability for must be judged on
// whether it hands off honestly, not on whether it produced the right answer.
func TestDumpCapabilityInventory(t *testing.T) {
	out := os.Getenv("COMPSHARE_CAPABILITY_OUT")
	require.NotEmpty(t, out, "set COMPSHARE_CAPABILITY_OUT")

	type toolRow struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	collect := func(mutating, instanceOps bool) []toolRow {
		var rows []toolRow
		for _, tool := range centralAgentToolWindow(mutating, instanceOps) {
			if tool.Function == nil {
				continue
			}
			rows = append(rows, toolRow{Name: tool.Function.Name, Description: tool.Function.Description})
		}
		return rows
	}

	registered := workflow.RegisteredWorkflowActions()
	visibleSet := make(map[string]struct{})
	for _, tool := range centralAgentToolWindow(true, true) {
		if tool.Function == nil {
			continue
		}
		if operation, ok := proposalOperationForTool(tool.Function.Name); ok {
			visibleSet[operation] = struct{}{}
		}
	}
	var modelVisible, serverOnly []string
	for _, operation := range registered {
		if _, ok := visibleSet[operation]; ok {
			modelVisible = append(modelVisible, operation)
		} else {
			serverOnly = append(serverOnly, operation)
		}
	}
	sort.Strings(modelVisible)
	sort.Strings(serverOnly)

	payload := map[string]any{
		"tool_window_readonly":           collect(false, false),
		"tool_window_production":         collect(true, true),
		"model_visible_workflow_actions": modelVisible,
		"server_only_workflow_actions":   serverOnly,
		"registered_workflow_actions":    registered,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(out, b, 0o600))
	t.Logf("wrote %d bytes to %s", len(b), out)
}
