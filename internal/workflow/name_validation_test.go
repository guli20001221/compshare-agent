package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidatedCompShareResourceName_MatchesUpstreamStringCheck(t *testing.T) {
	for _, valid := range []string{"实例-01", "AZaz09_,.:-", "一", "龥"} {
		got, err := validatedCompShareResourceName(valid, "名称", 63)
		assert.NoError(t, err, valid)
		assert.Equal(t, valid, got)
	}
	for _, invalid := range []string{"has space", " surrounded ", "path/name", "emoji😀", "龦", "line\nbreak"} {
		_, err := validatedCompShareResourceName(invalid, "名称", 63)
		assert.Error(t, err, invalid)
	}
	_, err := validatedCompShareResourceName(strings.Repeat("a", 64), "名称", 63)
	assert.Error(t, err)
}
