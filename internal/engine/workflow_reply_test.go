package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeterministicWorkflowReply pins WHY some mutating workflows skip the
// post-workflow LLM narration round: no-return-data lifecycle ops (reboot /
// stop / start / scheduler / rename) deliver a fixed, instant reply so the turn never
// stalls on the fast-tier thinking-mode narration call. Data-bearing or
// guidance-bearing workflows MUST keep narrating — short-circuiting them would
// hide the new password / DiskId / instance details / post-create steps the
// user needs. If this test breaks because a workflow moved buckets, that is a
// deliberate product decision, not an incidental refactor.
func TestDeterministicWorkflowReply(t *testing.T) {
	args := map[string]any{"UHostId": "uhost-abc123", "Name": "my-renamed-box"}

	// No-data lifecycle ops → deterministic short-circuit reply.
	for _, action := range []string{
		"RebootInstanceWorkflow",
		"StopInstanceWorkflow",
		"StartInstanceWorkflow",
		"SetStopSchedulerWorkflow",
		"CancelStopSchedulerWorkflow",
		"RenameInstanceWorkflow",
	} {
		reply, ok := deterministicWorkflowReply(action, args)
		require.Truef(t, ok, "%s must short-circuit narration", action)
		assert.Containsf(t, reply, "uhost-abc123", "%s reply must name the target instance", action)
	}

	// Rename must surface the new name so the user can confirm it landed.
	rename, _ := deterministicWorkflowReply("RenameInstanceWorkflow", args)
	assert.Contains(t, rename, "my-renamed-box", "rename reply must surface the new name")

	// Data/guidance-bearing workflows MUST still narrate (no short-circuit),
	// otherwise their returned IDs / passwords / guidance would be dropped.
	for _, action := range []string{
		"CreateInstanceWorkflow",
		"ResetPasswordWorkflow",
		"CreateDiskWorkflow",
		"ResizeDiskWorkflow",
		"CreateCustomImageWorkflow",
		"ResizeInstanceWorkflow",
		"ReinstallInstanceWorkflow",
	} {
		_, ok := deterministicWorkflowReply(action, args)
		assert.Falsef(t, ok, "%s must keep narration (carries return data/guidance)", action)
	}
}
