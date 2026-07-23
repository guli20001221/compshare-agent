package capability

import (
	"errors"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionableUpstreamErr is a typed upstream error that carries a user-facing
// recovery message — the shape *tools.UpstreamAPIError has on an actionable
// RetCode (e.g. 230 zone-unsupported). Declared locally so the test does not
// import the tools package.
type actionableUpstreamErr struct{ msg string }

func (actionableUpstreamErr) Error() string        { return "upstream failed" }
func (e actionableUpstreamErr) UserMessage() string { return e.msg }

// TestReadFailureAfterTool_SurfacesActionableUpstreamMessage locks the behavior
// the retired intent failure_after_tool_error_test covered, now on the live typed
// path: a typed upstream error with a non-empty UserMessage surfaces that message
// verbatim as an actionable-upstream failure (so an actionable recovery hint
// reaches the user instead of the generic "查询暂时失败").
func TestReadFailureAfterTool_SurfacesActionableUpstreamMessage(t *testing.T) {
	got := ReadFailureAfterTool("GetCompShareInstanceUserPrice", "pricing_query",
		actionableUpstreamErr{msg: "该可用区不支持该机型，请换一个可用区重试。"})

	require.Equal(t, platform.ReadStatusFailureAfterTool, got.Status)
	require.Equal(t, platform.ReadFailureActionableUpstream, got.FailureClass)
	assert.Equal(t, "该可用区不支持该机型，请换一个可用区重试。", got.Reply)
	assert.Equal(t, "GetCompShareInstanceUserPrice", got.ToolAction)
}

// TestReadFailureAfterTool_EmptyUserMessageFallsBackToGeneric: a typed error
// whose UserMessage is blank must NOT surface an empty reply — it falls through
// to the label-prefixed generic friendly reply, classed generic (not actionable).
func TestReadFailureAfterTool_EmptyUserMessageFallsBackToGeneric(t *testing.T) {
	got := ReadFailureAfterTool("GetCompShareInstanceUserPrice", "pricing_query",
		actionableUpstreamErr{msg: "   "})

	require.Equal(t, platform.ReadFailureGenericRead, got.FailureClass)
	assert.Equal(t, "pricing_query: "+FriendlyReadFailureReply, got.Reply)
}

// TestReadFailureAfterTool_PlainErrorIsGeneric: a plain (non-user-facing) error
// is a generic read failure with the label-prefixed friendly reply.
func TestReadFailureAfterTool_PlainErrorIsGeneric(t *testing.T) {
	got := ReadFailureAfterTool("GetCompShareInstanceUserPrice", "pricing_query", errors.New("boom"))

	require.Equal(t, platform.ReadFailureGenericRead, got.FailureClass)
	assert.Equal(t, "pricing_query: "+FriendlyReadFailureReply, got.Reply)
}
