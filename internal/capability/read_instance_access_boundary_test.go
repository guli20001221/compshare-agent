package capability

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/diagnosis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheCloudSideBoundaryIsStatedOnce guards the caveat against having two
// producers. The diagnosis chain's conclusion carried it AND the renderer appends
// it, so a live SSH precheck answered with both sentences in a single paragraph —
// the user reads the same limitation twice and the actual verdict shrinks.
//
// The renderer's copy is the load-bearing one: it is unconditional, so it also
// covers access types and outcomes the chain never produces. This asserts the
// invariant (stated once) rather than a literal, so rewording either sentence
// does not silently reintroduce the pair.
func TestTheCloudSideBoundaryIsStatedOnce(t *testing.T) {
	reply := instanceAccessRender(InstanceAccessResponse{
		InstanceID:   "uhost-x",
		InstanceName: "host",
		AccessType:   accessTypeSSH,
		Status:       "configured",
		Reason:       diagnosis.SSHFailureChain().Fallback.Conclusion,
	}).Reply

	require.NotEmpty(t, reply)
	// "没有实际连接" / "未实际探测" — the two spellings of the same boundary claim.
	occurrences := strings.Count(reply, "公网端口")
	assert.Equal(t, 1, occurrences,
		"the cloud-side-only boundary must be stated once, got %d times in: %s", occurrences, reply)
	assert.Contains(t, reply, "该结果没有实际连接公网端口",
		"the renderer's wider wording is the one that survives")
	assert.Contains(t, reply, "云侧预检未发现明确阻断",
		"removing the duplicate must not remove the verdict itself")
}
