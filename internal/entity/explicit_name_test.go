package entity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTextExplicitlyMentionsName(t *testing.T) {
	tests := []struct {
		text, name string
		want       bool
	}{
		{text: "pytest", name: "test", want: false},
		{text: "ghost", name: "host", want: false},
		{text: "data", name: "a", want: false},
		{text: "my-test", name: "test", want: false},
		{text: "test", name: "test", want: true},
		{text: "请检查 test，可能坏了", name: "test", want: true},
		{text: "test机器怎么了", name: "test", want: true},
		{text: "机器坏了", name: "机", want: false},
		{text: "测试环境异常", name: "测试", want: false},
		{text: "请测试一下", name: "测试", want: false},
		{text: "请检查「测试」，可能坏了", name: "测试", want: true},
		{text: "备用机C 上的 GPU 掉卡了", name: "备用机C", want: true},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, TextExplicitlyMentionsName(tc.text, tc.name), "%q / %q", tc.text, tc.name)
	}
}
