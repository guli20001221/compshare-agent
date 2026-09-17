package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeQueryFoldsCaseWidthPunctuationAndSpacing(t *testing.T) {
	assert.Equal(t, "https 地址", NormalizeQuery("  ＨＴＴＰＳ 地址？ "))
	assert.Equal(t, "comfyui 导入工作流", NormalizeQuery("ComfyUI  导入工作流！！"))
	assert.Equal(t, "错误码 226604 资源不足", NormalizeQuery("错误码 226604\t资源不足"))
	assert.Equal(t, "", NormalizeQuery("…—?!"))
}
