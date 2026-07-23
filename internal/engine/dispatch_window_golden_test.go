package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The agent's tool window is the only thing the model can act through, so any
// change to it is a change to what the product can do. The existing window
// tests assert membership (Contains / NotContains), which cannot see a tool
// silently appearing, disappearing or reordering.
//
// These two goldens are captured on unmodified product code so that a later
// change which is supposed to be inert — one that adds a tool behind an
// off-by-default dependency — has something it can actually be proved against.
// A golden written after such a change is written to match it and proves
// nothing.
//
// If a change to the window is intended, update the expected slice in the SAME
// commit that changes behavior, and say in the commit message which tool moved
// and why. Do not update it to make an unrelated refactor pass.

var goldenWindowReadOnly = []string{
	"UpdateTaskState",
	"ReadCapability_account_finance_status",
	"ReadCapability_cfs_create_price",
	"ReadCapability_cfs_list",
	"ReadCapability_cfs_refund_estimate",
	"ReadCapability_cfs_upgrade_price",
	"ReadCapability_gpu_specs_query",
	"ReadCapability_image_list",
	"ReadCapability_image_tag_catalog",
	"ReadCapability_model_repository_browse",
	"ReadCapability_monitor_history",
	"ReadCapability_monitor_query",
	"ReadCapability_network_accelerator_status",
	"ReadCapability_pricing_query",
	"ReadCapability_refund_estimate",
	"ReadCapability_resource_info",
	"ReadCapability_stock_availability",
	"ReadCapability_zone_catalog",
	"SearchKnowledge",
	"DiagnoseSSH",
	"DiagnoseBilling",
}

var goldenWindowMutating = []string{
	"UpdateTaskState",
	"RequestCreateInstance",
	"RequestStopInstance",
	"RequestStartInstance",
	"RequestRebootInstance",
	"RequestRenameInstance",
	"RequestResetPassword",
	"RequestSetStopScheduler",
	"RequestCancelStopScheduler",
	"RequestResizeInstance",
	"RequestResizeDisk",
	"RequestReinstallInstance",
	"RequestCreateDisk",
	"RequestCreateCustomImage",
	"RequestCloneCustomImage",
	"RequestEnableNetOptimizer",
	"RequestCreateCFS",
	"RequestResizeCFS",
	"ReadCapability_account_finance_status",
	"ReadCapability_cfs_create_price",
	"ReadCapability_cfs_list",
	"ReadCapability_cfs_refund_estimate",
	"ReadCapability_cfs_upgrade_price",
	"ReadCapability_gpu_specs_query",
	"ReadCapability_image_list",
	"ReadCapability_image_tag_catalog",
	"ReadCapability_model_repository_browse",
	"ReadCapability_monitor_history",
	"ReadCapability_monitor_query",
	"ReadCapability_network_accelerator_status",
	"ReadCapability_pricing_query",
	"ReadCapability_refund_estimate",
	"ReadCapability_resource_info",
	"ReadCapability_stock_availability",
	"ReadCapability_zone_catalog",
	"SearchKnowledge",
	"DiagnoseSSH",
	"DiagnoseBilling",
}

func TestCentralAgentToolWindowGolden(t *testing.T) {
	require.Equal(t, goldenWindowReadOnly, centralAgentToolNames(false, false),
		"read-only tool window drifted")
	require.Equal(t, goldenWindowMutating, centralAgentToolNames(true, false),
		"mutating tool window drifted")
}

// The window is built per round from the registry, so a tool that is present in
// the name list is present in the actual request. Pinning the count separately
// makes an accidental duplicate visible as a count mismatch rather than as a
// slice diff buried in 37 lines.
func TestCentralAgentToolWindowGoldenCounts(t *testing.T) {
	require.Len(t, centralAgentToolNames(false, false), 21)
	require.Len(t, centralAgentToolNames(true, false), 38)
}
