package prompt

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

const (
	// 2026-06-05: workflow and diagnosis catalogs are generated from registries.
	// 2026-06-05 (remediation): operation boundary / state-refresh / no-pretext /
	// vague-failure rules now single-sourced from cards.go (shared with the
	// flag-on cards), so the rule text lives in one place. SHA updated to match.
	// 2026-06-18: narrow deploy/create boundary to hardware-first creation only.
	// 2026-06-20: remove duplicate CreateInstanceWorkflow routing sentence from the shared card.
	// 2026-06-22 (阶段1A KV-cache): move the volatile "## 用户当前状态" block to the
	// tail so the static prefix is cacheable — body bytes unchanged, only position.
	mutatingReActPromptSHA256 = "5f1e4d7ac919a2069e3f38cfed61209097d945899ed96d85216062ba6c86a17b"
	// 2026-06-05: read-only diagnosis catalog is generated from the diagnosis registry.
	// 2026-06-22 (阶段1A KV-cache): volatile userContext block moved to the tail.
	readOnlyReActPromptSHA256 = "77fb47bb7ef45d6d73e0087574d4e2b1ba5bd9ea50b0cdc0c532330b89f5068f"
)

func TestReActPromptSnapshot_Mutating(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	assertPromptSHA256(t, "mutating", p, mutatingReActPromptSHA256)
}

func TestReActPromptSnapshot_ReadOnly(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false})
	assertPromptSHA256(t, "read_only", p, readOnlyReActPromptSHA256)
}

// TestReActPromptStaticPrefixStable is the KV-cache prefix-stability guard
// (P0 阶段1A). The volatile per-turn "## 用户当前状态" block must live at the TAIL
// so the static body before it is byte-identical across turns — that is the
// precondition for the provider's automatic prefix cache to hit. This test fails
// the moment someone moves the volatile block back into the middle of the prompt
// (which is exactly the regression that defeated caching before this change).
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
		if ia != ib || a[:ia] != b[:ib] {
			t.Fatalf("mutating=%v: static prefix is NOT byte-identical across turns — KV-cache prefix would miss. "+
				"The volatile %q block must stay at the tail.\nprefixA len=%d prefixB len=%d", mutating, marker, ia, ib)
		}
		// The marker must be the start of the FINAL block: only userContext follows it.
		if !strings.HasSuffix(a, ctxA+"\n") {
			t.Fatalf("mutating=%v: userContext is not at the very tail of the prompt", mutating)
		}
	}
}

func assertPromptSHA256(t *testing.T, name, prompt, want string) {
	t.Helper()
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
	if got != want {
		t.Fatalf("%s ReAct prompt SHA drifted\n got: %s\nwant: %s\nlength: %d", name, got, want, len([]byte(prompt)))
	}
}
