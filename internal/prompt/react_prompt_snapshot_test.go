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
	// tail so the static prefix is cacheable; also drop the now-wrong "上方"
	// (above) directional word from the real-time-query rule, since the block is
	// no longer above that rule.
	mutatingReActPromptSHA256 = "356348eb55afa2332e4836cf2d1b4aaf288169b5c48f778c14329c1546a410cd"
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
		// Position guard — the actual regression detector. Everything from the
		// marker to EOF must be EXACTLY "<marker>\n<userContext>\n": the volatile
		// block is the final block and no static text follows it. If someone moves
		// the block back into the middle, trailing static text appears after
		// userContext and this exact-tail equality fails. (The earlier
		// HasSuffix-only / prefix-identity checks could NOT catch that move: with
		// two different contexts injected at the same middle position, the static
		// prefix before the marker stays identical across A and B, so a[:ia]!=b[:ib]
		// never fires under the regression — only this tail check does.)
		if a[ia:] != marker+"\n"+ctxA+"\n" || b[ib:] != marker+"\n"+ctxB+"\n" {
			t.Fatalf("mutating=%v: the volatile %q block is not the final block — static text "+
				"follows userContext, so the KV-cache prefix would miss. It must stay at the tail.\n"+
				"tailA=%q", mutating, marker, a[ia:])
		}
		// Determinism complement: the static body before the volatile block does
		// not vary with the injected context. This cannot fail under the
		// middle-move regression above (both prefixes change together); it instead
		// catches a different bug — static text accidentally depending on
		// userContext (e.g. interpolating its length).
		if a[:ia] != b[:ib] {
			t.Fatalf("mutating=%v: static prefix before %q differs across turns — "+
				"some static content leaked a dependency on userContext.", mutating, marker)
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
