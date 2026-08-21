package prompt

import (
	"strings"
	"testing"
)

func TestReActPromptStaticPrefixStable(t *testing.T) {
	const marker = "## 用户当前状态"
	for _, mutating := range []bool{true, false} {
		opts := BuildOptions{MutatingToolsEnabled: mutating}
		ctxA := "当前会话已选实例：alpha（uhost-aaaaaaaa）"
		ctxB := "当前会话已选实例：beta（uhost-bbbbbbbb）\n\n当前账户只有 1 个实例：beta（uhost-bbbbbbbb），操作时可直接使用，无需追问。"
		a := BuildSystemWithOptions(ctxA, opts)
		b := BuildSystemWithOptions(ctxB, opts)

		ia, ib := strings.Index(a, marker), strings.Index(b, marker)
		if ia < 0 || ib < 0 {
			t.Fatalf("mutating=%v: %q marker missing from prompt", mutating, marker)
		}
		if a[ia:] != marker+"\n"+ctxA+"\n" || b[ib:] != marker+"\n"+ctxB+"\n" {
			t.Fatalf("mutating=%v: volatile user context is not the prompt's final section", mutating)
		}
		if a[:ia] != b[:ib] {
			t.Fatalf("mutating=%v: static prefix depends on user context", mutating)
		}
	}
}
