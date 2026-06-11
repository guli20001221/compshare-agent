package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderNetAcceleratorStatusReply_MissingRegionDoesNotLeakNil(t *testing.T) {
	reply := renderNetAcceleratorStatusReply(map[string]any{"Info": []any{
		map[string]any{"Optimized": false},
	}})

	assert.Contains(t, reply, "网络加速")
	assert.Contains(t, reply, "未开通")
	assert.NotContains(t, reply, "<nil>")
}
