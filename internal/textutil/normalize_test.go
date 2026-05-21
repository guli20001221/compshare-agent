package textutil

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain_ascii", "hello", "hello"},
		{"upper_to_lower", "HELLO", "hello"},
		{"mixed_case", "HeLLo World", "hello world"},
		{"leading_trailing_ws", "  hi  ", "hi"},
		{"collapse_internal_ws", "hi   there", "hi there"},
		{"tabs_and_newlines", "a\tb\nc", "a b c"},
		{"cjk_preserved", "你好世界", "你好世界"},
		{"cjk_with_ascii", "我有 4090 GPU", "我有 4090 gpu"},
		{"cjk_with_internal_ws", "查 一下", "查 一下"},
		{"trailing_full_width_unchanged", "余额不足。", "余额不足。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
