package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlashKnowledgeRouteGuardDefaultOff(t *testing.T) {
	SetFlashKnowledgeRouteGuardEnabled(false)
	t.Cleanup(func() { SetFlashKnowledgeRouteGuardEnabled(false) })

	require.False(t, FlashKnowledgeRouteGuardEnabled())
}

func TestFlashKnowledgeRouteGuardMatch(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "disk billing", text: "磁盘空间是如何收费的？100GB 原始空间免费吗", want: true},
		{name: "coding plan cancel refund", text: "取消 Coding Plan 套餐能退款吗", want: true},
		{name: "coding plan delete paraphrase", text: "把 Coding Plan 套餐退了", want: true},
		{name: "generic shortage", text: "一直暂无资源 是什么情况", want: true},
		{name: "sold out semantics", text: "SoldOut 是售罄还是下架", want: true},
		{name: "normal semantics", text: "Normal 状态是不是说明一定有库存", want: true},
		{name: "named gpu stock stays live", text: "5090一直暂无资源是什么情况", want: false},
		{name: "named gpu availability stays live", text: "4090 有没有货", want: false},
		{name: "instance lifecycle stays workflow", text: "帮我重启这台实例", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := matchFlashKnowledgeRouteGuard(tt.text)
			require.Equal(t, tt.want, got)
		})
	}
}
