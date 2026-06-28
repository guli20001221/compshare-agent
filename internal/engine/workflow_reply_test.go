package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeterministicWorkflowReply pins WHY some mutating workflows skip the
// post-workflow LLM narration round: no-return-data lifecycle ops (reboot /
// stop / start / rename) deliver a fixed, instant reply so the turn never
// stalls on the fast-tier thinking-mode narration call. Password-bearing
// workflows also use fixed replies so the model cannot echo secrets. Other
// data/guidance-bearing workflows MUST keep narrating — short-circuiting them
// would hide DiskId / instance details / post-create steps the user needs. If
// this test breaks because a workflow moved buckets, that is a deliberate
// product decision, not an incidental refactor.
func TestDeterministicWorkflowReply(t *testing.T) {
	args := map[string]any{"UHostId": "uhost-abc123", "Name": "my-renamed-box"}

	// No-data lifecycle ops → deterministic short-circuit reply.
	for _, action := range []string{
		"RebootInstanceWorkflow",
		"StopInstanceWorkflow",
		"StartInstanceWorkflow",
		"RenameInstanceWorkflow",
	} {
		reply, ok := deterministicWorkflowReply(action, args)
		require.Truef(t, ok, "%s must short-circuit narration", action)
		assert.Containsf(t, reply, "uhost-abc123", "%s reply must name the target instance", action)
	}

	// Rename must surface the new name so the user can confirm it landed.
	rename, _ := deterministicWorkflowReply("RenameInstanceWorkflow", args)
	assert.Contains(t, rename, "my-renamed-box", "rename reply must surface the new name")

	reset, ok := deterministicWorkflowReply("ResetPasswordWorkflow", map[string]any{
		"UHostId":  "uhost-abc123",
		"Password": "Secret123!",
	})
	require.True(t, ok, "password reset must use deterministic narration")
	assert.Contains(t, reset, "uhost-abc123")
	assert.NotContains(t, reset, "Secret123!", "password reset replies must never echo the password")
	assert.Contains(t, reset, "不会在对话中回显")

	reinstall, ok := deterministicWorkflowReply("ReinstallInstanceWorkflow", map[string]any{
		"UHostId":  "uhost-abc123",
		"Password": "Secret123!",
	})
	require.True(t, ok, "reinstall must use deterministic narration")
	assert.Contains(t, reinstall, "uhost-abc123")
	assert.NotContains(t, reinstall, "Secret123!", "reinstall replies must never echo the password")
	assert.Contains(t, reinstall, "不会在对话中回显")

	// Data/guidance-bearing workflows MUST still narrate (no short-circuit),
	// otherwise their returned IDs / guidance would be dropped.
	for _, action := range []string{
		"CreateInstanceWorkflow",
		"CreateDiskWorkflow",
		"ResizeDiskWorkflow",
		"CreateCustomImageWorkflow",
		"ResizeInstanceWorkflow",
	} {
		_, ok := deterministicWorkflowReply(action, args)
		assert.Falsef(t, ok, "%s must keep narration (carries return data/guidance)", action)
	}
}

func TestWorkflowSecretValuesIncludesEncodedPassword(t *testing.T) {
	secrets := workflowSecretValues(map[string]any{"Password": "SecurePass123"})

	assert.Contains(t, secrets, "SecurePass123")
	assert.Contains(t, secrets, "U2VjdXJlUGFzczEyMw==")
}
