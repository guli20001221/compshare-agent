package prompt

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

const (
	// 2026-07-15: the central Agent is the only semantic runtime. Per-intent
	// routing tables and temporary prompt cards were removed.
	// 2026-07-19: the action-call contract now states that a write is proposed only
	// when the user asks to actually change a resource — method/rules/fee/consequence/
	// hypothetical questions are answered, not executed (review round-2 finding on the
	// "怎么关机" ambiguity; behavioral effect validated in P7).
	// 2026-07-21: adopted the B4 central-agent behavior/reply prompt (the pro-carding
	// rewrite). It restates the SAME write-authorization guard — act on do-requests,
	// answer how-to/rules/fee/feasibility questions — with a proactive "适用就立即提交，
	// 由确认卡补齐参数" framing, validated behaviorally (pro+B4: reliably cards creates,
	// 0 false-cards on how-to probes). SHAs regenerated for the new segment text.
	// 2026-07-23: platform-specific rules and billing must be retrieved even when
	// the model considers them familiar; only stable general knowledge may bypass
	// SearchKnowledge.
	// 2026-07-23: uncertain tool observations may only become investigation items,
	// never a ranked cause or evidence that an unobserved layer is healthy.
	// SHAs regenerated for the merged segment text (A: retrieval discipline; B:
	// instance-access diagnosis) — both prompt changes are present in this build.
	// 2026-07-24: the knowledge-turn policy now names the search→read progression —
	// retrieval returns snippets, and a truncated / conclusion-only snippet must be
	// read in full (ReadChunk) before answering or denying, not guessed past. Pairs
	// with the new ReadChunk tool; SHAs regenerated for the added policy line.
	// 2026-07-24: the knowledge-turn policy now asks for a [[chunk_id]] marker on
	// evidence-backed sentences. The full validate → strip → trace machinery already
	// existed (knowledge.ValidateGroundedCitations / StripCiteMarkers /
	// emitSearchKnowledgeCitationTrace) but nothing ever ASKED the model to cite, so
	// only 41 of 1739 production RAG turns carried cited_chunk_ids and "did this
	// sentence come from the chunk it claims" was unmeasurable. Markers never reach
	// the user (stripped on both exits of finalizeAgentLoopKnowledgeAnswer, which is
	// entered whenever SearchKnowledge ran); the prompt says so, so the model does not
	// suppress them for readability. SHAs regenerated for the added policy line.
	// 2026-07-24 (REVERTED, kept as a warning): the retrieval-trigger enumeration
	// "平台收费、产品规则、功能支持和操作限制" was briefly replaced by a closed-form
	// criterion — "只在本平台成立、换一个平台就可能不同的具体事实" — on the argument that a
	// topic list can never be completed and grows with every production miss. It was
	// reverted after a live-40 measurement, and the reason generalizes:
	//
	// An ABSTRACT criterion asks the model to classify using knowledge it may not
	// have. "你的 coding plan 好贵" is only recognizable as platform-specific if you
	// already know Coding Plan is this platform's own product — the exact knowledge
	// retrieval was supposed to supply. Circular. Under the criterion that turn
	// stopped retrieving and asked whether the user meant "Codex/ChatGPT 的订阅",
	// misidentifying a first-party product; under the noun list it answered with the
	// real ¥99/¥199/¥499/¥799/¥999 tiers. A concrete noun ("收费") triggers on a
	// surface feature the model can always see, no prior knowledge required.
	//
	// So the enumeration is not merely a patch that survived — it is load-bearing for
	// a reason abstraction cannot replace. Caveat for whoever revisits this: N=1 per
	// arm, and per-case retrieval flips are noisy at this scale (fresh-001 flipped
	// between two runs with NO rule change). What the measurement establishes is the
	// absence of any benefit signal plus a mechanism for harm — not a proven delta.
	// 2026-07-28: current-data source selection is stated once in the knowledge
	// policy for every live platform fact (catalog, availability, state, price,
	// stock and popularity). The behavior segment keeps only the catalog decision
	// rule: model guesses cannot replace user criteria and supported expansions
	// may only add candidates. A failed live read cannot be replaced by a
	// knowledge-base candidate. Capability-specific parameter names stay in
	// tool schemas rather than the shared prompt.
	// 2026-07-30 (ATTEMPTED AND REVERTED, kept as the fourth and fifth warning):
	// two edits were made and backed out. (a) 机型规格（显存、最大卡数、可选 CPU/内存
	// 组合）added to the source-selection noun list, with its head noun changed
	// 实时事实 → 平台事实; (b) the scope segment made to NAME the platform's own paid
	// products (Coding Plan, 模型 API 套餐包). Neither changed behaviour, and the
	// reason they could not is worth more than the edits were:
	//
	// The trigger was 「4090 的显存和最大卡数是多少」 answering 「最多 10 张卡」 5/5 while
	// the live API returns 8 in all four zones (MachineSizes.Gpu=[1 2 4 8]). It was
	// diagnosed as a source-selection failure — the model never called
	// gpu_specs_query, which carries max_gpu_count=8 — hence a prompt rule naming
	// specs. It was not a source-selection failure. stage2b_w0 chunk
	// v2-resource_purchase-830490002fe09809 (source_origin official, confidence high)
	// carries a GPU table whose 4090 row says 10, and the model read it correctly:
	// it also got 4090_48G=8 right, which that same table says. The corpus is not
	// stale either — compshare-docs HEAD
	// (pages/gpus/instance/createcompshareinstance.md) still prints 10 today. The
	// conflict is between the docs and the API, upstream of anything in this repo.
	//
	// So the prompt was being asked to arbitrate a fact conflict it cannot see. With
	// the forced first hop ON the corpus answer always arrives before the model picks
	// a tool, and whichever source arrives first wins — measured: hop ON, 0/10 turns
	// called gpu_specs_query; hop OFF, 10/10 did and answered 8.
	//
	// Edit (b) failed its own test too: 「优云智算的 Coding Plan 套餐好贵」 searched 1/5
	// with the names in place, against a 0/5 baseline and a ±10pp A/A floor. An
	// earlier 3/3 on a different phrasing was noise, believed too early.
	//
	// Rule this leaves behind: before editing this prompt to make the model prefer a
	// source, check whether the two sources actually disagree. If they do, no wording
	// fixes it — the disagreement does.
	// 2026-07-31: a non-write catalog ID printed verbatim in recent complete
	// conversation may be carried into a later action as a candidate. It is not a
	// user decision or proof of current availability: the server point-queries it
	// and the confirmation gate remains. This narrow exception removes the
	// impossible "current turn only" contract without relaxing write targets.
	// 2026-08-03: ordinary model-visible tool observations gained one compact
	// control-plane contract. It makes the existing result envelope actionable
	// (including NO_CITABLE_EVIDENCE) without changing final deterministic replies.
	// 2026-08-03 (second): one exception added to that contract. A call the model
	// itself malformed resolves to correct_tool_call, not ask_user — the generic
	// needs_input rule would otherwise turn the model's own JSON mistake into a
	// question for a user who supplied nothing wrong.
	// 2026-08-06: UpdateTaskState and TaskSnapshot were deleted. The central
	// behavior segment must no longer tell the model to update a semantic-memory
	// tool that does not exist; the canonical transcript is the semantic history.
	mutatingReActPromptSHA256 = "5130902ad553d25db4de48f01488ba5a460e287978144561be90bbb337f2cd7d"
	readOnlyReActPromptSHA256 = "4ae223ec95dcde3c1f157a4ec9f1c8fe552c26201f1adc47efcc499ad54e6554"

	// 2026-07-30: the two SHAs above pin mutating and read-only with the SSH-ops repair lane OFF.
	// That leaves the rollout shape unpinned: deploy/conf/config.prod.yaml already sets
	// mutating_tools: true, so enabling ssh_ops there produces a fourth combination no snapshot
	// covered — and that is how it went unnoticed that no section named the lane in it at all (the
	// lane's only sentence lived inside the read-only boundary, which mutating mode skips). This
	// third SHA pins that combination. It includes the same shared 2026-07-31
	// catalog-candidate contract as the two snapshots above.
	mutatingWithRepairLaneReActPromptSHA256 = "f72be0d10cdaec54ca0644388787cc10a9ca679e3fc948817e70ec1defc323a4"
)

func TestReActPromptSnapshot_Mutating(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	assertPromptSHA256(t, "mutating", p, mutatingReActPromptSHA256)
}

func TestReActPromptSnapshot_ReadOnly(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false})
	assertPromptSHA256(t, "read_only", p, readOnlyReActPromptSHA256)
}

// The shape production renders: platform writes on AND the in-instance repair lane authorized.
func TestReActPromptSnapshot_MutatingWithRepairLane(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true, InstanceOpsWritesEnabled: true})
	assertPromptSHA256(t, "mutating_with_repair_lane", p, mutatingWithRepairLaneReActPromptSHA256)
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
